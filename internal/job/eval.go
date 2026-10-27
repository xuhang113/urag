package job

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/eval"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
)

type splitMode int

const (
	splitNormal splitMode = iota
	splitContrast
)

// Evaluate 读取夹具，用文档编号原样建索引，并生成报告。
// 正常切分在段落切分尚未接通时把每篇短文做成一个片段，编号为 documentID#0。
// 没有对照时 Direction 为 baseline，RecallDelta 与 MRRDelta 为 0。
// eval 包没有 Run，分数在本包内用 RecallAtK 和 MeanReciprocalRank 计算。
func (p *Pipeline) Evaluate(ctx context.Context) (eval.Report, error) {
	fix, err := p.loadFixture()
	if err != nil {
		return eval.Report{}, err
	}
	hits, err := p.indexAndAsk(ctx, fix, splitNormal)
	if err != nil {
		return eval.Report{}, err
	}
	return buildReport(fix, hits, "paragraph", "paragraph", "baseline", 0, 0)
}

// EvaluateContrast 丢掉夹具里的 contrast_drop_terms，把正文切成不超过 20 字节的碎片后再打分。
// Direction 相对正常结果取 up、down 或 flat。
func (p *Pipeline) EvaluateContrast(ctx context.Context) (eval.Report, error) {
	fix, err := p.loadFixture()
	if err != nil {
		return eval.Report{}, err
	}
	k := fixtureK(fix)
	baseHits, err := p.indexAndAsk(ctx, fix, splitNormal)
	if err != nil {
		return eval.Report{}, err
	}
	baseRecall, baseMRR, err := macroAverages(fix.Questions, baseHits, k)
	if err != nil {
		return eval.Report{}, err
	}
	conHits, err := p.indexAndAsk(ctx, fix, splitContrast)
	if err != nil {
		return eval.Report{}, err
	}
	conRecall, conMRR, err := macroAverages(fix.Questions, conHits, k)
	if err != nil {
		return eval.Report{}, err
	}
	report, err := buildReport(fix, conHits, "drop_keywords", "paragraph", directionOf(conRecall-baseRecall, conMRR-baseMRR), conRecall-baseRecall, conMRR-baseMRR)
	if err != nil {
		return eval.Report{}, err
	}
	if err := p.indexFixture(ctx, fix, splitNormal); err != nil {
		return eval.Report{}, err
	}
	return report, nil
}

func (p *Pipeline) loadFixture() (eval.Fixture, error) {
	path := p.cfg.FixturePath
	if path == "" {
		path = defaultFixturePath
	}
	f, err := os.Open(path)
	if err != nil {
		return eval.Fixture{}, err
	}
	defer f.Close()
	var fix eval.Fixture
	if err := json.NewDecoder(f).Decode(&fix); err != nil {
		return eval.Fixture{}, fmt.Errorf("job: fixture: %w", err)
	}
	return fix, nil
}

func (p *Pipeline) indexAndAsk(ctx context.Context, fix eval.Fixture, mode splitMode) ([][]retrieve.Hit, error) {
	if err := p.indexFixture(ctx, fix, mode); err != nil {
		return nil, err
	}
	return p.askFixture(ctx, fix)
}

