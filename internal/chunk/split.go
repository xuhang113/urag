package chunk

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/xuhang113/urag/internal/model"
)

type span struct {
	start int
	end   int
}

type runeAt struct {
	r   rune
	off int
}

func (paragraphSplitter) Split(doc model.Document, opts Options) ([]model.Chunk, error) {
	maxLen, overlap := normalize(opts)
	parts := packParagraphs(doc.Text, paragraphSpans(doc.Text), maxLen, overlap)
	return chunksFrom(doc, parts), nil
}

func (s dropSplitter) Split(doc model.Document, _ Options) ([]model.Chunk, error) {
	var parts []span
	for _, sp := range keptSpans(doc.Text, s.drop) {
		parts = append(parts, splitMax(doc.Text, sp, 20)...)
	}
	return chunksFrom(doc, parts), nil
}

func normalize(opts Options) (maxLen, overlap int) {
	maxLen = opts.MaxLen
	if maxLen <= 0 {
		maxLen = 800
	}
	switch {
	case opts.Overlap < 0:
		overlap = 0
	case opts.Overlap == 0:
		overlap = 100
	default:
		overlap = opts.Overlap
	}
	if half := maxLen / 2; overlap > half {
		overlap = half
	}
	return maxLen, overlap
}

func paragraphSpans(text string) []span {
	var lines []span
	var blanks []bool
	i := 0
	for i < len(text) {
		start := i
		for i < len(text) && text[i] != '\n' {
			i++
		}
		contentEnd := i
		if contentEnd > start && text[contentEnd-1] == '\r' {
			contentEnd--
		}
		lines = append(lines, span{start, contentEnd})
		blanks = append(blanks, isBlank(text[start:contentEnd]))
		if i < len(text) && text[i] == '\n' {
			i++
		}
	}
	var paras []span
	curStart, curEnd := -1, -1
	flush := func() {
		if curStart >= 0 && curEnd > curStart {
			paras = append(paras, span{curStart, curEnd})
		}
		curStart, curEnd = -1, -1
	}
	for i, ln := range lines {
		if blanks[i] {
			flush()
			continue
		}
		if curStart < 0 {
			curStart = ln.start
		}
		curEnd = ln.end
	}
	flush()
	return paras
}

