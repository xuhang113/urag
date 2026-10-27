package clean_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/clean"
	"github.com/xuhang113/urag/internal/model"
)

const pdfSentence = "The lantern festival lights the canal."

func TestDetectEncoding(t *testing.T) {
	const sentence = "河边有灯会。"
	raw, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(sentence))
	if err != nil {
		t.Fatalf("encode gbk: %v", err)
	}
	if utf8.Valid(raw) {
		t.Fatal("gbk fixture unexpectedly valid utf-8")
	}
	doc := mustClean(t, clean.New(newMemDedup()), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/lantern.txt",
		SourceURI:    "file://lantern.txt",
		Filename:     "lantern.txt",
		Body:         strings.NewReader(string(raw)),
	})
	if doc.Text != sentence {
		t.Fatalf("text %q", doc.Text)
	}
	if !utf8.ValidString(doc.Text) || !strings.Contains(doc.Text, "灯会") {
		t.Fatalf("text %q", doc.Text)
	}
	enc := strings.ToLower(doc.Lineage.Encoding)
	if !strings.Contains(enc, "gbk") && !strings.Contains(enc, "gb18030") {
		t.Fatalf("encoding %q", doc.Lineage.Encoding)
	}
	assertIdentity(t, doc, "alpha", "raw/lantern.txt", "file://lantern.txt", "txt")
	requireSteps(t, doc.Lineage.Steps)
	if !stepDetailHas(doc.Lineage.Steps, "detect", "gbk") && !stepDetailHas(doc.Lineage.Steps, "detect", "gb18030") {
		t.Fatalf("detect step %+v", doc.Lineage.Steps)
	}
}

func TestStripBoilerplate(t *testing.T) {
	body := `<!DOCTYPE html>
<html>
<head>
<title>page</title>
<style>.nav{color:red}</style>
<script>var secretNav = 1;</script>
</head>
<body>
<header id="site-header">站点页眉模板</header>
<nav>主导航模板</nav>
<div class="navigation">顶部导航条</div>
<div id="left-sidebar">侧栏模板</div>
<div class="main-menu">菜单模板</div>
<div class="page-footer">页脚链接模板</div>
<footer>版权页脚模板</footer>
<article>
<p>灯会在河岸举行。</p>
<p>这段正文提到 nav 这个词。</p>
</article>
</body>
</html>`
	doc := mustClean(t, clean.New(nil), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/page.html",
		SourceURI:    "https://example.test/page.html",
		Filename:     "page.html",
		Body:         strings.NewReader(body),
	})
	for _, keep := range []string{"灯会在河岸举行。", "这段正文提到 nav 这个词。"} {
		if !strings.Contains(doc.Text, keep) {
			t.Fatalf("missing %q in %q", keep, doc.Text)
		}
	}
	for _, drop := range []string{
		"站点页眉模板",
		"主导航模板",
		"顶部导航条",
		"侧栏模板",
		"菜单模板",
		"页脚链接模板",
		"版权页脚模板",
		"secretNav",
		".nav{color:red}",
	} {
		if strings.Contains(doc.Text, drop) {
			t.Fatalf("boilerplate %q still in %q", drop, doc.Text)
		}
	}
	assertIdentity(t, doc, "alpha", "raw/page.html", "https://example.test/page.html", "html")
	requireSteps(t, doc.Lineage.Steps)
	if doc.Lineage.Encoding == "" {
		t.Fatal("empty encoding")
	}
}

