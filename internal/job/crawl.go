package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuhang113/urag/internal/acquire"
	"github.com/xuhang113/urag/internal/catalog"
	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/clean"
	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
)

// CrawlInput 是一次公开收录。
// Seeds 是站点地图或目录页 URL。Client 为空时使用默认客户端；测试传入 httptest 的客户端。
// Limits.PerHostInterval 为 0 时不额外等待。真实站点应传 1 秒。
type CrawlInput struct {
	CollectionID model.CollectionID
	Seeds        []string
	Client       *http.Client
	Limits       acquire.CrawlLimits
}

// SubmitCrawl 从来源 URL 收录。正文通过 Found.Open 流式进入清洗，不写入对象存储。
// 同一集合的同一个 source URI 只占台账一行。
// Inline 为 true 时，返回前任务已经结束。
// 失败最多尝试 3 次，次数含第一次。取消后状态为 canceled，不再重试。
func (p *Pipeline) SubmitCrawl(ctx context.Context, in CrawlInput) (Job, error) {
	if p == nil || p.deps.Catalog == nil || p.deps.Vectors == nil || p.deps.Embedder == nil {
		return Job{}, errMissingDeps
	}
	if in.CollectionID == "" {
		return Job{}, errors.New("job: collection required")
	}
	if len(in.Seeds) == 0 {
		return Job{}, errors.New("job: crawl seeds required")
	}
	copied := in
	copied.Seeds = append([]string(nil), in.Seeds...)
	rec := p.newRecord(KindCrawl, in.CollectionID)
	rec.crawl = &copied
	rec.Job.State = StateQueued
	p.mu.Lock()
	p.jobs[rec.Job.ID] = rec
	p.mu.Unlock()
	err := p.start(ctx, rec)
	return p.snapshot(rec), err
}

func (p *Pipeline) executeCrawl(ctx context.Context, rec *jobRecord) error {
	if rec.crawl == nil {
		return errors.New("job: crawl input missing")
	}
	in := rec.crawl
	if err := p.restoreCrawlCheckpoint(rec); err != nil {
		return err
	}
	// 每次尝试使用新的去重器。上一轮失败留下的哈希不会把同一篇标成重复。
	cleaner := clean.New(clean.NewMemoryDeduper())
	crawler := acquire.NewCrawler(in.Client, in.Limits).WithDone(p.sourceURIs(rec))
	found, skips, err := crawler.Crawl(ctx, in.Seeds)
	if err != nil {
		return err
	}
	total := len(found) + len(skips)
	done := 0
	for _, sk := range skips {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.markSource(rec, sk.URI); err != nil {
			return err
		}
		done++
		p.setProgress(rec, "crawl", done, total)
	}
	logSkipCounts(skips)
	cleanPDF := 0
	for _, item := range found {
		if err := ctx.Err(); err != nil {
			return err
		}
		if p.hasSource(rec, item.SourceURI) {
			done++
			p.setProgress(rec, "crawl", done, total)
			continue
		}
		err := p.ingestFound(ctx, rec, cleaner, item)
		if errors.Is(err, errCleanSkip) {
			cleanPDF++
			done++
			p.setProgress(rec, "crawl", done, total)
			continue
		}
		if err != nil {
			return err
		}
		done++
		p.setProgress(rec, "crawl", done, total)
	}
	fmt.Fprintf(os.Stderr, "清洗跳过 pdf=%d\n", cleanPDF)
	return nil
}

// errCleanSkip 表示这篇已写入断点，但不计入合格篇数。调用方不要重试。
var errCleanSkip = errors.New("job: clean skip")

// ReleaseJob 丢掉已经结束的任务记录。下一批会从磁盘断点继续，不在内存里留下一份断点副本。
func (p *Pipeline) ReleaseJob(id string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	delete(p.jobs, id)
	p.mu.Unlock()
}

func logSkipCounts(skips []acquire.Skip) {
	n := map[string]int{}
	for _, sk := range skips {
		n[sk.Reason]++
	}
	fmt.Fprintf(os.Stderr, "跳过 robots=%d login=%d payment=%d captcha=%d unsupported=%d download=%d\n",
		n["robots"], n["login"], n["payment"], n["captcha"], n["unsupported"], n["download"])
}