func isBlank(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func packParagraphs(text string, paras []span, maxLen, overlap int) []span {
	if len(paras) == 0 {
		return nil
	}
	units := make([]span, 0, len(paras))
	for _, p := range paras {
		if p.end-p.start > maxLen {
			units = append(units, slide(text, p, maxLen, overlap)...)
			continue
		}
		units = append(units, p)
	}
	var out []span
	for i := 0; i < len(units); {
		start := units[i].start
		end := units[i].end
		j := i
		for j+1 < len(units) && units[j+1].end-start <= maxLen {
			j++
			end = units[j].end
		}
		if len(out) > 0 && overlap > 0 {
			want := out[len(out)-1].end - overlap
			if want < start {
				if want < 0 {
					want = 0
				}
				snapped := snapStart(text, want, end, maxLen)
				if snapped < start && snapped < end && end-snapped <= maxLen {
					start = snapped
				}
			}
		}
		if end > start {
			out = append(out, span{start, end})
		}
		i = j + 1
	}
	return out
}

func slide(text string, sp span, maxLen, overlap int) []span {
	var out []span
	pos := sp.start
	for pos < sp.end {
		end := snapEnd(text, pos, pos+maxLen, sp.end)
		if end <= pos {
			break
		}
		out = append(out, span{pos, end})
		if end >= sp.end {
			break
		}
		next := end - overlap
		if next < sp.start {
			next = sp.start
		}
		next = snapStart(text, next, end, maxLen)
		if next <= pos {
			_, size := utf8.DecodeRuneInString(text[pos:])
			if size < 1 {
				size = 1
			}
			next = pos + size
			if next > sp.end {
				next = sp.end
			}
		}
		pos = next
	}
	return out
}

func snapEnd(s string, start, want, limit int) int {
	if want > limit {
		want = limit
	}
	if want <= start {
		return start
	}
	end := want
	if end < len(s) && !utf8.RuneStart(s[end]) {
		for end > start && !utf8.RuneStart(s[end]) {
			end--
		}
	}
	if end <= start {
		_, size := utf8.DecodeRuneInString(s[start:])
		if size < 1 {
			size = 1
		}
		end = start + size
		if end > limit {
			end = limit
		}
	}
	return end
}

func snapStart(s string, want, end, maxLen int) int {
	if want <= 0 {
		return 0
	}
	if want >= len(s) || want >= end {
		return want
	}
	if utf8.RuneStart(s[want]) {
		return want
	}
	down := want
	for down > 0 && !utf8.RuneStart(s[down]) {
		down--
	}
	if end-down <= maxLen {
		return down
	}
	up := want
	for up < end && up < len(s) && !utf8.RuneStart(s[up]) {
		up++
	}
	return up
}

func keptSpans(text string, terms []string) []span {
	folded := make([]string, 0, len(terms))
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		folded = append(folded, strings.ToLower(term))
	}
	sort.Slice(folded, func(i, j int) bool {
		return utf8.RuneCountInString(folded[i]) > utf8.RuneCountInString(folded[j])
	})
	rs := make([]runeAt, 0, len(text))
	for off := 0; off < len(text); {
		r, size := utf8.DecodeRuneInString(text[off:])
		if size < 1 {
			size = 1
			r = rune(text[off])
		}
		rs = append(rs, runeAt{r, off})
		off += size
	}
	isWord := func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsNumber(r)
	}
	var spans []span
	byteStart := 0
	for i := 0; i < len(rs); {
		matched := 0
		if i == 0 || !isWord(rs[i-1].r) {
			for _, term := range folded {
				n := matchTerm(rs, i, term)
				if n == 0 {
					continue
				}
				if i+n < len(rs) && isWord(rs[i+n].r) {
					continue
				}
				matched = n
				break
			}
		}
		if matched > 0 {
			if rs[i].off > byteStart {
				spans = append(spans, span{byteStart, rs[i].off})
			}
			i += matched
			if i < len(rs) {
				byteStart = rs[i].off
			} else {
				byteStart = len(text)
			}
			continue
		}
		i++
	}
	if byteStart < len(text) {
		spans = append(spans, span{byteStart, len(text)})
	}
	return spans
}

func matchTerm(rs []runeAt, i int, term string) int {
	tr := []rune(term)
	if i+len(tr) > len(rs) {
		return 0
	}
	var b strings.Builder
	for k := 0; k < len(tr); k++ {
		b.WriteRune(rs[i+k].r)
	}
	if !strings.EqualFold(b.String(), term) {
		return 0
	}
	return len(tr)
}

func splitMax(text string, sp span, max int) []span {
	if max < 1 {
		max = 1
	}
	var out []span
	i := sp.start
	for i < sp.end {
		end := i + max
		if end > sp.end {
			end = sp.end
		}
		if end < len(text) && end < sp.end && !utf8.RuneStart(text[end]) {
			for end > i && !utf8.RuneStart(text[end]) {
				end--
			}
		}
		if end <= i {
			_, size := utf8.DecodeRuneInString(text[i:])
			if size < 1 {
				size = 1
			}
			end = i + size
			if end > sp.end {
				end = sp.end
			}
		}
		if end <= i {
			break
		}
		out = append(out, span{i, end})
		i = end
	}
	return out
}

func chunksFrom(doc model.Document, parts []span) []model.Chunk {
	if len(parts) == 0 {
		return nil
	}
	out := make([]model.Chunk, len(parts))
	for i, sp := range parts {
		out[i] = model.Chunk{
			ID:           model.ChunkID(fmt.Sprintf("%s#%d", doc.ID, i)),
			DocumentID:   doc.ID,
			CollectionID: doc.CollectionID,
			Ordinal:      i,
			Text:         doc.Text[sp.start:sp.end],
			StartByte:    sp.start,
			EndByte:      sp.end,
		}
	}
	return out
}