func TestDedupePointsAtExisting(t *testing.T) {
	dedup := newMemDedup()
	c := clean.New(dedup)
	body := "同一篇灯会记录。"
	first := mustClean(t, c, clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/a.txt",
		SourceURI:    "file://a.txt",
		Filename:     "a.txt",
		Body:         strings.NewReader(body),
	})
	second := mustClean(t, c, clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/b.txt",
		SourceURI:    "file://b.txt",
		Filename:     "b.txt",
		Body:         strings.NewReader(body),
	})
	other := mustClean(t, c, clean.Input{
		CollectionID: "beta",
		ObjectKey:    "raw/c.txt",
		SourceURI:    "file://c.txt",
		Filename:     "c.txt",
		Body:         strings.NewReader(body),
	})
	if first.Duplicate || first.Lineage.DuplicateOf != "" {
		t.Fatalf("first duplicate=%v of=%q", first.Duplicate, first.Lineage.DuplicateOf)
	}
	if !second.Duplicate || second.Lineage.DuplicateOf != first.ID {
		t.Fatalf("second duplicate=%v of=%q first=%q", second.Duplicate, second.Lineage.DuplicateOf, first.ID)
	}
	if other.Duplicate || other.Lineage.DuplicateOf != "" {
		t.Fatalf("other collection duplicate=%v of=%q", other.Duplicate, other.Lineage.DuplicateOf)
	}
	if second.Text != first.Text || other.Text != first.Text {
		t.Fatal("text changed")
	}
	assertIdentity(t, first, "alpha", "raw/a.txt", "file://a.txt", "txt")
	assertIdentity(t, second, "alpha", "raw/b.txt", "file://b.txt", "txt")
	assertIdentity(t, other, "beta", "raw/c.txt", "file://c.txt", "txt")
	if second.ID != first.ID {
		t.Fatalf("same collection content id %s vs %s", first.ID, second.ID)
	}
	if other.ID == first.ID {
		t.Fatalf("other collection reused id %s", other.ID)
	}
	requireSteps(t, second.Lineage.Steps)
	if second.Lineage.RawObjectKey != "raw/b.txt" {
		t.Fatalf("second key %s", second.Lineage.RawObjectKey)
	}
}

func TestLineageRoundTrip(t *testing.T) {
	const (
		key = model.ObjectKey("raw/objects/note.txt")
		uri = "file://note.txt"
		col = model.CollectionID("alpha")
	)
	body := "第一段灯会。\n\n第二段在河边，句子比较长一点用来跨过很小的窗口。\n\n第三段结束。"
	doc := mustClean(t, clean.New(newMemDedup()), clean.Input{
		CollectionID: col,
		ObjectKey:    key,
		SourceURI:    uri,
		Filename:     "note.txt",
		Body:         strings.NewReader(body),
	})
	if doc.Text != body {
		t.Fatalf("text %q", doc.Text)
	}
	assertIdentity(t, doc, col, key, uri, "txt")
	requireSteps(t, doc.Lineage.Steps)
	for _, step := range doc.Lineage.Steps {
		if step.Detail == doc.Text {
			t.Fatalf("step kept only final text: %+v", step)
		}
	}
	chunks, err := chunk.NewParagraph().Split(doc, chunk.Options{MaxLen: 30, Overlap: 6})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	ids := make([]model.ChunkID, len(chunks))
	for i, ch := range chunks {
		if ch.StartByte < 0 || ch.EndByte > len(doc.Text) || ch.StartByte > ch.EndByte {
			t.Fatalf("range %d:%d", ch.StartByte, ch.EndByte)
		}
		if doc.Text[ch.StartByte:ch.EndByte] != ch.Text {
			t.Fatalf("chunk %d text %q", i, ch.Text)
		}
		wantID := model.ChunkID(string(doc.ID) + "#" + strconv.Itoa(i))
		if ch.ID != wantID || ch.Ordinal != i {
			t.Fatalf("id %s ordinal %d", ch.ID, ch.Ordinal)
		}
		if ch.DocumentID != doc.ID || ch.CollectionID != doc.CollectionID {
			t.Fatalf("chunk owner %+v", ch)
		}
		ids[i] = ch.ID
	}
	if len(doc.Lineage.ChunkIDs) == 0 {
		doc.Lineage.ChunkIDs = ids
	}
	if len(doc.Lineage.ChunkIDs) != len(ids) {
		t.Fatalf("lineage chunks %v", doc.Lineage.ChunkIDs)
	}
	for i := range ids {
		if doc.Lineage.ChunkIDs[i] != ids[i] {
			t.Fatalf("lineage chunk %s want %s", doc.Lineage.ChunkIDs[i], ids[i])
		}
	}
	if doc.Lineage.RawObjectKey != key {
		t.Fatalf("key %s", doc.Lineage.RawObjectKey)
	}
}