func (p *Pipeline) ingestFound(ctx context.Context, rec *jobRecord, cleaner clean.Cleaner, item acquire.Found) error {
	if item.Open == nil {
		return p.markSource(rec, item.SourceURI)
	}
	rc, err := item.Open(ctx)
	if err != nil {
		if canceled(ctx, err) {
			return err
		}
		return p.markSource(rec, item.SourceURI)
	}
	defer rc.Close()

	doc, err := cleaner.Clean(ctx, clean.Input{
		CollectionID: model.CollectionID(rec.Job.CollectionID),
		SourceURI:    item.SourceURI,
		Filename:     filenameOf(item.SourceURI),
		Body:         rc,
	})
	if err != nil {
		if canceled(ctx, err) {
			return err
		}
		if errors.Is(err, clean.ErrUnsupportedType) {
			return p.markSource(rec, item.SourceURI)
		}
		// 抽不出文字的 PDF 记入断点后继续。否则一份扫描件会让整批停住。
		if strings.HasPrefix(err.Error(), "clean: pdf:") {
			if markErr := p.markSource(rec, item.SourceURI); markErr != nil {
				return markErr
			}
			return errCleanSkip
		}
		return err
	}
	if doc.Duplicate {
		return p.markSource(rec, item.SourceURI)
	}
	sections, err := chunk.SplitSections(doc)
	if err != nil {
		return err
	}
	groups := sectionGroups(sections)
	if len(groups) == 0 {
		return p.markSource(rec, item.SourceURI)
	}

	now := time.Now().UTC()
	docID, err := p.deps.Catalog.PutDocument(ctx, catalog.Document{
		CollectionID: rec.Job.CollectionID,
		Version:      1,
		SourceURI:    item.SourceURI,
		Checksum:     doc.Lineage.ContentHash,
		MediaType:    doc.Lineage.MediaType,
		License:      item.License,
		Title:        item.Title,
		FetchedAt:    now,
	})
	if err != nil {
		return err
	}
	for i := range groups {
		groups[i].Parent.DocID = docID
		for j := range groups[i].Children {
			groups[i].Children[j].DocID = docID
		}
	}
	if err := p.deps.Catalog.ReplaceSections(ctx, docID, groups); err != nil {
		return err
	}
	kids, err := p.deps.Catalog.ListChildren(ctx, docID)
	if err != nil {
		return err
	}
	if len(kids) == 0 {
		return errors.New("job: catalog stored no children")
	}
	if err := p.writeVectors(ctx, model.CollectionID(rec.Job.CollectionID), docID, kids); err != nil {
		return err
	}
	return p.markSource(rec, item.SourceURI)
}

func (p *Pipeline) writeVectors(ctx context.Context, col model.CollectionID, docID int64, kids []catalog.Child) error {
	batch := forwardBatch(p.cfg.Limits.EmbedBatch)
	for start := 0; start < len(kids); start += batch {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := start + batch
		if end > len(kids) {
			end = len(kids)
		}
		part := kids[start:end]
		texts := make([]string, len(part))
		for i, child := range part {
			texts[i] = chunk.Passage(child.HeadingPath, child.Body)
		}
		vecs, err := p.deps.Embedder.Embed(ctx, texts)
		if err != nil {
			return err
		}
		if len(vecs) != len(texts) {
			return fmt.Errorf("job: embedder returned %d vectors for %d texts", len(vecs), len(texts))
		}
		recs := make([]index.VectorRecord, len(part))
		for i, child := range part {
			recs[i] = index.VectorRecord{
				ChunkID:      child.ChunkID,
				ParentID:     child.ParentID,
				DocID:        docID,
				Version:      1,
				CollectionID: col,
				HeadingPath:  child.HeadingPath,
				Page:         child.Page,
				Body:         child.Body,
				Vector:       vecs[i],
			}
		}
		if err := p.deps.Vectors.UpsertRecords(ctx, recs); err != nil {
			return err
		}
	}
	return nil
}

func forwardBatch(n int) int {
	if n <= 0 {
		n = DefaultEmbedBatch
	}
	if n > embed.MaxForwardBatch {
		n = embed.MaxForwardBatch
	}
	return n
}

