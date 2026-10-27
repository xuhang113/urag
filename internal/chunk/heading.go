package chunk

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/xuhang113/urag/internal/model"
)

const (
	// 字数是不含空白的 Unicode 字符。800 以内保持整节，再长才按段落切开。
	headingMaxChars = 800
	headingMinChars = 400
)

// Section 是按标题划出的父块。父块不进入索引，台账从这里读取。
// Children 是这一节的子块，和 NewHeading.Split 里的片段是同一批。
// 页码未知时 PageFrom 和 PageTo 为 0。现有 PDF 抽取看不到页界，这里不补造。
type Section struct {
	HeadingPath string
	Body        string
	PageFrom    int
	PageTo      int
	Children    []model.Chunk
}

// Passage 拼出待嵌入的文本。本包不嵌入。
// headingPath 为空时只返回正文。
func Passage(headingPath, body string) string {
	if headingPath == "" {
		return body
	}
	return headingPath + "\n" + body
}

type headingSplitter struct{}

// NewHeading 返回按 markdown 标题分节的切法，名称是 heading。
// 输入是清洗后的 markdown：标题行用 # 到 ######，段落之间空行。
// 字数按 Unicode 字符计，不计空白。不超过 800 字的节只有一个子块。
// 更长的节只在空行段落边界切开，每块大约 400 到 800 字。
// 相邻子块重叠大约 10% 到 15%，重叠也落在段落边界上。
// 单个段落超过 800 字时不从段落中间切开。
// Options 按字节控制 paragraph 切法，heading 不用那两个字段。
func NewHeading() Splitter { return headingSplitter{} }

func (headingSplitter) Name() string { return "heading" }

func (headingSplitter) Split(doc model.Document, _ Options) ([]model.Chunk, error) {
	_, flat, err := splitHeading(doc)
	return flat, err
}

// SplitSections 按标题切出父块。子块顺序与 NewHeading.Split 相同。
func SplitSections(doc model.Document) ([]Section, error) {
	sections, _, err := splitHeading(doc)
	return sections, err
}

func splitHeading(doc model.Document) ([]Section, []model.Chunk, error) {
	raws := markdownSections(doc.Text)
	var sections []Section
	var flat []model.Chunk
	ordinal := 0
	for _, raw := range raws {
		sec := Section{
			HeadingPath: raw.path,
			Body:        raw.body,
			PageFrom:    0,
			PageTo:      0,
		}
		for _, sp := range windowsFor(raw.body) {
			start := raw.start + sp.start
			end := raw.start + sp.end
			if start < 0 || end > len(doc.Text) || start > end {
				return nil, nil, fmt.Errorf("chunk: heading range %d:%d", start, end)
			}
			ch := model.Chunk{
				ID:           model.ChunkID(fmt.Sprintf("%s#%d", doc.ID, ordinal)),
				DocumentID:   doc.ID,
				CollectionID: doc.CollectionID,
				Ordinal:      ordinal,
				Text:         doc.Text[start:end],
				StartByte:    start,
				EndByte:      end,
			}
			sec.Children = append(sec.Children, ch)
			flat = append(flat, ch)
			ordinal++
		}
		sections = append(sections, sec)
	}
	return sections, flat, nil
}

type rawSection struct {
	path  string
	body  string
	start int
	end   int
}

type headingNode struct {
	level int
	text  string
}

type mdLine struct {
	text  string
	start int
	end   int
}

func markdownSections(text string) []rawSection {
	lines := scanLines(text)
	var stack []headingNode
	var out []rawSection
	path := ""
	contentStart, contentEnd := 0, 0
	hasContent := false
	active := true

	flush := func() {
		if !active {
			return
		}
		body := ""
		start, end := contentStart, contentStart
		if hasContent {
			body = text[contentStart:contentEnd]
			start, end = contentStart, contentEnd
		}
		active = false
		hasContent = false
		if path == "" && body == "" {
			return
		}
		out = append(out, rawSection{path: path, body: body, start: start, end: end})
	}

	for _, ln := range lines {
		if level, title, ok := parseHeading(ln.text); ok {
			flush()
			stack, path = pushHeading(stack, level, title)
			active = true
			hasContent = false
			continue
		}
		// 只剥掉标题前后的空行。段落末尾的空格仍是正文。
		if isBlank(ln.text) {
			continue
		}
		if !hasContent {
			contentStart = ln.start
			hasContent = true
		}
		contentEnd = ln.end
	}
	flush()
	return out
}

func scanLines(text string) []mdLine {
	var lines []mdLine
	i := 0
	for i < len(text) {
		start := i
		for i < len(text) && text[i] != '\n' {
			i++
		}
		end := i
		if end > start && text[end-1] == '\r' {
			end--
		}
		if i < len(text) && text[i] == '\n' {
			i++
		}
		lines = append(lines, mdLine{text: text[start:end], start: start, end: end})
	}
	return lines
}

func parseHeading(line string) (int, string, bool) {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	if i > 3 || i >= len(line) || line[i] != '#' {
		return 0, "", false
	}
	hashes := 0
	for i < len(line) && line[i] == '#' {
		hashes++
		i++
	}
	if hashes < 1 || hashes > 6 || i >= len(line) || (line[i] != ' ' && line[i] != '\t') {
		return 0, "", false
	}
	title := strings.TrimSpace(line[i:])
	if title == "" {
		return 0, "", false
	}
	return hashes, title, true
}

