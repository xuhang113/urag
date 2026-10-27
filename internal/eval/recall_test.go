package eval_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/eval"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
)

// recallEmbedDim 让夹具里的 59 个词各自落在不同桶里，对照切分才不会靠哈希碰撞蒙混过关。
const recallEmbedDim = 32768

func TestUnnamedCollectionNotSearched(t *testing.T) {
	fx := loadFixture(t)
	ctx := context.Background()
	emb := embed.NewFixture(recallEmbedDim)
	reg := indexFixture(t, fx, emb, paragraphChunks)

	gamma, ok := reg.Get("gamma")
	if !ok {
		t.Fatal("gamma 没有写入索引")
	}
	gammaHits, err := gamma.SearchKeyword(ctx, index.Query{
		CollectionID: "gamma",
		Tokens:       embed.Tokens("obsidian vault"),
		TopK:         fx.K,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gammaHits) != 1 || gammaHits[0].Chunk.DocumentID != "gamma-secret" {
		t.Fatalf("gamma-secret 不在 gamma 索引里: %+v", gammaHits)
	}

	rec := &recordingRegistry{inner: reg}
	svc := retrieve.New(rec, emb, retrieve.Weights{}, nil)
	hits, err := svc.Retrieve(ctx, retrieve.Request{
		Collections: []model.CollectionID{"alpha", "beta"},
		Query:       "crimson lantern festival",
		TopK:        fx.K,
		Hybrid:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoGamma(t, hits)
	if !containsDoc(hits, "alpha-lantern") {
		t.Fatalf("alpha 查询没有召回 alpha-lantern: %+v", hits)
	}
	for _, id := range rec.opened {
		if id == "gamma" || (id != "alpha" && id != "beta") {
			t.Fatalf("打开了未点名的集合 %s，记录 %v", id, rec.opened)
		}
	}

	onlyAlpha, err := svc.Retrieve(ctx, retrieve.Request{
		Collections: []model.CollectionID{"alpha"},
		Query:       "obsidian vault",
		TopK:        fx.K,
		Hybrid:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoGamma(t, onlyAlpha)
	for _, id := range rec.opened {
		if id == "gamma" {
			t.Fatal("只查询 alpha 时打开了 gamma")
		}
	}
}

func TestCrossCollectionHitsCarryID(t *testing.T) {
	fx := loadFixture(t)
	ctx := context.Background()
	emb := embed.NewFixture(recallEmbedDim)
	reg := indexFixture(t, fx, emb, paragraphChunks)
	svc := retrieve.New(reg, emb, retrieve.Weights{}, nil)
	home := docHome(fx)
	original := docText(fx)

	for _, id := range []string{"m-lantern", "m-harbor"} {
		q := findQuestion(t, fx, id)
		hits, err := svc.Retrieve(ctx, retrieve.Request{
			Collections: q.Collections,
			Query:       q.Query,
			TopK:        fx.K,
			Hybrid:      true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) == 0 {
			t.Fatalf("%s 没有命中", id)
		}
		for _, h := range hits {
			if h.CollectionID == "" || h.DocumentID == "" || h.ChunkID == "" || h.Text == "" {
				t.Fatalf("%s 命中缺字段: %+v", id, h)
			}
			if h.CollectionID != home[h.DocumentID] {
				t.Fatalf("%s 文档 %s 的集合是 %s，命中写成 %s", id, h.DocumentID, home[h.DocumentID], h.CollectionID)
			}
			raw := original[h.DocumentID]
			if h.StartByte < 0 || h.EndByte > len(raw) || h.EndByte < h.StartByte || raw[h.StartByte:h.EndByte] != h.Text {
				t.Fatalf("%s 字节区间对不上: %+v", id, h)
			}
		}
	}

	lantern := findQuestion(t, fx, "m-lantern")
	harbor := findQuestion(t, fx, "m-harbor")
	both, err := svc.Retrieve(ctx, retrieve.Request{
		Collections: []model.CollectionID{"alpha", "beta"},
		Query:       lantern.Query + " " + harbor.Query,
		TopK:        fx.K,
		Hybrid:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sawAlpha, sawBeta := false, false
	for _, h := range both {
		if h.CollectionID != home[h.DocumentID] || h.CollectionID == "" {
			t.Fatalf("跨集合命中的集合编号不对: %+v", h)
		}
		switch h.CollectionID {
		case "alpha":
			sawAlpha = true
			if h.DocumentID != "alpha-lantern" {
				t.Fatalf("alpha 命中了 %s", h.DocumentID)
			}
		case "beta":
			sawBeta = true
			if h.DocumentID != "beta-harbor" {
				t.Fatalf("beta 命中了 %s", h.DocumentID)
			}
		default:
			t.Fatalf("跨集合查询返回了 %s", h.CollectionID)
		}
	}
	if !sawAlpha || !sawBeta {
		t.Fatalf("同一次查询没有同时带回 alpha 和 beta: %+v", both)
	}
}

func TestFixtureRecallAndMRR(t *testing.T) {
	fx := loadFixture(t)
	requireThresholds(t, fx)
	requireDocumentIDs(t, fx)
	ctx := context.Background()
	emb := embed.NewFixture(recallEmbedDim)
	reg := indexFixture(t, fx, emb, paragraphChunks)
	for _, col := range fx.Collections {
		for _, doc := range col.Documents {
			parts := paragraphChunks(model.Document{ID: doc.ID, CollectionID: col.ID, Text: doc.Text})
			if len(parts) != 1 || parts[0].Text != doc.Text || len(doc.Text) > 800 {
				t.Fatalf("%s 不是整篇一块", doc.ID)
			}
		}
	}
	ranked := map[string][]model.DocumentID{}
	report, err := eval.Run(ctx, fx, recordingRanker(reg, emb, fx.K, ranked), "paragraph", "")
	if err != nil {
		t.Fatal(err)
	}
	if report.Splitter != "paragraph" || report.Direction != "baseline" || report.RecallDelta != 0 || report.MRRDelta != 0 {
		t.Fatalf("基线报告不对: %+v", report)
	}
	byID := scoreByID(report)
	for _, id := range []string{"alpha", "beta", "merged"} {
		s, ok := byID[id]
		if !ok {
			t.Fatalf("缺少集合 %s 的分数\n%s", id, formatRanks(fx, ranked))
		}
		if s.K != fx.K || s.RecallAtK < fx.Thresholds.NormalRecallAtK || s.MRR < fx.Thresholds.NormalMRR {
			t.Fatalf("集合 %s Recall@K=%v（阈值 %v）MRR=%v（阈值 %v）K=%d\n%s",
				id, s.RecallAtK, fx.Thresholds.NormalRecallAtK, s.MRR, fx.Thresholds.NormalMRR, s.K, formatRanks(fx, ranked))
		}
	}
}

func TestContrastSplitterWorse(t *testing.T) {
	fx := loadFixture(t)
	requireThresholds(t, fx)
	ctx := context.Background()
	emb := embed.NewFixture(recallEmbedDim)

	for _, col := range fx.Collections {
		for _, doc := range col.Documents {
			parts := contrastChunks(model.Document{ID: doc.ID, CollectionID: col.ID, Text: doc.Text}, fx.ContrastDropTerms)
			for _, part := range parts {
				if len(part.Text) > 20 {
					t.Fatalf("%s 碎片超过 20 字节: %q", part.ID, part.Text)
				}
				for _, tok := range embed.Tokens(part.Text) {
					for _, term := range fx.ContrastDropTerms {
						if tok == strings.ToLower(term) {
							t.Fatalf("%s 仍含整词 %s: %q", part.ID, term, part.Text)
						}
					}
				}
			}
		}
	}

	normalReg := indexFixture(t, fx, emb, paragraphChunks)
	contrastReg := indexFixture(t, fx, emb, func(doc model.Document) []model.Chunk {
		return contrastChunks(doc, fx.ContrastDropTerms)
	})
	normalRanked := map[string][]model.DocumentID{}
	contrastRanked := map[string][]model.DocumentID{}
	normal, err := eval.Run(ctx, fx, recordingRanker(normalReg, emb, fx.K, normalRanked), "paragraph", "")
	if err != nil {
		t.Fatal(err)
	}
	contrast, err := eval.Run(ctx, fx, recordingRanker(contrastReg, emb, fx.K, contrastRanked), "drop_keywords", "")
	if err != nil {
		t.Fatal(err)
	}
	compared := eval.Compare(normal, contrast)
	if compared.Direction != "down" || compared.RecallDelta >= 0 {
		t.Fatalf("对照 Direction=%s delta=%v\n正常 %s\n对照 %s",
			compared.Direction, compared.RecallDelta, formatRanks(fx, normalRanked), formatRanks(fx, contrastRanked))
	}
	if compared.Splitter != "drop_keywords" || compared.Baseline != "paragraph" {
		t.Fatalf("对照报告没有标出基线: %+v", compared)
	}
	normalBy := scoreByID(normal)
	seen := map[string]bool{}
	for _, s := range contrast.Scores {
		seen[s.CollectionID] = true
		if s.RecallAtK > fx.Thresholds.ContrastRecallAtKMax {
			t.Fatalf("集合 %s 对照 Recall@K=%v，高于 %v\n%s",
				s.CollectionID, s.RecallAtK, fx.Thresholds.ContrastRecallAtKMax, formatRanks(fx, contrastRanked))
		}
		base, ok := normalBy[s.CollectionID]
		if !ok || s.RecallAtK >= base.RecallAtK {
			t.Fatalf("集合 %s 对照 Recall@K=%v 没有严格低于正常的 %v", s.CollectionID, s.RecallAtK, base.RecallAtK)
		}
	}
	for _, id := range []string{"alpha", "beta", "merged"} {
		if !seen[id] {
			t.Fatalf("对照分数缺少 %s", id)
		}
	}
}

func TestRunBucketsAndDedupe(t *testing.T) {
	ctx := context.Background()
	fx := eval.Fixture{
		K: 5,
		Questions: []eval.Question{
			{
				ID:           "q",
				Collections:  []model.CollectionID{"alpha"},
				RelevantDocs: []model.DocumentID{"a"},
			},
			{
				ID:           "m",
				Collections:  []model.CollectionID{"alpha", "beta"},
				RelevantDocs: []model.DocumentID{"a"},
			},
		},
	}
	report, err := eval.Run(ctx, fx, func(_ context.Context, q eval.Question) ([]model.DocumentID, error) {
		if q.ID == "q" {
			return []model.DocumentID{"b", "a", "a", "b"}, nil
		}
		return []model.DocumentID{"a", "a"}, nil
	}, "paragraph", "")
	if err != nil {
		t.Fatal(err)
	}
	if report.Direction != "baseline" || report.RecallDelta != 0 || report.MRRDelta != 0 {
		t.Fatalf("%+v", report)
	}
	byID := scoreByID(report)
	if byID["alpha"].RecallAtK != 1 || byID["alpha"].MRR != 0.5 || byID["alpha"].K != 5 {
		t.Fatalf("alpha %+v", byID["alpha"])
	}
	if byID["merged"].RecallAtK != 1 || byID["merged"].MRR != 1 {
		t.Fatalf("merged %+v", byID["merged"])
	}
	if _, err := eval.Run(ctx, eval.Fixture{Questions: []eval.Question{{ID: "bare"}}}, func(context.Context, eval.Question) ([]model.DocumentID, error) {
		return nil, nil
	}, "paragraph", "paragraph"); err == nil {
		t.Fatal("没有集合的题应该失败")
	}
}

func TestCompareDirections(t *testing.T) {
	base := eval.Report{Splitter: "paragraph", Scores: []eval.Score{{CollectionID: "merged", RecallAtK: 0.5, MRR: 1}}}
	up := eval.Compare(base, eval.Report{Splitter: "wider", Scores: []eval.Score{{CollectionID: "merged", RecallAtK: 1, MRR: 1}}})
	if up.Direction != "up" || up.RecallDelta <= 0 || up.Baseline != "paragraph" {
		t.Fatalf("%+v", up)
	}
	flat := eval.Compare(base, eval.Report{Splitter: "same", Scores: []eval.Score{{CollectionID: "merged", RecallAtK: 0.5, MRR: 0}}})
	if flat.Direction != "flat" || flat.RecallDelta != 0 || flat.MRRDelta != -1 {
		t.Fatalf("%+v", flat)
	}
	macro := eval.Compare(base, eval.Report{Splitter: "split", Scores: []eval.Score{
		{CollectionID: "alpha", RecallAtK: 1},
		{CollectionID: "beta", RecallAtK: 0},
	}})
	if macro.Direction != "flat" || macro.RecallDelta != 0 {
		t.Fatalf("宏平均 %+v", macro)
	}
}

func TestLoadFixture(t *testing.T) {
	fx := loadFixture(t)
	if fx.K != 5 || len(fx.ContrastDropTerms) != 15 || len(fx.Questions) != 7 || len(fx.Collections) != 3 {
		t.Fatalf("夹具形状不对: k=%d terms=%d questions=%d collections=%d", fx.K, len(fx.ContrastDropTerms), len(fx.Questions), len(fx.Collections))
	}
	if _, err := eval.Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("缺少文件时应返回错误")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"k":1,"extra":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := eval.Load(bad); err == nil {
		t.Fatal("未知字段应返回错误")
	}
}

func loadFixture(t *testing.T) eval.Fixture {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("找不到测试文件路径")
	}
	fx, err := eval.Load(filepath.Join(filepath.Dir(file), "..", "..", "testdata", "recall", "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	return fx
}

func requireThresholds(t *testing.T, fx eval.Fixture) {
	t.Helper()
	if fx.K != 5 || fx.Thresholds.NormalRecallAtK < 0.8 || fx.Thresholds.NormalMRR < 0.6 || fx.Thresholds.ContrastRecallAtKMax > 0.4 {
		t.Fatalf("夹具阈值被放宽: k=%d %+v", fx.K, fx.Thresholds)
	}
}

func requireDocumentIDs(t *testing.T, fx eval.Fixture) {
	t.Helper()
	var got []model.DocumentID
	for _, col := range fx.Collections {
		for _, doc := range col.Documents {
			got = append(got, doc.ID)
		}
	}
	want := []model.DocumentID{"alpha-lantern", "alpha-canal", "alpha-library", "beta-harbor", "beta-market", "gamma-secret"}
	if len(got) != len(want) {
		t.Fatalf("文档编号 %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("文档编号 %v", got)
		}
	}
}

func indexFixture(t *testing.T, fx eval.Fixture, emb embed.Embedder, split func(model.Document) []model.Chunk) index.Registry {
	t.Helper()
	reg := index.NewMemory()
	ctx := context.Background()
	for _, col := range fx.Collections {
		idx, err := reg.Open(ctx, col.ID)
		if err != nil {
			t.Fatal(err)
		}
		var chunks []model.Chunk
		for _, doc := range col.Documents {
			chunks = append(chunks, split(model.Document{ID: doc.ID, CollectionID: col.ID, Text: doc.Text})...)
		}
		texts := make([]string, len(chunks))
		for i, chunk := range chunks {
			texts[i] = chunk.Text
		}
		vecs, err := emb.Embed(ctx, texts)
		if err != nil {
			t.Fatal(err)
		}
		if err := idx.Upsert(ctx, chunks, vecs); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func recordingRanker(reg index.Registry, emb embed.Embedder, k int, ranked map[string][]model.DocumentID) func(context.Context, eval.Question) ([]model.DocumentID, error) {
	svc := retrieve.New(reg, emb, retrieve.Weights{}, nil)
	return func(ctx context.Context, q eval.Question) ([]model.DocumentID, error) {
		hits, err := svc.Retrieve(ctx, retrieve.Request{
			Collections: q.Collections,
			Query:       q.Query,
			TopK:        k,
			Hybrid:      true,
		})
		if err != nil {
			return nil, err
		}
		ids := make([]model.DocumentID, len(hits))
		for i, h := range hits {
			ids[i] = h.DocumentID
		}
		ranked[q.ID] = ids
		return ids, nil
	}
}

func paragraphChunks(doc model.Document) []model.Chunk {
	if doc.Text == "" {
		return nil
	}
	return []model.Chunk{{
		ID:           model.ChunkID(string(doc.ID) + "#0"),
		DocumentID:   doc.ID,
		CollectionID: doc.CollectionID,
		Ordinal:      0,
		Text:         doc.Text,
		StartByte:    0,
		EndByte:      len(doc.Text),
	}}
}

type wordSpan struct {
	lower string
	start int
	end   int
}

func contrastChunks(doc model.Document, drop []string) []model.Chunk {
	ban := make(map[string]struct{}, len(drop))
	for _, term := range drop {
		ban[strings.ToLower(term)] = struct{}{}
	}
	var kept []wordSpan
	for _, w := range wordSpans(doc.Text) {
		if _, ok := ban[w.lower]; ok {
			continue
		}
		kept = append(kept, w)
	}
	var chunks []model.Chunk
	var cur []wordSpan
	curLen := 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		parts := make([]string, len(cur))
		for i, w := range cur {
			parts[i] = w.lower
		}
		text := strings.Join(parts, " ")
		chunks = append(chunks, model.Chunk{
			ID:           model.ChunkID(fmt.Sprintf("%s#%d", doc.ID, len(chunks))),
			DocumentID:   doc.ID,
			CollectionID: doc.CollectionID,
			Ordinal:      len(chunks),
			Text:         text,
			StartByte:    cur[0].start,
			EndByte:      cur[len(cur)-1].end,
		})
		cur = nil
		curLen = 0
	}
	for _, w := range kept {
		pieces := []string{w.lower}
		if len(w.lower) > 20 {
			pieces = pieces[:0]
			for i := 0; i < len(w.lower); i += 20 {
				end := i + 20
				if end > len(w.lower) {
					end = len(w.lower)
				}
				pieces = append(pieces, w.lower[i:end])
			}
		}
		for _, piece := range pieces {
			add := len(piece)
			if len(cur) > 0 {
				add++
			}
			if len(cur) > 0 && curLen+add > 20 {
				flush()
			}
			cur = append(cur, wordSpan{lower: piece, start: w.start, end: w.end})
			if curLen == 0 {
				curLen = len(piece)
			} else {
				curLen += 1 + len(piece)
			}
		}
	}
	flush()
	return chunks
}

func wordSpans(text string) []wordSpan {
	var out []wordSpan
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if !unicode.IsLetter(r) {
			i += size
			continue
		}
		start := i
		var b strings.Builder
		for i < len(text) {
			r, size = utf8.DecodeRuneInString(text[i:])
			if !unicode.IsLetter(r) {
				break
			}
			b.WriteRune(r)
			i += size
		}
		out = append(out, wordSpan{lower: strings.ToLower(b.String()), start: start, end: i})
	}
	return out
}

func docHome(fx eval.Fixture) map[model.DocumentID]model.CollectionID {
	home := make(map[model.DocumentID]model.CollectionID)
	for _, col := range fx.Collections {
		for _, doc := range col.Documents {
			home[doc.ID] = col.ID
		}
	}
	return home
}

func docText(fx eval.Fixture) map[model.DocumentID]string {
	text := make(map[model.DocumentID]string)
	for _, col := range fx.Collections {
		for _, doc := range col.Documents {
			text[doc.ID] = doc.Text
		}
	}
	return text
}

func findQuestion(t *testing.T, fx eval.Fixture, id string) eval.Question {
	t.Helper()
	for _, q := range fx.Questions {
		if q.ID == id {
			return q
		}
	}
	t.Fatalf("夹具里没有题目 %s", id)
	return eval.Question{}
}

func containsDoc(hits []retrieve.Hit, id model.DocumentID) bool {
	for _, h := range hits {
		if h.DocumentID == id {
			return true
		}
	}
	return false
}

func assertNoGamma(t *testing.T, hits []retrieve.Hit) {
	t.Helper()
	for _, h := range hits {
		if h.CollectionID == "gamma" || h.DocumentID == "gamma-secret" || strings.Contains(h.Text, "gamma only token") {
			t.Fatalf("未点名集合 gamma 出现在命中里: %+v", h)
		}
	}
}

func scoreByID(report eval.Report) map[string]eval.Score {
	out := make(map[string]eval.Score, len(report.Scores))
	for _, s := range report.Scores {
		out[s.CollectionID] = s
	}
	return out
}

func formatRanks(fx eval.Fixture, ranked map[string][]model.DocumentID) string {
	var b strings.Builder
	for _, q := range fx.Questions {
		ids := ranked[q.ID]
		rank := 0
		rel := make(map[model.DocumentID]struct{}, len(q.RelevantDocs))
		for _, id := range q.RelevantDocs {
			rel[id] = struct{}{}
		}
		seen := make(map[model.DocumentID]struct{})
		pos := 0
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			pos++
			if _, ok := rel[id]; ok {
				rank = pos
				break
			}
		}
		fmt.Fprintf(&b, "%s collections=%v query=%q want=%v got=%v firstRelevantRank=%d\n",
			q.ID, q.Collections, q.Query, q.RelevantDocs, ids, rank)
	}
	return b.String()
}

type recordingRegistry struct {
	inner  index.Registry
	opened []model.CollectionID
}

func (r *recordingRegistry) Open(ctx context.Context, id model.CollectionID) (index.Index, error) {
	r.opened = append(r.opened, id)
	return r.inner.Open(ctx, id)
}

func (r *recordingRegistry) Get(id model.CollectionID) (index.Index, bool) {
	return r.inner.Get(id)
}
