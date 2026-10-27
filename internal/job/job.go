// Package job 编排上传、抓取、收录、清洗、嵌入、检索和评价任务。
// 接受上传时只把正文流式写入存储并入队。清洗和嵌入在任务执行时进行。
// 收录任务不把原文写入对象存储，只把来源 URL 记入台账。
// 本包不提供 HTTP 页面。
//
// 校验和不符不重试。任务取消后不再重试。其他失败最多尝试 3 次，次数含第一次。
// Checkpoint 记下已完成的对象键；收录任务另外用 source_uris 记下已经处理完的来源 URL。
package job

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/xuhang113/urag/internal/acquire"
	"github.com/xuhang113/urag/internal/catalog"
	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/clean"
	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
	"github.com/xuhang113/urag/internal/storage"
)

const (
	// StateQueued 表示任务已入队，尚未运行。
	StateQueued = "queued"
	// StateRunning 表示任务正在运行。
	StateRunning = "running"
	// StateSucceeded 表示任务已经成功。
	StateSucceeded = "succeeded"
	// StateFailed 表示任务失败，且不会再自动重试。
	StateFailed = "failed"
	// StateCanceled 表示任务已取消，之后不再重试。
	StateCanceled = "canceled"

	// KindUpload 是上传任务。
	KindUpload = "upload"
	// KindFetch 是抓取任务。
	KindFetch = "fetch"
	// KindCrawl 是公开收录任务。原文留在来源 URL，不写入对象存储。
	KindCrawl = "crawl"
	// KindEval 是评价任务。
	KindEval = "eval"

	defaultFixturePath = "testdata/recall/fixture.json"
	defaultAttempts    = 3
)

var (
	// ErrNotImplemented 表示调用方要求的能力尚未接通。
	ErrNotImplemented = errors.New("job: not implemented")

	// ErrNotFound 表示任务不存在。
	ErrNotFound = errors.New("job: not found")

	errMissingDeps = errors.New("job: missing deps")
)

// Progress 是任务进度。Done 不超过 Total。Stage 是当前阶段名。
type Progress struct {
	Done  int    `json:"done"`
	Total int    `json:"total"`
	Stage string `json:"stage"`
}

// Job 是一次异步任务的公开状态。
// State 只取 queued、running、succeeded、failed、canceled。
// Kind 只取 upload、fetch、eval。
// 校验和不符不增加后续重试。取消后 State 为 canceled，且不再重试。
// Checkpoint 记下已完成的对象键。收录任务的 source_uris 记下已经处理完的来源 URL。
type Job struct {
	ID           string          `json:"job_id"`
	Kind         string          `json:"kind"`
	State        string          `json:"state"`
	CollectionID string          `json:"collection_id"`
	Progress     Progress        `json:"progress"`
	ObjectKey    string          `json:"object_key,omitempty"`
	Checksum     string          `json:"checksum,omitempty"`
	Size         int64           `json:"size,omitempty"`
	Error        string          `json:"error,omitempty"`
	Attempts     int             `json:"attempts"`
	Checkpoint   json.RawMessage `json:"checkpoint,omitempty"`
}

// UploadInput 是一次上传。Body 只能流式读取，禁止先读进内存。
type UploadInput struct {
	CollectionID   model.CollectionID
	IdempotencyKey model.IdempotencyKey
	Filename       string
	ContentType    string
	Body           io.Reader
}

// Accepted 是上传入队后立刻返回给调用方的结果。
// 成功时带有任务、对象键、校验和和大小。清洗与嵌入在任务里执行。
// Inline 为 true 时，返回前任务已经跑完。
type Accepted struct {
	Job       Job
	ObjectKey model.ObjectKey
	Checksum  model.Checksum
	Size      int64
}

// FetchInput 是一次抓取请求。Kind 取 acquire 的来源种类。
type FetchInput struct {
	CollectionID      model.CollectionID `json:"collection_id"`
	Kind              string             `json:"kind"`
	URI               string             `json:"uri"`
	IdempotencyPrefix string             `json:"idempotency_prefix"`
}