func pushHeading(stack []headingNode, level int, text string) ([]headingNode, string) {
	for len(stack) > 0 && stack[len(stack)-1].level >= level {
		stack = stack[:len(stack)-1]
	}
	stack = append(stack, headingNode{level: level, text: text})
	parts := make([]string, len(stack))
	for i, n := range stack {
		parts[i] = n.text
	}
	return stack, strings.Join(parts, " / ")
}

func windowsFor(body string) []span {
	if body == "" {
		return nil
	}
	if nonWS(body) <= headingMaxChars {
		return []span{{0, len(body)}}
	}
	paras := paragraphSpans(body)
	if len(paras) <= 1 {
		return []span{{0, len(body)}}
	}
	counts := make([]int, len(paras))
	for i, sp := range paras {
		counts[i] = nonWS(body[sp.start:sp.end])
	}
	ranges := packHeading(counts)
	if len(ranges) == 0 {
		return []span{{0, len(body)}}
	}
	out := make([]span, 0, len(ranges))
	for _, rg := range ranges {
		if rg[0] < 0 || rg[1] > len(paras) || rg[0] >= rg[1] {
			continue
		}
		out = append(out, span{paras[rg[0]].start, paras[rg[1]-1].end})
	}
	if len(out) == 0 {
		return []span{{0, len(body)}}
	}
	return out
}

func nonWS(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

func packHeading(counts []int) [][2]int {
	n := len(counts)
	if n == 0 {
		return nil
	}
	prefix := make([]int, n+1)
	for i, c := range counts {
		prefix[i+1] = prefix[i] + c
	}
	sum := func(a, b int) int { return prefix[b] - prefix[a] }
	var out [][2]int
	i := 0
	for step := 0; i < n && step < n+2; step++ {
		if sum(i, n) <= headingMaxChars || n-i == 1 {
			out = append(out, [2]int{i, n})
			break
		}
		j, k := chooseCut(i, n, sum)
		if j <= i {
			j = i + 1
		}
		if j > n {
			j = n
		}
		out = append(out, [2]int{i, j})
		if j >= n {
			break
		}
		next := k
		if next <= i || next >= j {
			next = j
		}
		i = next
	}
	return out
}

func chooseCut(i, n int, sum func(int, int) int) (int, int) {
	maxJ := i
	for j := i + 1; j <= n; j++ {
		cur := sum(i, j)
		if j > i+1 && cur > headingMaxChars {
			break
		}
		maxJ = j
	}
	bestJ, bestK := -1, -1
	var bestScore int64
	consider := func(j, k int, relaxed bool) {
		if j <= i || j >= n || k <= i || k >= j {
			return
		}
		cur := sum(i, j)
		ov := sum(k, j)
		if cur <= 0 || ov <= 0 {
			return
		}
		if cur > headingMaxChars && j != i+1 {
			return
		}
		if relaxed {
			if !overlapNear(ov, cur) {
				return
			}
		} else if !overlapInBand(ov, cur) {
			return
		}
		score := ratioDistance(ov, cur)
		if cur < headingMinChars {
			score += int64(headingMinChars-cur)*1000 + 40_000
		}
		if cur > headingMaxChars {
			score += int64(cur-headingMaxChars)*1000 + 40_000
		}
		nextAll := sum(k, n)
		if nextAll < headingMinChars {
			score += int64(headingMinChars-nextAll)*1000 + 80_000
		} else if nextAll > headingMaxChars {
			score += 5_000
		}
		score += int64(headingMaxChars - min(cur, headingMaxChars))
		if bestJ < 0 || score < bestScore {
			bestJ, bestK, bestScore = j, k, score
		}
	}
	for j := i + 1; j <= maxJ && j < n; j++ {
		for k := i + 1; k < j; k++ {
			consider(j, k, false)
		}
	}
	if bestJ < 0 {
		for j := i + 1; j <= maxJ && j < n; j++ {
			for k := i + 1; k < j; k++ {
				consider(j, k, true)
			}
		}
	}
	if bestJ < 0 {
		return fallbackCut(i, n, maxJ, sum)
	}
	return bestJ, bestK
}

func fallbackCut(i, n, maxJ int, sum func(int, int) int) (int, int) {
	j := maxJ
	if j <= i {
		j = i + 1
	}
	if j > n {
		j = n
	}
	if j == n && sum(i, n) > headingMaxChars && n > i+1 {
		j = i + 1
		for j+1 < n && sum(i, j+1) <= headingMaxChars {
			j++
		}
	}
	return j, j
}

func overlapInBand(ov, cur int) bool {
	if ov <= 0 || cur <= 0 {
		return false
	}
	o, c := int64(ov), int64(cur)
	return 10*o >= c && 20*o <= 3*c
}

func overlapNear(ov, cur int) bool {
	if ov <= 0 || cur <= 0 {
		return false
	}
	o, c := int64(ov), int64(cur)
	return 25*o >= 2*c && 5*o <= c
}

func ratioDistance(ov, cur int) int64 {
	diff := 8*int64(ov) - int64(cur)
	if diff < 0 {
		diff = -diff
	}
	return diff * 1000 / int64(cur)
}