func (p *Pipeline) indexFixture(ctx context.Context, fix eval.Fixture, mode splitMode) error {
	for _, col := range fix.Collections {
		idx, err := p.deps.Registry.Open(ctx, col.ID)
		if err != nil {
			return err
		}
		var chunks []model.Chunk
		for _, doc := range col.Documents {
			parts, err := p.chunksFor(model.Document{
				ID:           doc.ID,
				CollectionID: col.ID,
				Text:         doc.Text,
			}, mode, fix.ContrastDropTerms)
			if err != nil {
				return err
			}
			chunks = append(chunks, parts...)
		}
		if len(chunks) == 0 {
			continue
		}
		texts := make([]string, len(chunks))
		for i, ch := range chunks {
			texts[i] = ch.Text
		}
		vecs, err := p.embedAll(ctx, texts)
		if err != nil {
			return err
		}
		if err := idx.Upsert(ctx, chunks, vecs); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pipeline) askFixture(ctx context.Context, fix eval.Fixture) ([][]retrieve.Hit, error) {
	k := fixtureK(fix)
	hits := make([][]retrieve.Hit, len(fix.Questions))
	for i, q := range fix.Questions {
		got, err := p.Ask(ctx, retrieve.Request{
			Collections: append([]model.CollectionID(nil), q.Collections...),
			Query:       q.Query,
			TopK:        k,
			Hybrid:      true,
		})
		if err != nil {
			return nil, err
		}
		hits[i] = got
	}
	return hits, nil
}

func (p *Pipeline) chunksFor(doc model.Document, mode splitMode, terms []string) ([]model.Chunk, error) {
	if mode == splitContrast {
		return contrastChunks(doc, terms), nil
	}
	if p.deps.Splitter != nil {
		chunks, err := p.deps.Splitter.Split(doc, p.cfg.Chunk)
		if err == nil {
			return chunks, nil
		}
		if !isNotImplemented(err) {
			return nil, err
		}
	}
	if doc.Text == "" {
		return nil, nil
	}
	return []model.Chunk{wholeChunk(doc)}, nil
}

func wholeChunk(doc model.Document) model.Chunk {
	return model.Chunk{
		ID:           model.ChunkID(string(doc.ID) + "#0"),
		DocumentID:   doc.ID,
		CollectionID: doc.CollectionID,
		Ordinal:      0,
		Text:         doc.Text,
		StartByte:    0,
		EndByte:      len(doc.Text),
	}
}

func (p *Pipeline) embedAll(ctx context.Context, texts []string) ([]embed.Vector, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	batch := p.cfg.Limits.EmbedBatch
	if batch <= 0 || batch >= len(texts) {
		vecs, err := p.deps.Embedder.Embed(ctx, texts)
		if err != nil {
			return nil, err
		}
		if len(vecs) != len(texts) {
			return nil, fmt.Errorf("job: embedder returned %d vectors for %d texts", len(vecs), len(texts))
		}
		return vecs, nil
	}
	out := make([]embed.Vector, 0, len(texts))
	for i := 0; i < len(texts); i += batch {
		j := i + batch
		if j > len(texts) {
			j = len(texts)
		}
		part, err := p.deps.Embedder.Embed(ctx, texts[i:j])
		if err != nil {
			return nil, err
		}
		if len(part) != j-i {
			return nil, fmt.Errorf("job: embedder returned %d vectors for %d texts", len(part), j-i)
		}
		out = append(out, part...)
	}
	return out, nil
}

func buildReport(fix eval.Fixture, hits [][]retrieve.Hit, splitter, baseline, direction string, recallDelta, mrrDelta float64) (eval.Report, error) {
	k := fixtureK(fix)
	recalls, err := eval.RecallAtK(fix.Questions, hits, k)
	if err != nil {
		return eval.Report{}, err
	}
	mrrs, err := eval.MeanReciprocalRank(fix.Questions, hits)
	if err != nil {
		return eval.Report{}, err
	}
	return eval.Report{
		Splitter:    splitter,
		Baseline:    baseline,
		Scores:      mergeScores(recalls, mrrs),
		RecallDelta: recallDelta,
		MRRDelta:    mrrDelta,
		Direction:   direction,
	}, nil
}

func mergeScores(recalls, mrrs []eval.Score) []eval.Score {
	order := make([]string, 0, len(recalls))
	byID := map[string]eval.Score{}
	for _, score := range recalls {
		if _, ok := byID[score.CollectionID]; !ok {
			order = append(order, score.CollectionID)
		}
		byID[score.CollectionID] = score
	}
	for _, score := range mrrs {
		cur, ok := byID[score.CollectionID]
		if !ok {
			order = append(order, score.CollectionID)
			cur = score
		}
		cur.MRR = score.MRR
		byID[score.CollectionID] = cur
	}
	out := make([]eval.Score, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

func macroAverages(questions []eval.Question, hits [][]retrieve.Hit, k int) (float64, float64, error) {
	if len(questions) == 0 {
		return 0, 0, nil
	}
	var recallSum, mrrSum float64
	for i := range questions {
		recalls, err := eval.RecallAtK(questions[i:i+1], hits[i:i+1], k)
		if err != nil {
			return 0, 0, err
		}
		mrrs, err := eval.MeanReciprocalRank(questions[i:i+1], hits[i:i+1])
		if err != nil {
			return 0, 0, err
		}
		if len(recalls) == 1 {
			recallSum += recalls[0].RecallAtK
		}
		if len(mrrs) == 1 {
			mrrSum += mrrs[0].MRR
		}
	}
	n := float64(len(questions))
	return recallSum / n, mrrSum / n, nil
}

func directionOf(recallDelta, mrrDelta float64) string {
	const eps = 1e-9
	switch {
	case recallDelta > eps:
		return "up"
	case recallDelta < -eps:
		return "down"
	case mrrDelta > eps:
		return "up"
	case mrrDelta < -eps:
		return "down"
	default:
		return "flat"
	}
}

func fixtureK(fix eval.Fixture) int {
	if fix.K <= 0 {
		return 5
	}
	return fix.K
}

func isNotImplemented(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not implemented")
}