// Limits 是各阶段上限。
// Upload 是单次上传的字节上限。Clean 是清洗并发上限。
// EmbedBatch 与 EmbedConcurrency 是嵌入的批大小和并发。
type Limits struct {
	Upload           int64
	Clean            int
	EmbedBatch       int
	EmbedConcurrency int
}

// DefaultEmbedBatch 是未设置 URAG_EMBED_BATCH_SIZE 时的条数。
// 比逐批重载模型时用的 16 大，仍不超过 embed.MaxForwardBatch，避免把整库放进内存。
const DefaultEmbedBatch = 64

// Config 是进程组装参数。
// Storage 只取 local 或 s3。FixturePath 是召回夹具路径。
// Inline 为 true 时，清洗和建索引在入队方法返回前完成，成功状态为 succeeded。
// UploadConcurrency 小于等于 0 时不限制同时运行的任务数。
// CrawlCheckpoint 非空时，收录把已完成的 source URI 写进这个文件。
// 下次执行先读文件，跳过已经处理完的来源。空字符串只把断点留在任务内存里。
type Config struct {
	Addr              string
	DataDir           string
	Storage           string
	Retry             storage.RetryPolicy
	Weights           retrieve.Weights
	Chunk             chunk.Options
	Limits            Limits
	FixturePath       string
	Inline            bool
	UploadConcurrency int
	CrawlCheckpoint   string
	// RerankMinScore 是重排分数门槛。
	// 试查只有真正拿到重排分时才用它。没有重排分时按混合分返回，不套这道门槛。
	// urag search 先重排再套它。生产进程从 URAG_RERANK_MIN_SCORE 读入，未设置时用 retrieve.DefaultRerankMinScore。
	// 夹具评价走 Ask，不读这个字段。
	RerankMinScore float64
	// KeepEmbed 为 true 时，试查不在每次提问后关掉嵌入进程。
	// urag try 在进程活着期间只加载一次嵌入模型。
	KeepEmbed bool
	// ParkEmbed 为 true 时，内存放不下嵌入和重排两个模型。
	// 第一次需要重排前写一次取舍，之后的提问不再写。
	// 这次没有重排分，试查按混合分返回。
	ParkEmbed bool
}

// VectorWriter 按批把子块向量写入磁盘索引。
// 收录测试传入 index.NewDisk 的结果，并使用夹具嵌入，不加载本地权重。
type VectorWriter interface {
	UpsertRecords(ctx context.Context, recs []index.VectorRecord) error
}

// Deps 是管道使用的协作件。Store、Embedder、Registry、Cleaner 不能为空。
// 缺任意一个时 New 返回错误，不会换成全局索引。
// Catalog 与 Vectors 只给收录任务使用。上传和提问不要求它们。
type Deps struct {
	Store    storage.Store
	Embedder embed.Embedder
	Registry index.Registry
	Reranker retrieve.Reranker
	Splitter chunk.Splitter
	Cleaner  clean.Cleaner
	Catalog  catalog.Store
	Vectors  VectorWriter
	// Passages 给试查重排打分。为空时 Try 返回错误。提问仍用 Reranker。
	Passages retrieve.PassageReranker
	// Parents 按父块编号取整节正文。为空时试查只保留子块正文。
	Parents retrieve.ParentLookup
}

// Pipeline 是上传、抓取、检索和评价的任务入口。
type Pipeline struct {
	cfg  Config
	deps Deps
	svc  retrieve.Service

	mu            sync.Mutex
	tryMu         sync.Mutex
	notedEmbedPID int
	parked        bool
	jobs          map[string]*jobRecord
	docs          map[model.CollectionID][]model.Document
	sem           chan struct{}
}

type jobRecord struct {
	Job         Job
	filename    string
	contentType string
	sourceURI   string
	fetch       *FetchInput
	crawl       *CrawlInput
	cancel      context.CancelFunc
	done        chan struct{}
	cleaned     map[model.ObjectKey]model.Document
}

type objectCheckpoint struct {
	ObjectKeys []string `json:"object_keys"`
	SourceURIs []string `json:"source_uris,omitempty"`
}