func sectionGroups(sections []chunk.Section) []catalog.ParentWithChildren {
	groups := make([]catalog.ParentWithChildren, 0, len(sections))
	for _, sec := range sections {
		if len(sec.Children) == 0 {
			continue
		}
		children := make([]catalog.Child, len(sec.Children))
		for i, ch := range sec.Children {
			page := sec.PageFrom
			if sec.PageFrom != sec.PageTo {
				page = 0
			}
			children[i] = catalog.Child{
				Ordinal:     ch.Ordinal,
				StartByte:   ch.StartByte,
				EndByte:     ch.EndByte,
				HeadingPath: sec.HeadingPath,
				Page:        page,
				Body:        ch.Text,
			}
		}
		groups = append(groups, catalog.ParentWithChildren{
			Parent: catalog.Parent{
				HeadingPath: sec.HeadingPath,
				PageFrom:    sec.PageFrom,
				PageTo:      sec.PageTo,
				Body:        sec.Body,
			},
			Children: children,
		})
	}
	return groups
}

func canceled(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
}

func (p *Pipeline) sourceURIs(rec *jobRecord) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var cp objectCheckpoint
	if len(rec.Job.Checkpoint) > 0 {
		if err := json.Unmarshal(rec.Job.Checkpoint, &cp); err != nil {
			return nil
		}
	}
	return append([]string(nil), cp.SourceURIs...)
}

func (p *Pipeline) hasSource(rec *jobRecord, uri string) bool {
	for _, item := range p.sourceURIs(rec) {
		if item == uri {
			return true
		}
	}
	return false
}

func (p *Pipeline) markSource(rec *jobRecord, uri string) error {
	if uri == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var cp objectCheckpoint
	if len(rec.Job.Checkpoint) > 0 {
		if err := json.Unmarshal(rec.Job.Checkpoint, &cp); err != nil {
			return err
		}
	}
	for _, item := range cp.SourceURIs {
		if item == uri {
			return nil
		}
	}
	cp.SourceURIs = append(cp.SourceURIs, uri)
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	rec.Job.Checkpoint = raw
	return p.writeCrawlCheckpoint(raw)
}

// restoreCrawlCheckpoint 把磁盘上的已完成来源并进本次任务。
// 没有配置路径，或文件还不存在时，保持内存里的断点。
func (p *Pipeline) restoreCrawlCheckpoint(rec *jobRecord) error {
	path := p.cfg.CrawlCheckpoint
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("job: read crawl checkpoint: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var disk objectCheckpoint
	if err := json.Unmarshal(raw, &disk); err != nil {
		return fmt.Errorf("job: read crawl checkpoint: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var mem objectCheckpoint
	if len(rec.Job.Checkpoint) > 0 {
		if err := json.Unmarshal(rec.Job.Checkpoint, &mem); err != nil {
			return err
		}
	}
	mem.ObjectKeys = unionStrings(mem.ObjectKeys, disk.ObjectKeys)
	mem.SourceURIs = unionStrings(mem.SourceURIs, disk.SourceURIs)
	merged, err := json.Marshal(mem)
	if err != nil {
		return err
	}
	rec.Job.Checkpoint = merged
	return nil
}

func (p *Pipeline) writeCrawlCheckpoint(raw []byte) error {
	path := p.cfg.CrawlCheckpoint
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("job: write crawl checkpoint: %w", err)
	}
	f, err := os.CreateTemp(dir, ".crawl-checkpoint-*")
	if err != nil {
		return fmt.Errorf("job: write crawl checkpoint: %w", err)
	}
	tmp := f.Name()
	done := false
	defer func() {
		if !done {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(append(append([]byte{}, raw...), '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("job: write crawl checkpoint: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("job: write crawl checkpoint: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("job: write crawl checkpoint: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("job: write crawl checkpoint: %w", err)
	}
	done = true
	return nil
}

func unionStrings(lists ...[]string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, list := range lists {
		for _, item := range list {
			if item == "" {
				continue
			}
			if _, ok := seen[item]; ok {
				continue
			}
			seen[item] = struct{}{}
			out = append(out, item)
		}
	}
	return out
}
