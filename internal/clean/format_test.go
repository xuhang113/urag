package clean_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/clean"
)

func TestDOCXHeadingsAndParagraphs(t *testing.T) {
	const document = `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p>
      <w:pPr><w:pStyle w:val="Heading1"/></w:pPr>
      <w:r><w:t>灯会</w:t></w:r>
    </w:p>
    <w:p>
      <w:r><w:t>灯会在</w:t></w:r>
      <w:r><w:t>河岸举行。</w:t></w:r>
    </w:p>
    <w:p>
      <w:r><w:t>第二段在河边。</w:t></w:r>
      <w:r><w:tab/></w:r>
      <w:r><w:t>甲</w:t><w:br/><w:t>乙</w:t></w:r>
    </w:p>
    <w:p>
      <w:pPr><w:pStyle w:val="Heading2"/></w:pPr>
      <w:r><w:t>河岸</w:t></w:r>
    </w:p>
    <w:p>
      <w:hyperlink w:anchor="x">
        <w:r><w:t>这段正文。</w:t></w:r>
      </w:hyperlink>
      <w:r><w:instrText> PAGE </w:instrText></w:r>
    </w:p>
    <w:p>
      <w:pPr><w:pStyle w:val="Heading3"/></w:pPr>
      <w:r><w:t>夜航</w:t></w:r>
    </w:p>
    <w:p>
      <w:r><w:t>灯会 &amp; 河</w:t></w:r>
      <w:del><w:r><w:t>删掉的字</w:t></w:r></w:del>
      <w:r><w:delText>也删</w:delText></w:r>
    </w:p>
    <w:tbl>
      <w:tr><w:tc>
        <w:p><w:r><w:t>格内正文</w:t></w:r></w:p>
      </w:tc></w:tr>
    </w:tbl>
    <w:p>
      <w:r><w:t></w:t></w:r>
    </w:p>
  </w:body>
</w:document>`
	raw := buildZip(t, map[string]string{
		"word/document.xml": document,
		"word/header1.xml":  `<w:hdr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:p><w:r><w:t>页眉模板</w:t></w:r></w:p></w:hdr>`,
		"word/footer1.xml":  `<w:ftr xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:p><w:r><w:t>页脚模板</w:t></w:r></w:p></w:ftr>`,
	})
	doc := mustClean(t, clean.New(nil), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/lantern.docx",
		SourceURI:    "file://lantern.docx",
		Filename:     "lantern.docx",
		Body:         bytes.NewReader(raw),
	})
	want := strings.Join([]string{
		"# 灯会",
		"灯会在河岸举行。",
		"第二段在河边。 甲 乙",
		"## 河岸",
		"这段正文。",
		"### 夜航",
		"灯会 & 河",
		"格内正文",
	}, "\n\n")
	if doc.Text != want {
		t.Fatalf("docx text\n%s", doc.Text)
	}
	for _, drop := range []string{"页眉模板", "页脚模板", "删掉的字", "也删", "PAGE"} {
		if strings.Contains(doc.Text, drop) {
			t.Fatalf("kept %q in %q", drop, doc.Text)
		}
	}
	assertIdentity(t, doc, "alpha", "raw/lantern.docx", "file://lantern.docx", "docx")
	requireSteps(t, doc.Lineage.Steps)
	if doc.Lineage.Encoding != "utf-8" {
		t.Fatalf("encoding %q", doc.Lineage.Encoding)
	}

	sections, err := chunk.SplitSections(doc)
	if err != nil {
		t.Fatalf("sections: %v", err)
	}
	if len(sections) != 3 {
		t.Fatalf("sections %d", len(sections))
	}
	paths := []string{"灯会", "灯会 / 河岸", "灯会 / 河岸 / 夜航"}
	bodies := []string{
		"灯会在河岸举行。\n\n第二段在河边。 甲 乙",
		"这段正文。",
		"灯会 & 河\n\n格内正文",
	}
	for i := range paths {
		if sections[i].HeadingPath != paths[i] || sections[i].Body != bodies[i] {
			t.Fatalf("section %d path %q body %q", i, sections[i].HeadingPath, sections[i].Body)
		}
		if sections[i].PageFrom != 0 || sections[i].PageTo != 0 {
			t.Fatalf("pages %d %d", sections[i].PageFrom, sections[i].PageTo)
		}
	}
}