// New 组装管道。不拨号、不打开存储、不读取夹具。
// Store、Embedder、Registry 或 Cleaner 为空时返回错误。
func New(cfg Config, deps Deps) (*Pipeline, error) {
	if deps.Store == nil || deps.Embedder == nil || deps.Registry == nil || deps.Cleaner == nil {
		return nil, errMissingDeps
	}
	if cfg.FixturePath == "" {
		cfg.FixturePath = defaultFixturePath
	}
	if cfg.Weights.Vector == 0 && cfg.Weights.Keyword == 0 {
		cfg.Weights = retrieve.Weights{Vector: 0.7, Keyword: 0.3}
	}
	if cfg.Chunk.MaxLen <= 0 {
		cfg.Chunk.MaxLen = 800
	}
	if cfg.Chunk.Overlap <= 0 {
		cfg.Chunk.Overlap = 100
	}
	p := &Pipeline{
		cfg:  cfg,
		deps: deps,
		jobs: map[string]*jobRecord{},
		docs: map[model.CollectionID][]model.Document{},
	}
	if cfg.UploadConcurrency > 0 {
		p.sem = make(chan struct{}, cfg.UploadConcurrency)
	}
	p.svc = retrieve.New(deps.Registry, deps.Embedder, cfg.Weights, deps.Reranker)
	return p, nil
}

