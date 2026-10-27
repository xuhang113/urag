package job

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
)

func (p *Pipeline) searchNamed(ctx context.Context, reg index.Registry, req retrieve.Request) ([]retrieve.Hit, error) {
	if reg == nil {
		return nil, errMissingDeps
	}
	vecs, err := p.deps.Embedder.Embed(ctx, []string{req.Query})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("job: embedder returned %d vectors for the query", len(vecs))
	}
	tokens := tokenize(req.Query)
	top := req.TopK
	if top <= 0 {
		top = 5
	}
	var hits []retrieve.Hit
	for _, col := range req.Collections {
		idx, err := reg.Open(ctx, col)
		if err != nil {
			return nil, err
		}
		q := index.Query{
			CollectionID: col,
			Vector:       vecs[0],
			Tokens:       tokens,
			TopK:         1 << 20,
		}
		vecHits, err := idx.SearchVector(ctx, q)
		if err != nil {
			return nil, err
		}
		var kwHits []index.Hit
		if req.Hybrid {
			kwHits, err = idx.SearchKeyword(ctx, q)
			if err != nil {
				return nil, err
			}
		}
		mixed, err := mixHits(col, vecHits, kwHits, p.cfg.Weights, req.Hybrid)
		if err != nil {
			return nil, err
		}
		hits = append(hits, mixed...)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			if hits[i].CollectionID == hits[j].CollectionID {
				return hits[i].ChunkID < hits[j].ChunkID
			}
			return hits[i].CollectionID < hits[j].CollectionID
		}
		return hits[i].Score > hits[j].Score
	})
	kept := hits[:0]
	for _, hit := range hits {
		if hit.Score > 0 {
			kept = append(kept, hit)
		}
	}
	hits = kept
	if req.Rerank && p.deps.Reranker != nil {
		ranked, err := p.deps.Reranker.Rerank(ctx, req.Query, hits)
		if err != nil {
			return nil, err
		}
		hits = ranked
	}
	if len(hits) > top {
		hits = hits[:top]
	}
	if hits == nil {
		return []retrieve.Hit{}, nil
	}
	return hits, nil
}

func mixHits(col model.CollectionID, vecHits, kwHits []index.Hit, w retrieve.Weights, hybrid bool) ([]retrieve.Hit, error) {
	type acc struct {
		hit retrieve.Hit
		vec float64
		kw  float64
	}
	order := make([]model.ChunkID, 0, len(vecHits)+len(kwHits))
	byID := map[model.ChunkID]*acc{}
	add := func(h index.Hit, vec, kw float64) error {
		if h.CollectionID != col {
			return fmt.Errorf("job: index returned collection %s for %s", h.CollectionID, col)
		}
		item, ok := byID[h.Chunk.ID]
		if !ok {
			item = &acc{hit: retrieve.Hit{
				CollectionID: col,
				DocumentID:   h.Chunk.DocumentID,
				ChunkID:      h.Chunk.ID,
				Text:         h.Chunk.Text,
				StartByte:    h.Chunk.StartByte,
				EndByte:      h.Chunk.EndByte,
			}}
			byID[h.Chunk.ID] = item
			order = append(order, h.Chunk.ID)
		}
		if vec > item.vec {
			item.vec = vec
		}
		if kw > item.kw {
			item.kw = kw
		}
		return nil
	}
	for _, hit := range vecHits {
		if err := add(hit, hit.Score, 0); err != nil {
			return nil, err
		}
	}
	for _, hit := range kwHits {
		if err := add(hit, 0, hit.Score); err != nil {
			return nil, err
		}
	}
	out := make([]retrieve.Hit, 0, len(order))
	for _, id := range order {
		item := byID[id]
		if hybrid {
			item.hit.Score = w.Vector*item.vec + w.Keyword*item.kw
		} else {
			item.hit.Score = item.vec
		}
		out = append(out, item.hit)
	}
	return out, nil
}

func contrastChunks(doc model.Document, terms []string) []model.Chunk {
	text := doc.Text
	drop := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		term = strings.ToLower(strings.TrimSpace(term))
		if term != "" {
			drop[term] = struct{}{}
		}
	}
	dropped := make([]bool, len(text))
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if size <= 0 {
			break
		}
		if unicode.IsLetter(r) {
			j := i
			for j < len(text) {
				rr, sz := utf8.DecodeRuneInString(text[j:])
				if sz <= 0 || !unicode.IsLetter(rr) {
					break
				}
				j += sz
			}
			if _, ok := drop[strings.ToLower(text[i:j])]; ok {
				for k := i; k < j; k++ {
					dropped[k] = true
				}
			}
			i = j
			continue
		}
		i += size
	}
	var chunks []model.Chunk
	ordinal := 0
	emit := func(start, end int) {
		for s := start; s < end; {
			e := s
			for e < end {
				_, sz := utf8.DecodeRuneInString(text[e:])
				if sz <= 0 {
					break
				}
				if e > s && e+sz-s > 20 {
					break
				}
				e += sz
				if e-s >= 20 {
					break
				}
			}
			if e == s {
				return
			}
			piece := text[s:e]
			if strings.TrimSpace(piece) != "" {
				chunks = append(chunks, model.Chunk{
					ID:           model.ChunkID(fmtChunkID(doc.ID, ordinal)),
					DocumentID:   doc.ID,
					CollectionID: doc.CollectionID,
					Ordinal:      ordinal,
					Text:         piece,
					StartByte:    s,
					EndByte:      e,
				})
				ordinal++
			}
			s = e
		}
	}
	start := -1
	for i := 0; i < len(text); i++ {
		if dropped[i] {
			if start >= 0 {
				emit(start, i)
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		emit(start, len(text))
	}
	return chunks
}

func fmtChunkID(id model.DocumentID, ordinal int) string {
	return string(id) + "#" + itoa(ordinal)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func tokenize(text string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		out = append(out, strings.ToLower(b.String()))
		b.Reset()
	}
	for _, r := range text {
		if unicode.IsLetter(r) {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}