func TestPDFVisibleSentence(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/clean/lantern.pdf")
	if err != nil {
		t.Fatalf("read pdf: %v", err)
	}
	doc := mustClean(t, clean.New(nil), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/lantern.pdf",
		SourceURI:    "file://lantern.pdf",
		Filename:     "lantern.pdf",
		Body:         strings.NewReader(string(raw)),
	})
	if !strings.Contains(doc.Text, pdfSentence) {
		t.Fatalf("pdf text %q", doc.Text)
	}
	assertIdentity(t, doc, "alpha", "raw/lantern.pdf", "file://lantern.pdf", "pdf")
	requireSteps(t, doc.Lineage.Steps)
}

func TestSniffHTMLAndPDF(t *testing.T) {
	htmlRaw := "<html><body><nav>主导航模板</nav><p>灯会在河岸举行。</p></body></html>"
	htmlDoc := mustClean(t, clean.New(nil), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/sniff-html",
		SourceURI:    "upload://html",
		Filename:     "upload.bin",
		Body:         strings.NewReader(htmlRaw),
	})
	if htmlDoc.Lineage.MediaType != "html" || !strings.Contains(htmlDoc.Text, "灯会在河岸举行。") || strings.Contains(htmlDoc.Text, "主导航模板") {
		t.Fatalf("sniff html %+v text %q", htmlDoc.Lineage, htmlDoc.Text)
	}
	pdfRaw, err := os.ReadFile("../../testdata/clean/lantern.pdf")
	if err != nil {
		t.Fatalf("read pdf: %v", err)
	}
	pdfDoc := mustClean(t, clean.New(nil), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/sniff-pdf",
		SourceURI:    "upload://pdf",
		Filename:     "scan.bin",
		Body:         strings.NewReader(string(pdfRaw)),
	})
	if pdfDoc.Lineage.MediaType != "pdf" || !strings.Contains(pdfDoc.Text, pdfSentence) {
		t.Fatalf("sniff pdf %+v text %q", pdfDoc.Lineage, pdfDoc.Text)
	}
}

func TestMarkdownAndTextKept(t *testing.T) {
	md := "# 灯会\n\n河边举行。\n"
	doc := mustClean(t, clean.New(nil), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/note.md",
		SourceURI:    "file://note.md",
		Filename:     "note.md",
		Body:         strings.NewReader(md),
	})
	if doc.Text != md {
		t.Fatalf("markdown %q", doc.Text)
	}
	assertIdentity(t, doc, "alpha", "raw/note.md", "file://note.md", "md")
	plain := "<nav>导航模板</nav>\n灯会在河岸举行。\n"
	txt := mustClean(t, clean.New(nil), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/notes.txt",
		SourceURI:    "file://notes.txt",
		Filename:     "notes.TXT",
		Body:         strings.NewReader(plain),
	})
	if txt.Text != plain || txt.Lineage.MediaType != "txt" {
		t.Fatalf("txt %q media %s", txt.Text, txt.Lineage.MediaType)
	}
}

func TestUTF8BOMAndUTF16(t *testing.T) {
	bom := append([]byte{0xEF, 0xBB, 0xBF}, []byte("灯会")...)
	doc := mustClean(t, clean.New(nil), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/bom.txt",
		Filename:     "bom.txt",
		Body:         strings.NewReader(string(bom)),
	})
	if doc.Text != "灯会" || strings.ToLower(doc.Lineage.Encoding) != "utf-8" {
		t.Fatalf("bom text %q enc %q", doc.Text, doc.Lineage.Encoding)
	}
	le := utf16LE("灯会")
	doc16 := mustClean(t, clean.New(nil), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/utf16.txt",
		Filename:     "utf16.txt",
		Body:         strings.NewReader(string(le)),
	})
	if doc16.Text != "灯会" || !strings.Contains(strings.ToLower(doc16.Lineage.Encoding), "utf-16") {
		t.Fatalf("utf16 text %q enc %q", doc16.Text, doc16.Lineage.Encoding)
	}
}

