// Package eval 定义召回夹具，以及 Recall@K 和 MRR 的计算。
// 本包按题目宏平均，并把多集合题归到 merged。
// 本包不改写索引，也不代替检索决定命中顺序。
package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
)

// ErrNotImplemented 是评价尚未接通时的哨兵。
// Load 会读取夹具文件，不返回这个错误。
var ErrNotImplemented = errors.New("eval: not implemented")

// Thresholds 是召回测试的及格线。低于对应阈值时测试失败。
type Thresholds struct {
	NormalRecallAtK      float64 `json:"normal_recall_at_k"`
	NormalMRR            float64 `json:"normal_mrr"`
	ContrastRecallAtKMax float64 `json:"contrast_recall_at_k_max"`
}

// Question 是一道评价题。
// 只含一个集合的题计入该集合。含多个集合的题计入 collection_id 为 merged 的分数。
type Question struct {
	ID           string               `json:"id"`
	Collections  []model.CollectionID `json:"collections"`
	Query        string               `json:"query"`
	RelevantDocs []model.DocumentID   `json:"relevant_docs"`
}

// Score 是一个集合桶上的宏平均。
type Score struct {
	CollectionID string  `json:"collection_id"`
	RecallAtK    float64 `json:"recall_at_k"`
	MRR          float64 `json:"mrr"`
	K            int     `json:"k"`
}

// Report 比较一种切分和基线。
// 没有基线时 Direction 为 baseline，RecallDelta 与 MRRDelta 为 0。
// Compare 优先用 merged 的分数做差；没有 merged 时用各桶分数的宏平均。
// 变高为 up，变低为 down，相同为 flat。
// Scores 仍按集合分桶。多个集合的题只出现在 collection_id 为 merged 的项里。
type Report struct {
	Splitter    string  `json:"splitter"`
	Baseline    string  `json:"baseline"`
	Scores      []Score `json:"scores"`
	RecallDelta float64 `json:"recall_delta"`
	MRRDelta    float64 `json:"mrr_delta"`
	Direction   string  `json:"direction"` // up, down, flat, baseline
}

// FixtureDocument 是夹具里的一篇原文。
type FixtureDocument struct {
	ID   model.DocumentID `json:"id"`
	Text string           `json:"text"`
}

// FixtureCollection 是夹具里的一个集合。文档只属于这一个集合。
type FixtureCollection struct {
	ID        model.CollectionID `json:"id"`
	Documents []FixtureDocument  `json:"documents"`
}

// Fixture 是一份召回夹具。JSON 字段使用 snake_case。
type Fixture struct {
	K                 int                 `json:"k"`
	Thresholds        Thresholds          `json:"thresholds"`
	ContrastDropTerms []string            `json:"contrast_drop_terms"`
	Collections       []FixtureCollection `json:"collections"`
	Questions         []Question          `json:"questions"`
}

// Load 读取 path 上的夹具 JSON。
// 文件不存在、JSON 无法解析，或出现 Fixture 没有的字段时返回错误。
func Load(path string) (Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var fx Fixture
	if err := dec.Decode(&fx); err != nil {
		return Fixture{}, err
	}
	if dec.More() {
		return Fixture{}, errors.New("eval: trailing json")
	}
	return fx, nil
}

// Run 对题目打分。ranker 返回去重前的文档编号，相关文档按出现顺序；Run 自己去重保序。
// 只含一个集合的题计入该集合；多个集合的题只计入 collection_id "merged"。
// baseline 为空字符串时 Direction 为 "baseline"，delta 为 0。
// baseline 非空时只记下这个名字，不在这里比较另一份报告，delta 仍为 0，Direction 为 flat。
func Run(ctx context.Context, fx Fixture, ranker func(context.Context, Question) ([]model.DocumentID, error), splitterName, baseline string) (Report, error) {
	if ranker == nil {
		return Report{}, errors.New("eval: nil ranker")
	}
	hits := make([][]retrieve.Hit, len(fx.Questions))
	for i, q := range fx.Questions {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		ids, err := ranker(ctx, q)
		if err != nil {
			return Report{}, err
		}
		ids = dedupeIDs(ids)
		row := make([]retrieve.Hit, len(ids))
		for j, id := range ids {
			row[j] = retrieve.Hit{DocumentID: id}
		}
		hits[i] = row
	}
	recall, err := RecallAtK(fx.Questions, hits, fx.K)
	if err != nil {
		return Report{}, err
	}
	mrr, err := MeanReciprocalRank(fx.Questions, hits)
	if err != nil {
		return Report{}, err
	}
	mrrByID := make(map[string]float64, len(mrr))
	for _, s := range mrr {
		mrrByID[s.CollectionID] = s.MRR
	}
	scores := make([]Score, len(recall))
	for i, s := range recall {
		s.MRR = mrrByID[s.CollectionID]
		scores[i] = s
	}
	report := Report{
		Splitter: splitterName,
		Baseline: baseline,
		Scores:   scores,
	}
	if baseline == "" {
		report.Direction = "baseline"
	} else {
		report.Direction = "flat"
	}
	return report, nil
}

// Compare 用候选的 merged Recall 减去基线的 merged Recall。
// 某一份报告没有 merged 时，用该报告全部分数的宏平均代替。
// MRRDelta 使用同样的规则。Direction：delta>0 为 up，<0 为 down，否则 flat。
// 返回的报告保留候选的切分名称和分数，Baseline 记下基线的切分名称。
func Compare(baseline, candidate Report) Report {
	out := candidate
	if baseline.Splitter != "" {
		out.Baseline = baseline.Splitter
	} else {
		out.Baseline = baseline.Baseline
	}
	out.RecallDelta = metric(candidate, true) - metric(baseline, true)
	out.MRRDelta = metric(candidate, false) - metric(baseline, false)
	switch {
	case out.RecallDelta > 0:
		out.Direction = "up"
	case out.RecallDelta < 0:
		out.Direction = "down"
	default:
		out.Direction = "flat"
	}
	return out
}

