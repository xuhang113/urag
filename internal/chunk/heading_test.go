package chunk_test

import (
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/model"
)

func TestHeadingNamePassageAndEmpty(t *testing.T) {
	sp := chunk.NewHeading()
	if sp.Name() != "heading" {
		t.Fatalf("name %s", sp.Name())
	}
	if chunk.Passage("灯会 / 河岸", "正文") != "灯会 / 河岸\n正文" {
		t.Fatal("passage")
	}
	if chunk.Passage("", "正文") != "正文" || chunk.Passage("", "") != "" {
		t.Fatal("empty path")
	}
	for _, text := range []string{"", "\n\n", " \n\t"} {
		doc := model.Document{ID: "d", CollectionID: "c", Text: text}
		got, err := sp.Split(doc, chunk.Options{MaxLen: 10, Overlap: -1})
		if err != nil || len(got) != 0 {
			t.Fatalf("text %q got %#v err %v", text, got, err)
		}
		sections, err := chunk.SplitSections(doc)
		if err != nil || len(sections) != 0 {
			t.Fatalf("sections %q %#v err %v", text, sections, err)
		}
	}
}

func TestHeadingPathsAndShortSections(t *testing.T) {
	doc := model.Document{
		ID:           "doc",
		CollectionID: "col",
		Text: strings.Join([]string{
			"前言。",
			"# A",
			"甲。",
			"## B",
			"乙。",
			"### C",
			"丙。",
			"## D",
			"丁。",
			"# E",
			"戊。",
			"#### 深",
			"己。",
		}, "\n\n"),
	}
	sections, err := chunk.SplitSections(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := []chunk.Section{
		{HeadingPath: "", Body: "前言。"},
		{HeadingPath: "A", Body: "甲。"},
		{HeadingPath: "A / B", Body: "乙。"},
		{HeadingPath: "A / B / C", Body: "丙。"},
		{HeadingPath: "A / D", Body: "丁。"},
		{HeadingPath: "E", Body: "戊。"},
		{HeadingPath: "E / 深", Body: "己。"},
	}
	if len(sections) != len(want) {
		t.Fatalf("len %d", len(sections))
	}
	flat, err := chunk.NewHeading().Split(doc, chunk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var children []model.Chunk
	for i, sec := range sections {
		if sec.HeadingPath != want[i].HeadingPath || sec.Body != want[i].Body {
			t.Fatalf("section %d path %q body %q", i, sec.HeadingPath, sec.Body)
		}
		if sec.PageFrom != 0 || sec.PageTo != 0 {
			t.Fatalf("page %+v", sec)
		}
		if len(sec.Children) != 1 || sec.Children[0].Text != sec.Body {
			t.Fatalf("child %#v", sec.Children)
		}
		children = append(children, sec.Children...)
	}
	if !reflect.DeepEqual(flat, children) {
		t.Fatalf("split %#v", flat)
	}
	assertHeadingChunks(t, doc, flat)
	if chunk.Passage(sections[2].HeadingPath, sections[2].Body) != "A / B\n乙。" {
		t.Fatal("passage section")
	}
}

func TestHeadingNotMarkerWithoutSpace(t *testing.T) {
	doc := model.Document{ID: "d", CollectionID: "c", Text: "# 灯会\n\n#不是标题\n\n正文。"}
	sections, err := chunk.SplitSections(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0].HeadingPath != "灯会" {
		t.Fatalf("%#v", sections)
	}
	if sections[0].Body != "#不是标题\n\n正文。" || len(sections[0].Children) != 1 {
		t.Fatalf("body %q", sections[0].Body)
	}
}

func TestHeadingCRLF(t *testing.T) {
	doc := model.Document{ID: "d", CollectionID: "c", Text: "# 灯会\r\n\r\n正文。\r\n"}
	sections, err := chunk.SplitSections(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0].HeadingPath != "灯会" || sections[0].Body != "正文。" {
		t.Fatalf("%#v", sections)
	}
	ch := sections[0].Children[0]
	if doc.Text[ch.StartByte:ch.EndByte] != ch.Text || ch.Text != "正文。" {
		t.Fatalf("%#v", ch)
	}
}

func TestHeadingShortDespiteByteOptions(t *testing.T) {
	// 300 个非空白字符，字节数远超 10。字数规则仍把整节当成唯一子块。
	para := strings.Repeat("灯 ", 100)
	body := strings.Join([]string{para, para, para}, "\n\n")
	doc := model.Document{ID: "d", CollectionID: "c", Text: "# 灯会\n\n" + body}
	if countNonWS(body) != 300 {
		t.Fatalf("nonws %d", countNonWS(body))
	}
	got, err := chunk.NewHeading().Split(doc, chunk.Options{MaxLen: 10, Overlap: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != body || countNonWS(got[0].Text) != 300 {
		t.Fatalf("%#v", got)
	}
	sections, err := chunk.SplitSections(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0].HeadingPath != "灯会" || len(sections[0].Children) != 1 {
		t.Fatalf("%#v", sections)
	}
}

func TestHeadingDoesNotCutParagraph(t *testing.T) {
	body := strings.Repeat("灯", 1000)
	doc := model.Document{ID: "d", CollectionID: "c", Text: "# 灯会\n\n" + body}
	got, err := chunk.NewHeading().Split(doc, chunk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != body {
		t.Fatalf("len %d", len(got))
	}
	if countNonWS(got[0].Text) != 1000 {
		t.Fatalf("nonws %d", countNonWS(got[0].Text))
	}
}

func TestHeadingLongOverlap(t *testing.T) {
	paras := make([]string, 10)
	for i := range paras {
		paras[i] = strings.Repeat("灯", 100)
	}
	body := strings.Join(paras, "\n\n")
	doc := model.Document{ID: "doc", CollectionID: "col", Text: "# 灯会\n\n" + body + "\n\n# 尾声\n\n结束。"}
	sections, err := chunk.SplitSections(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 2 || sections[0].HeadingPath != "灯会" || sections[1].HeadingPath != "尾声" {
		t.Fatalf("paths %#v", sections)
	}
	if sections[0].Body != body || sections[1].Body != "结束。" {
		t.Fatalf("bodies %q %q", sections[0].Body, sections[1].Body)
	}
	if len(sections[1].Children) != 1 || sections[1].Children[0].Text != "结束。" {
		t.Fatalf("tail %#v", sections[1].Children)
	}
	kids := sections[0].Children
	if len(kids) != 2 {
		t.Fatalf("kids %d", len(kids))
	}
	want0 := strings.Join(paras[:7], "\n\n")
	want1 := strings.Join(paras[6:], "\n\n")
	if kids[0].Text != want0 || kids[1].Text != want1 {
		t.Fatalf("texts %d %d", countNonWS(kids[0].Text), countNonWS(kids[1].Text))
	}
	if countNonWS(kids[0].Text) != 700 || countNonWS(kids[1].Text) != 400 {
		t.Fatalf("counts %d %d", countNonWS(kids[0].Text), countNonWS(kids[1].Text))
	}
	// 段落内容相同，按字符串找最长重叠会把多段都算进去。窗口的交集就是第 6 段。
	ov := paras[6]
	if !strings.HasSuffix(kids[0].Text, ov) || !strings.HasPrefix(kids[1].Text, ov) {
		t.Fatal("overlap missed the shared paragraph")
	}
	ratio := float64(countNonWS(ov)) / float64(countNonWS(kids[0].Text))
	if ratio < 0.10 || ratio > 0.15 {
		t.Fatalf("ratio %f overlap %d", ratio, countNonWS(ov))
	}
	for _, kid := range kids {
		if strings.Contains(kid.Text, "#") {
			t.Fatalf("heading leaked %q", kid.Text)
		}
		parts := strings.Split(kid.Text, "\n\n")
		for _, part := range parts {
			if part != paras[0] {
				t.Fatalf("partial paragraph %d", utf8.RuneCountInString(part))
			}
		}
	}

	flat, err := chunk.NewHeading().Split(doc, chunk.Options{MaxLen: 30, Overlap: 100})
	if err != nil {
		t.Fatal(err)
	}
	var joined []model.Chunk
	joined = append(joined, sections[0].Children...)
	joined = append(joined, sections[1].Children...)
	if !reflect.DeepEqual(flat, joined) {
		t.Fatalf("flat %d joined %d", len(flat), len(joined))
	}
	assertHeadingChunks(t, doc, flat)
	if flat[0].Ordinal != 0 || flat[len(flat)-1].Ordinal != len(flat)-1 {
		t.Fatal("ordinal")
	}
	if flat[2].Text != "结束。" || flat[2].ID != "doc#2" {
		t.Fatalf("tail chunk %#v", flat[2])
	}
}

func TestHeadingWhitespaceNotCounted(t *testing.T) {
	paras := make([]string, 10)
	for i := range paras {
		paras[i] = strings.Repeat("灯 ", 100)
	}
	body := strings.Join(paras, "\n\n")
	if countNonWS(body) != 1000 {
		t.Fatalf("nonws %d", countNonWS(body))
	}
	doc := model.Document{ID: "d", CollectionID: "c", Text: "# 灯会\n\n" + body}
	sections, err := chunk.SplitSections(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || len(sections[0].Children) != 2 {
		t.Fatalf("%d children", len(sections[0].Children))
	}
	if countNonWS(sections[0].Children[0].Text) != 700 || countNonWS(sections[0].Children[1].Text) != 400 {
		t.Fatalf("counts %d %d", countNonWS(sections[0].Children[0].Text), countNonWS(sections[0].Children[1].Text))
	}
}

func TestHeadingOversizeParagraphs(t *testing.T) {
	p1 := strings.Repeat("甲", 900)
	p2 := strings.Repeat("乙", 100)
	doc := model.Document{ID: "d", CollectionID: "c", Text: "# 长\n\n" + p1 + "\n\n" + p2}
	got, err := chunk.NewHeading().Split(doc, chunk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != p1 || got[1].Text != p2 {
		t.Fatalf("%#v", got)
	}
	if sharedEdge(got[0].Text, got[1].Text) != "" {
		t.Fatal("unexpected overlap")
	}

	q1 := strings.Repeat("甲", 400)
	q2 := strings.Repeat("乙", 401)
	doc = model.Document{ID: "d", CollectionID: "c", Text: "# 长\n\n" + q1 + "\n\n" + q2}
	got, err = chunk.NewHeading().Split(doc, chunk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != q1 || got[1].Text != q2 {
		t.Fatalf("boundary %#v", got)
	}
}

func TestHeadingFitsInOneChunk(t *testing.T) {
	paras := make([]string, 8)
	for i := range paras {
		paras[i] = strings.Repeat("灯", 100)
	}
	body := strings.Join(paras, "\n\n")
	if countNonWS(body) != 800 {
		t.Fatalf("nonws %d", countNonWS(body))
	}
	doc := model.Document{ID: "d", CollectionID: "c", Text: "# 灯会\n\n" + body}
	got, err := chunk.NewHeading().Split(doc, chunk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != body {
		t.Fatalf("len %d", len(got))
	}
}

func TestHeadingVariedParagraphs(t *testing.T) {
	const n = 40
	paras := make([]string, n)
	for i := range paras {
		paras[i] = twoDigit(i) + strings.Repeat("灯", 28)
		if countNonWS(paras[i]) != 30 {
			t.Fatalf("para %d nonws %d", i, countNonWS(paras[i]))
		}
	}
	body := strings.Join(paras, "\n\n")
	doc := model.Document{ID: "d", CollectionID: "c", Text: "# 灯会\n\n" + body}
	sections, err := chunk.SplitSections(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 1 || sections[0].HeadingPath != "灯会" {
		t.Fatalf("%#v", sections)
	}
	assertParagraphWindows(t, paras, sections[0].Children)
}

func assertHeadingChunks(t *testing.T, doc model.Document, chunks []model.Chunk) {
	t.Helper()
	for i, ch := range chunks {
		if ch.StartByte < 0 || ch.EndByte > len(doc.Text) || ch.StartByte > ch.EndByte {
			t.Fatalf("range %+v", ch)
		}
		if !strings.Contains(doc.Text[ch.StartByte:ch.EndByte], ch.Text) {
			t.Fatalf("slice does not contain %q", ch.Text)
		}
		if !utf8.ValidString(ch.Text) {
			t.Fatal("utf-8")
		}
		if ch.Ordinal != i || ch.DocumentID != doc.ID || ch.CollectionID != doc.CollectionID {
			t.Fatalf("owner %+v", ch)
		}
		wantID := model.ChunkID(string(doc.ID) + "#" + itoa(i))
		if ch.ID != wantID {
			t.Fatalf("id %s", ch.ID)
		}
	}
}

func assertParagraphWindows(t *testing.T, paras []string, chunks []model.Chunk) {
	t.Helper()
	if len(chunks) < 2 {
		t.Fatalf("expected split, got %d", len(chunks))
	}
	seen := make([]int, len(paras))
	var prev []string
	for i, ch := range chunks {
		parts := strings.Split(ch.Text, "\n\n")
		non := countNonWS(ch.Text)
		if len(parts) > 1 && (non < 400 || non > 800) {
			t.Fatalf("chunk %d nonws %d", i, non)
		}
		for _, part := range parts {
			idx := indexPara(paras, part)
			if idx < 0 {
				t.Fatalf("unknown paragraph %q", part)
			}
			seen[idx]++
		}
		if i > 0 {
			ov := overlapParas(prev, parts)
			prevNon := countNonWS(chunks[i-1].Text)
			if len(ov) == 0 {
				t.Fatalf("chunk %d has no paragraph overlap", i)
			}
			ovText := strings.Join(ov, "\n\n")
			ratio := float64(countNonWS(ovText)) / float64(prevNon)
			if ratio < 0.10 || ratio > 0.15 {
				t.Fatalf("chunk %d ratio %f", i, ratio)
			}
		}
		prev = parts
	}
	for i, n := range seen {
		if n == 0 {
			t.Fatalf("paragraph %d missing", i)
		}
	}
}

func overlapParas(prev, next []string) []string {
	max := len(prev)
	if len(next) < max {
		max = len(next)
	}
	for n := max; n > 0; n-- {
		if reflect.DeepEqual(prev[len(prev)-n:], next[:n]) {
			return next[:n]
		}
	}
	return nil
}

func indexPara(paras []string, part string) int {
	for i, p := range paras {
		if p == part {
			return i
		}
	}
	return -1
}

func sharedEdge(a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	max := len(a)
	if len(b) < max {
		max = len(b)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(a, b[:n]) && strings.HasPrefix(b, b[:n]) {
			return b[:n]
		}
	}
	return ""
}

func countNonWS(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

func twoDigit(n int) string {
	const digits = "0123456789"
	return string([]byte{digits[n/10], digits[n%10]})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