func TestDOCXStyleResolution(t *testing.T) {
	const styles = `<?xml version="1.0" encoding="UTF-8"?>
<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:style w:type="paragraph" w:styleId="Heading1">
    <w:name w:val="heading 1"/>
    <w:pPr><w:outlineLvl w:val="0"/></w:pPr>
  </w:style>
  <w:style w:type="paragraph" w:styleId="MyH2">
    <w:name w:val="heading 2"/>
  </w:style>
  <w:style w:type="paragraph" w:styleId="Alias">
    <w:name w:val="Weird"/>
    <w:basedOn w:val="MyH2"/>
  </w:style>
  <w:style w:type="paragraph" w:styleId="Cn2">
    <w:name w:val="标题 2"/>
  </w:style>
  <w:style w:type="paragraph" w:styleId="Normal">
    <w:name w:val="Normal"/>
    <w:pPr><w:outlineLvl w:val="9"/></w:pPr>
  </w:style>
</w:styles>`
	const document = `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>总题</w:t></w:r></w:p>
    <w:p><w:pPr><w:pStyle w:val="Alias"/></w:pPr><w:r><w:t>别名节</w:t></w:r></w:p>
    <w:p><w:pPr><w:pStyle w:val="Cn2"/></w:pPr><w:r><w:t>中文节</w:t></w:r></w:p>
    <w:p><w:pPr><w:pStyle w:val="Normal"/><w:outlineLvl w:val="1"/></w:pPr><w:r><w:t>直接大纲</w:t></w:r></w:p>
    <w:p><w:pPr><w:pStyle w:val="Normal"/></w:pPr><w:r><w:t>普通正文</w:t></w:r></w:p>
    <w:p><w:pPr><w:pStyle w:val="Heading1"/><w:outlineLvl w:val="2"/></w:pPr><w:r><w:t>覆盖层级</w:t></w:r></w:p>
  </w:body>
</w:document>`
	raw := buildZip(t, map[string]string{
		"word/styles.xml":   styles,
		"word/document.xml": document,
	})
	doc := mustClean(t, clean.New(nil), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/styles.docx",
		Filename:     "styles.DOCX",
		Body:         bytes.NewReader(raw),
	})
	want := strings.Join([]string{
		"# 总题",
		"## 别名节",
		"## 中文节",
		"## 直接大纲",
		"普通正文",
		"### 覆盖层级",
	}, "\n\n")
	if doc.Text != want {
		t.Fatalf("styled text\n%s", doc.Text)
	}
	if doc.Lineage.MediaType != "docx" {
		t.Fatalf("media %s", doc.Lineage.MediaType)
	}
}

func TestDOCXSniffAndReject(t *testing.T) {
	raw := buildZip(t, map[string]string{
		"word/document.xml": `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body><w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>灯会</w:t></w:r></w:p></w:body>
</w:document>`,
	})
	doc := mustClean(t, clean.New(nil), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/sniff-docx",
		Filename:     "upload.bin",
		Body:         bytes.NewReader(raw),
	})
	if doc.Lineage.MediaType != "docx" || doc.Text != "# 灯会" {
		t.Fatalf("sniff %+v %q", doc.Lineage, doc.Text)
	}

	other := buildZip(t, map[string]string{"hello.txt": "不是文档"})
	_, err := clean.New(nil).Clean(context.Background(), clean.Input{
		CollectionID: "alpha",
		ObjectKey:    "raw/a.docx",
		Filename:     "a.docx",
		Body:         bytes.NewReader(other),
	})
	if !errors.Is(err, clean.ErrUnsupportedType) {
		t.Fatalf("zip err %v", err)
	}
}

func TestHTMLKeepsBodyHeadings(t *testing.T) {
	body := `<!DOCTYPE html>
<html>
<head><title>page</title><style>.nav{color:red}</style></head>
<body>
<header id="site-header">站点页眉模板</header>
<nav><h1>导航标题</h1>主导航模板</nav>
<div class="sidebar"><h2>侧栏标题</h2><p>侧栏模板</p></div>
<footer>版权页脚模板</footer>
<article>
<h1>灯会</h1>
<p>灯会在河岸举行。</p>
<h2>河<em>岸</em></h2>
<p>这段正文提到 nav 这个词。</p>
<h3>夜航</h3>
<p>第三段。</p>
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
	want := strings.Join([]string{
		"# 灯会",
		"灯会在河岸举行。",
		"## 河岸",
		"这段正文提到 nav 这个词。",
		"### 夜航",
		"第三段。",
	}, "\n\n")
	if !strings.Contains(doc.Text, want) {
		t.Fatalf("html text\n%s", doc.Text)
	}
	for _, drop := range []string{"站点页眉模板", "导航标题", "主导航模板", "侧栏标题", "侧栏模板", "版权页脚模板", ".nav{color:red}"} {
		if strings.Contains(doc.Text, drop) {
			t.Fatalf("boilerplate %q still in %q", drop, doc.Text)
		}
	}
	assertIdentity(t, doc, "alpha", "raw/page.html", "https://example.test/page.html", "html")

	sections, err := chunk.SplitSections(doc)
	if err != nil {
		t.Fatalf("sections: %v", err)
	}
	var paths []string
	for _, sec := range sections {
		if sec.HeadingPath != "" {
			paths = append(paths, sec.HeadingPath)
		}
		if sec.PageFrom != 0 || sec.PageTo != 0 {
			t.Fatalf("pages %+v", sec)
		}
	}
	wantPaths := []string{"灯会", "灯会 / 河岸", "灯会 / 河岸 / 夜航"}
	if strings.Join(paths, "|") != strings.Join(wantPaths, "|") {
		t.Fatalf("paths %v", paths)
	}
}

func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}
