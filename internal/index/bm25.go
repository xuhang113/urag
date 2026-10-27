package index

import (
	"math"
	"strings"
)

const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// bm25Scores 计算 Okapi BM25。IDF 用 ln(1 + (N-df+0.5)/(df+0.5))，避免常见词出现负分。
// 查询词去重后各算一次。docs[i] 是该子块的词，重复词计入词频。
// 只使用调用方传来的这一个集合，不读取别的集合。
func bm25Scores(docs [][]string, query []string) []float64 {
	scores := make([]float64, len(docs))
	terms := uniqueTokens(query)
	if len(docs) == 0 || len(terms) == 0 {
		return scores
	}
	nDocs := float64(len(docs))
	df := make(map[string]int, len(terms))
	tfs := make([]map[string]int, len(docs))
	dls := make([]int, len(docs))
	var sum int
	for i, toks := range docs {
		tf := make(map[string]int, len(toks))
		for _, tok := range toks {
			if tf[tok] == 0 {
				df[tok]++
			}
			tf[tok]++
		}
		tfs[i] = tf
		dls[i] = len(toks)
		sum += len(toks)
	}
	if sum == 0 {
		return scores
	}
	avg := float64(sum) / nDocs
	idf := make(map[string]float64, len(terms))
	for _, term := range terms {
		n := float64(df[term])
		idf[term] = math.Log(1 + (nDocs-n+0.5)/(n+0.5))
	}
	for i := range docs {
		if dls[i] == 0 {
			continue
		}
		norm := 1 - bm25B + bm25B*float64(dls[i])/avg
		var score float64
		for _, term := range terms {
			tf := float64(tfs[i][term])
			if tf == 0 || idf[term] == 0 {
				continue
			}
			score += idf[term] * (tf * (bm25K1 + 1)) / (tf + bm25K1*norm)
		}
		scores[i] = score
	}
	return scores
}

func uniqueTokens(tokens []string) []string {
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		tok = strings.ToLower(tok)
		if tok == "" {
			continue
		}
		if _, ok := seen[tok]; ok {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	return out
}