func metric(r Report, recall bool) float64 {
	for _, s := range r.Scores {
		if s.CollectionID == "merged" {
			if recall {
				return s.RecallAtK
			}
			return s.MRR
		}
	}
	if len(r.Scores) == 0 {
		return 0
	}
	var sum float64
	for _, s := range r.Scores {
		if recall {
			sum += s.RecallAtK
		} else {
			sum += s.MRR
		}
	}
	return sum / float64(len(r.Scores))
}

func dedupeIDs(ids []model.DocumentID) []model.DocumentID {
	seen := make(map[model.DocumentID]struct{}, len(ids))
	out := make([]model.DocumentID, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// RecallAtK 计算 Recall@K 的题目宏平均。
// hits 与 questions 必须等长，否则返回错误，不返回部分分数。
// hits[i] 是第 i 题已经排好的命中，靠前者名次靠前；本函数不按 Score 再排序。
// 每题先按 DocumentID 去重，只保留第一次出现。
// 至少一篇相关文档出现在前 k 名则该题记 1，否则记 0。k 小于等于 0 或没有相关文档时该题记 0。
// 只含一个集合的题计入该集合。含多个集合的题计入 collection_id 为 merged。
// 没有点名集合的题返回错误。桶内分数是该桶题目的算术平均。
// 桶的先后与该桶在题目里第一次出现的顺序相同。
// 返回的 Score 填写 CollectionID、RecallAtK 和 K，MRR 为 0。
// 没有可计入的题时返回空切片。
func RecallAtK(questions []Question, hits [][]retrieve.Hit, k int) ([]Score, error) {
	avgs, err := average(questions, hits, func(q Question, docs []model.DocumentID) float64 {
		return binaryRecall(q.RelevantDocs, docs, k)
	})
	if err != nil {
		return nil, err
	}
	scores := make([]Score, 0, len(avgs))
	for _, avg := range avgs {
		scores = append(scores, Score{
			CollectionID: avg.id,
			RecallAtK:    avg.value,
			K:            k,
		})
	}
	return scores, nil
}

// MeanReciprocalRank 计算 MRR 的题目宏平均。
// 排名规则、去重和集合分桶与 RecallAtK 相同。
// 每题的分数是第一个相关文档名次的倒数，名次从 1 开始。没有命中时该题记 0。
// 返回的 Score 填写 CollectionID 和 MRR。RecallAtK 为 0，K 为 0。
// hits 与 questions 长度不一致，或题目没有点名集合时返回错误。
func MeanReciprocalRank(questions []Question, hits [][]retrieve.Hit) ([]Score, error) {
	avgs, err := average(questions, hits, func(q Question, docs []model.DocumentID) float64 {
		return reciprocalRank(q.RelevantDocs, docs)
	})
	if err != nil {
		return nil, err
	}
	scores := make([]Score, 0, len(avgs))
	for _, avg := range avgs {
		scores = append(scores, Score{
			CollectionID: avg.id,
			MRR:          avg.value,
		})
	}
	return scores, nil
}

type bucketAvg struct {
	id    string
	value float64
}

func average(questions []Question, hits [][]retrieve.Hit, score func(Question, []model.DocumentID) float64) ([]bucketAvg, error) {
	if len(questions) != len(hits) {
		return nil, fmt.Errorf("eval: %d questions and %d rankings", len(questions), len(hits))
	}
	type acc struct {
		sum float64
		n   int
	}
	order := make([]string, 0)
	groups := make(map[string]*acc)
	for i, q := range questions {
		id, err := bucket(q)
		if err != nil {
			return nil, err
		}
		group, ok := groups[id]
		if !ok {
			group = &acc{}
			groups[id] = group
			order = append(order, id)
		}
		group.sum += score(q, documentRank(hits[i]))
		group.n++
	}
	out := make([]bucketAvg, 0, len(order))
	for _, id := range order {
		group := groups[id]
		out = append(out, bucketAvg{id: id, value: group.sum / float64(group.n)})
	}
	return out, nil
}

func bucket(q Question) (string, error) {
	switch len(q.Collections) {
	case 0:
		return "", fmt.Errorf("eval: question %s has no collection", q.ID)
	case 1:
		return string(q.Collections[0]), nil
	default:
		return "merged", nil
	}
}

func documentRank(hits []retrieve.Hit) []model.DocumentID {
	seen := make(map[model.DocumentID]struct{}, len(hits))
	docs := make([]model.DocumentID, 0, len(hits))
	for _, hit := range hits {
		if _, ok := seen[hit.DocumentID]; ok {
			continue
		}
		seen[hit.DocumentID] = struct{}{}
		docs = append(docs, hit.DocumentID)
	}
	return docs
}

func binaryRecall(relevant []model.DocumentID, docs []model.DocumentID, k int) float64 {
	if k <= 0 || len(relevant) == 0 {
		return 0
	}
	rel := idSet(relevant)
	limit := k
	if limit > len(docs) {
		limit = len(docs)
	}
	for i := 0; i < limit; i++ {
		if _, ok := rel[docs[i]]; ok {
			return 1
		}
	}
	return 0
}

func reciprocalRank(relevant []model.DocumentID, docs []model.DocumentID) float64 {
	if len(relevant) == 0 {
		return 0
	}
	rel := idSet(relevant)
	for i, id := range docs {
		if _, ok := rel[id]; ok {
			return 1 / float64(i+1)
		}
	}
	return 0
}

func idSet(ids []model.DocumentID) map[model.DocumentID]struct{} {
	set := make(map[model.DocumentID]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}