func TestUnsupportedType(t *testing.T) {
	c := clean.New(nil)
	_, err := c.Clean(context.Background(), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/a.docx",
		Filename:     "a.docx",
		Body:         strings.NewReader("hello"),
	})
	if !errors.Is(err, clean.ErrUnsupportedType) {
		t.Fatalf("docx err %v", err)
	}
	_, err = c.Clean(context.Background(), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/plain",
		Filename:     "plain",
		Body:         strings.NewReader("hello"),
	})
	if !errors.Is(err, clean.ErrUnsupportedType) {
		t.Fatalf("plain err %v", err)
	}
}

func TestNilBodyAndCanceled(t *testing.T) {
	c := clean.New(nil)
	if _, err := c.Clean(context.Background(), clean.Input{CollectionID: "alpha", Filename: "a.txt"}); err == nil {
		t.Fatal("nil body")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Clean(ctx, clean.Input{
		CollectionID: "alpha",
		Filename:     "a.txt",
		Body:         strings.NewReader("灯会"),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled %v", err)
	}
}

func mustClean(t *testing.T, c clean.Cleaner, in clean.Input) model.Document {
	t.Helper()
	doc, err := c.Clean(context.Background(), in)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	return doc
}

func assertIdentity(t *testing.T, doc model.Document, col model.CollectionID, key model.ObjectKey, uri, media string) {
	t.Helper()
	if doc.CollectionID != col || doc.Lineage.CollectionID != col {
		t.Fatalf("collection doc=%s lineage=%s", doc.CollectionID, doc.Lineage.CollectionID)
	}
	if doc.Lineage.RawObjectKey != key {
		t.Fatalf("key %s", doc.Lineage.RawObjectKey)
	}
	if doc.Lineage.SourceURI != uri {
		t.Fatalf("uri %s", doc.Lineage.SourceURI)
	}
	if doc.Lineage.MediaType != media {
		t.Fatalf("media %s", doc.Lineage.MediaType)
	}
	if doc.Lineage.DocumentID != doc.ID {
		t.Fatalf("document id %s lineage %s", doc.ID, doc.Lineage.DocumentID)
	}
	if !utf8.ValidString(doc.Text) {
		t.Fatal("text is not utf-8")
	}
	sum := sha256.Sum256([]byte(doc.Text))
	hash := hex.EncodeToString(sum[:])
	if doc.Lineage.ContentHash != hash || len(hash) != 64 {
		t.Fatalf("hash %s", doc.Lineage.ContentHash)
	}
	wantID := model.DocumentID(string(col) + ":" + hash[:16])
	if doc.ID != wantID {
		t.Fatalf("id %s want %s", doc.ID, wantID)
	}
	if doc.Lineage.Encoding == "" {
		t.Fatal("empty encoding")
	}
}

func requireSteps(t *testing.T, steps []model.CleanStep) {
	t.Helper()
	want := []string{"detect", "strip", "dedupe"}
	at := 0
	for _, step := range steps {
		if step.Name == "" || step.Detail == "" {
			t.Fatalf("blank step %+v", step)
		}
		if at < len(want) && step.Name == want[at] {
			at++
		}
	}
	if at != len(want) {
		t.Fatalf("steps %+v", steps)
	}
}

func stepDetailHas(steps []model.CleanStep, name, frag string) bool {
	frag = strings.ToLower(frag)
	for _, step := range steps {
		if step.Name == name && strings.Contains(strings.ToLower(step.Detail), frag) {
			return true
		}
	}
	return false
}

func utf16LE(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := []byte{0xFF, 0xFE}
	for _, x := range u {
		b = append(b, byte(x), byte(x>>8))
	}
	return b
}

type memDedup struct {
	mu sync.Mutex
	by map[model.CollectionID]map[string]model.DocumentID
}

func newMemDedup() *memDedup {
	return &memDedup{by: map[model.CollectionID]map[string]model.DocumentID{}}
}

func (m *memDedup) Find(_ context.Context, col model.CollectionID, contentHash string) (model.DocumentID, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.by[col][contentHash]
	return id, ok, nil
}

func (m *memDedup) Put(_ context.Context, col model.CollectionID, contentHash string, id model.DocumentID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.by[col]; !ok {
		m.by[col] = map[string]model.DocumentID{}
	}
	if old, ok := m.by[col][contentHash]; ok && old != id {
		return errors.New("dedupe: binding exists")
	}
	m.by[col][contentHash] = id
	return nil
}