// AcceptUpload 只把 Body 流式写入存储并入队。
// 调用方得到任务编号、对象键、校验和和大小。清洗和嵌入在任务里做。
// 单次读取不得超过 storage.CopyBufSize。禁止把 Body 读进内存。
// 校验和不符时返回 storage.ErrChecksumMismatch，并且不重试。
// Inline 为 true 时，返回前清洗和建索引已经结束，成功状态为 succeeded。
func (p *Pipeline) AcceptUpload(ctx context.Context, in UploadInput) (Accepted, error) {
	if p == nil || p.deps.Store == nil {
		return Accepted{}, errMissingDeps
	}
	if in.Body == nil {
		return Accepted{}, errors.New("job: empty body")
	}
	if in.CollectionID == "" {
		return Accepted{}, errors.New("job: collection required")
	}
	key := newObjectKey(in.CollectionID, in.Filename)
	meta, err := p.deps.Store.Put(ctx, key, in.Body, storage.PutOptions{
		ContentType:    in.ContentType,
		CollectionID:   in.CollectionID,
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return Accepted{}, err
	}
	rec := p.newRecord(KindUpload, in.CollectionID)
	rec.filename = in.Filename
	rec.contentType = in.ContentType
	rec.Job.ObjectKey = string(meta.Key)
	rec.Job.Checksum = string(meta.Checksum)
	rec.Job.Size = meta.Size
	rec.Job.State = StateQueued
	p.mu.Lock()
	p.jobs[rec.Job.ID] = rec
	p.mu.Unlock()
	err = p.start(ctx, rec)
	return Accepted{
		Job:       p.snapshot(rec),
		ObjectKey: meta.Key,
		Checksum:  meta.Checksum,
		Size:      meta.Size,
	}, err
}

// SubmitFetch 按 Kind 把 URI 的抓取任务入队，并立刻带上任务编号。
// 任务里按 Kind 调用 acquire，再 IngestSource，然后与上传相同地清洗建索引。
// Inline 为 true 时，返回前任务已经结束。
func (p *Pipeline) SubmitFetch(ctx context.Context, in FetchInput) (Job, error) {
	if p == nil || p.deps.Store == nil {
		return Job{}, errMissingDeps
	}
	if in.CollectionID == "" || in.Kind == "" || in.URI == "" {
		return Job{}, errors.New("job: fetch input incomplete")
	}
	copied := in
	rec := p.newRecord(KindFetch, in.CollectionID)
	rec.fetch = &copied
	rec.Job.State = StateQueued
	p.mu.Lock()
	p.jobs[rec.Job.ID] = rec
	p.mu.Unlock()
	err := p.start(ctx, rec)
	return p.snapshot(rec), err
}

// GetJob 返回编号为 id 的任务。任务不存在时返回 ErrNotFound。
func (p *Pipeline) GetJob(ctx context.Context, id string) (Job, error) {
	if p == nil {
		return Job{}, errMissingDeps
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return cloneJob(rec.Job), nil
}

// Cancel 取消编号为 id 的任务。取消后状态为 canceled，不再重试。
// 任务不存在时返回 ErrNotFound。
func (p *Pipeline) Cancel(ctx context.Context, id string) error {
	if p == nil {
		return errMissingDeps
	}
	p.mu.Lock()
	rec, ok := p.jobs[id]
	if !ok {
		p.mu.Unlock()
		return ErrNotFound
	}
	rec.Job.State = StateCanceled
	cancel := rec.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Documents 返回一个集合内的清洗结果，包含 Duplicate 为 true 的文档。
// 不包含其他集合。
func (p *Pipeline) Documents(ctx context.Context, col model.CollectionID) ([]model.Document, error) {
	if p == nil {
		return nil, errMissingDeps
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	src := p.docs[col]
	out := make([]model.Document, len(src))
	copy(out, src)
	return out, nil
}

// Ask 只检索 req 点名的集合，规则与 retrieve.Service 相同。
// 集合名单原样传给检索，不搜索未点名的集合。
func (p *Pipeline) Ask(ctx context.Context, req retrieve.Request) ([]retrieve.Hit, error) {
	if p == nil {
		return nil, errMissingDeps
	}
	if len(req.Collections) == 0 {
		return nil, retrieve.ErrCollectionsRequired
	}
	forwarded := req
	forwarded.Collections = append([]model.CollectionID(nil), req.Collections...)
	hits, err := p.svc.Retrieve(ctx, forwarded)
	if err == nil {
		if hits == nil {
			return []retrieve.Hit{}, nil
		}
		return hits, nil
	}
	if err.Error() != "retrieve: not implemented" {
		return nil, err
	}
	return p.searchNamed(ctx, p.deps.Registry, forwarded)
}

func (p *Pipeline) newRecord(kind string, col model.CollectionID) *jobRecord {
	return &jobRecord{
		Job: Job{
			ID:           newID(),
			Kind:         kind,
			State:        StateQueued,
			CollectionID: string(col),
		},
		cleaned: map[model.ObjectKey]model.Document{},
	}
}

func (p *Pipeline) start(parent context.Context, rec *jobRecord) error {
	base := parent
	if !p.cfg.Inline || base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	rec.cancel = cancel
	rec.done = make(chan struct{})
	if p.cfg.Inline {
		defer close(rec.done)
		defer cancel()
		return p.executeWithRetry(ctx, rec)
	}
	go func() {
		defer close(rec.done)
		defer cancel()
		_ = p.executeWithRetry(ctx, rec)
	}()
	return nil
}

func (p *Pipeline) executeWithRetry(ctx context.Context, rec *jobRecord) error {
	if err := p.acquire(ctx); err != nil {
		p.finishCanceled(rec, err)
		return err
	}
	defer p.release()
	var last error
	for {
		p.mu.Lock()
		if rec.Job.State == StateCanceled || errors.Is(ctx.Err(), context.Canceled) {
			rec.Job.State = StateCanceled
			if ctx.Err() != nil {
				rec.Job.Error = ctx.Err().Error()
			}
			p.mu.Unlock()
			return context.Canceled
		}
		if rec.Job.Attempts >= p.maxAttempts() {
			if rec.Job.State != StateCanceled {
				rec.Job.State = StateFailed
			}
			p.mu.Unlock()
			if last == nil {
				last = errors.New("job: attempts exhausted")
			}
			return last
		}
		rec.Job.Attempts++
		rec.Job.State = StateRunning
		attempt := rec.Job.Attempts
		p.mu.Unlock()

		err := p.execute(ctx, rec)
		if err == nil {
			p.mu.Lock()
			if rec.Job.State != StateCanceled {
				rec.Job.State = StateSucceeded
				rec.Job.Error = ""
			}
			p.mu.Unlock()
			return nil
		}
		last = err
		p.mu.Lock()
		rec.Job.Error = err.Error()
		canceled := rec.Job.State == StateCanceled || errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
		if canceled {
			rec.Job.State = StateCanceled
			p.mu.Unlock()
			return err
		}
		if errors.Is(err, storage.ErrChecksumMismatch) {
			rec.Job.State = StateFailed
			p.mu.Unlock()
			return err
		}
		p.mu.Unlock()
		if attempt >= p.maxAttempts() {
			p.mu.Lock()
			if rec.Job.State != StateCanceled {
				rec.Job.State = StateFailed
			}
			p.mu.Unlock()
			return err
		}
	}
}

func (p *Pipeline) finishCanceled(rec *jobRecord, err error) {
	p.mu.Lock()
	rec.Job.State = StateCanceled
	if err != nil {
		rec.Job.Error = err.Error()
	}
	p.mu.Unlock()
}

func (p *Pipeline) execute(ctx context.Context, rec *jobRecord) error {
	switch rec.Job.Kind {
	case KindUpload:
		return p.processKey(ctx, rec, model.ObjectKey(rec.Job.ObjectKey), rec.filename, "")
	case KindFetch:
		return p.executeFetch(ctx, rec)
	case KindCrawl:
		return p.executeCrawl(ctx, rec)
	default:
		return fmt.Errorf("job: unknown kind %q", rec.Job.Kind)
	}
}

func (p *Pipeline) executeFetch(ctx context.Context, rec *jobRecord) error {
	if rec.fetch == nil {
		return errors.New("job: fetch input missing")
	}
	in := *rec.fetch
	src, err := openSource(in)
	if err != nil {
		return err
	}
	results, err := acquire.IngestSource(ctx, p.deps.Store, in.CollectionID, src, in.IdempotencyPrefix)
	if err != nil {
		return err
	}
	p.setProgress(rec, "fetch", 0, len(results))
	for i, res := range results {
		name := filenameOf(res.SourceURI)
		if err := p.processKey(ctx, rec, res.Meta.Key, name, res.SourceURI); err != nil {
			return err
		}
		p.setProgress(rec, "index", i+1, len(results))
	}
	return nil
}

func openSource(in FetchInput) (acquire.Source, error) {
	switch in.Kind {
	case acquire.KindLocalDir:
		return acquire.NewLocalDir(in.URI)
	case acquire.KindHTTP:
		return acquire.NewHTTP(in.URI, nil)
	case acquire.KindDatabase:
		return acquire.NewDatabase(in.URI)
	case acquire.KindSitemap:
		return acquire.NewSitemap(in.URI)
	case acquire.KindVendorAPI:
		return acquire.NewVendorAPI(in.URI)
	default:
		return nil, fmt.Errorf("job: unknown source kind %q", in.Kind)
	}
}

func (p *Pipeline) processKey(ctx context.Context, rec *jobRecord, key model.ObjectKey, filename, sourceURI string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if key == "" {
		return errors.New("job: empty object key")
	}
	if p.hasCheckpoint(rec, key) {
		return nil
	}
	doc, ok := p.cachedDoc(rec, key)
	if !ok {
		p.setProgress(rec, "clean", 0, 1)
		rc, meta, err := p.deps.Store.Get(ctx, key)
		if err != nil {
			return err
		}
		if rec.Job.Checksum != "" && meta.Checksum != "" && string(meta.Checksum) != rec.Job.Checksum {
			rc.Close()
			return storage.ErrChecksumMismatch
		}
		if filename == "" {
			filename = rec.filename
		}
		doc, err = p.deps.Cleaner.Clean(ctx, clean.Input{
			CollectionID: model.CollectionID(rec.Job.CollectionID),
			ObjectKey:    key,
			SourceURI:    sourceURI,
			Filename:     filename,
			Body:         rc,
		})
		rc.Close()
		if err != nil {
			return err
		}
		if doc.CollectionID == "" {
			doc.CollectionID = model.CollectionID(rec.Job.CollectionID)
		}
		p.cacheDoc(rec, key, doc)
	}
	if !doc.Duplicate {
		chunks, err := p.chunksFor(doc, splitNormal, nil)
		if err != nil {
			return err
		}
		if len(chunks) > 0 {
			p.setProgress(rec, "embed", 0, 1)
			texts := make([]string, len(chunks))
			for i, ch := range chunks {
				texts[i] = ch.Text
			}
			vecs, err := p.embedAll(ctx, texts)
			if err != nil {
				return err
			}
			idx, err := p.deps.Registry.Open(ctx, doc.CollectionID)
			if err != nil {
				return err
			}
			if err := idx.Upsert(ctx, chunks, vecs); err != nil {
				return err
			}
			ids := make([]model.ChunkID, len(chunks))
			for i, ch := range chunks {
				ids[i] = ch.ID
			}
			doc.Lineage.ChunkIDs = ids
			p.cacheDoc(rec, key, doc)
		}
	}
	p.addDoc(doc)
	p.setProgress(rec, "index", 1, 1)
	return p.markCheckpoint(rec, key)
}

func (p *Pipeline) hasCheckpoint(rec *jobRecord, key model.ObjectKey) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	var cp objectCheckpoint
	if len(rec.Job.Checkpoint) == 0 {
		return false
	}
	if err := json.Unmarshal(rec.Job.Checkpoint, &cp); err != nil {
		return false
	}
	for _, item := range cp.ObjectKeys {
		if item == string(key) {
			return true
		}
	}
	return false
}

func (p *Pipeline) markCheckpoint(rec *jobRecord, key model.ObjectKey) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var cp objectCheckpoint
	if len(rec.Job.Checkpoint) > 0 {
		if err := json.Unmarshal(rec.Job.Checkpoint, &cp); err != nil {
			return err
		}
	}
	for _, item := range cp.ObjectKeys {
		if item == string(key) {
			return nil
		}
	}
	cp.ObjectKeys = append(cp.ObjectKeys, string(key))
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	rec.Job.Checkpoint = raw
	return nil
}

func (p *Pipeline) cachedDoc(rec *jobRecord, key model.ObjectKey) (model.Document, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	doc, ok := rec.cleaned[key]
	return doc, ok
}

func (p *Pipeline) cacheDoc(rec *jobRecord, key model.ObjectKey, doc model.Document) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rec.cleaned == nil {
		rec.cleaned = map[model.ObjectKey]model.Document{}
	}
	rec.cleaned[key] = doc
}

func (p *Pipeline) addDoc(doc model.Document) {
	p.mu.Lock()
	defer p.mu.Unlock()
	list := p.docs[doc.CollectionID]
	for _, existing := range list {
		if existing.ID == doc.ID && existing.Duplicate == doc.Duplicate && existing.Lineage.RawObjectKey == doc.Lineage.RawObjectKey {
			return
		}
	}
	p.docs[doc.CollectionID] = append(list, doc)
}

func (p *Pipeline) setProgress(rec *jobRecord, stage string, done, total int) {
	if done > total {
		done = total
	}
	p.mu.Lock()
	rec.Job.Progress = Progress{Stage: stage, Done: done, Total: total}
	p.mu.Unlock()
}

func (p *Pipeline) snapshot(rec *jobRecord) Job {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneJob(rec.Job)
}

func (p *Pipeline) maxAttempts() int {
	if p.cfg.Retry.MaxAttempts > 0 {
		return p.cfg.Retry.MaxAttempts
	}
	return defaultAttempts
}

func (p *Pipeline) acquire(ctx context.Context) error {
	if p.sem == nil {
		return nil
	}
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Pipeline) release() {
	if p.sem == nil {
		return
	}
	<-p.sem
}

func cloneJob(job Job) Job {
	if job.Checkpoint != nil {
		job.Checkpoint = append(json.RawMessage(nil), job.Checkpoint...)
	}
	return job
}

func newID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("job_%d", time.Now().UnixNano())
	}
	return "job_" + hex.EncodeToString(buf[:])
}

func newObjectKey(col model.CollectionID, filename string) model.ObjectKey {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	name := strings.TrimSpace(filename)
	if name == "" {
		name = "object"
	}
	name = strings.NewReplacer("/", "_", "\\", "_").Replace(name)
	return model.ObjectKey(fmt.Sprintf("raw/%s/%s-%s", col, hex.EncodeToString(buf[:]), name))
}

func filenameOf(uri string) string {
	uri = strings.TrimRight(uri, "/")
	if i := strings.LastIndex(uri, "/"); i >= 0 && i < len(uri)-1 {
		return uri[i+1:]
	}
	if uri == "" {
		return "object"
	}
	return uri
}
