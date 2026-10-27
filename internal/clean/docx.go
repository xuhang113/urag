package clean

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// extractDOCX 用标准库读取 zip 里的 word/document.xml，按段落样式输出 UTF-8。
// 标题写成 markdown 的 #、##、###。正文段落之间空一行。不把部件写到磁盘。
func extractDOCX(raw []byte) (string, string, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", "", fmt.Errorf("clean: docx: %w", err)
	}
	levels := map[string]int{}
	if f := findZip(zr, "word/styles.xml"); f != nil {
		body, readErr := readZipFile(f)
		if readErr != nil {
			return "", "", fmt.Errorf("clean: docx: styles: %w", readErr)
		}
		levels, err = parseStyleLevels(body)
		if err != nil {
			return "", "", fmt.Errorf("clean: docx: styles: %w", err)
		}
	}
	f := findZip(zr, "word/document.xml")
	if f == nil {
		return "", "", errors.New("clean: docx: missing document")
	}
	body, err := readZipFile(f)
	if err != nil {
		return "", "", fmt.Errorf("clean: docx: %w", err)
	}
	text, err := parseDocumentXML(body, levels)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", "", errors.New("clean: docx: no text")
	}
	return text, "utf-8", nil
}

func isDocx(raw []byte) bool {
	if len(raw) < 4 || raw[0] != 'P' || raw[1] != 'K' {
		return false
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return false
	}
	return findZip(zr, "word/document.xml") != nil
}

func findZip(zr *zip.Reader, name string) *zip.File {
	name = strings.ToLower(name)
	for _, f := range zr.File {
		n := strings.ToLower(strings.ReplaceAll(f.Name, "\\", "/"))
		n = strings.TrimPrefix(n, "/")
		if n == name {
			return f
		}
	}
	return nil
}

func readZipFile(f *zip.File) ([]byte, error) {
	r, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

type docxStyle struct {
	id      string
	name    string
	basedOn string
	outline int
}

func parseStyleLevels(data []byte) (map[string]int, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var styles []docxStyle
	var cur *docxStyle
	inPPr := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "style":
				typ := xmlAttr(t, "type")
				if typ != "" && typ != "paragraph" {
					cur = nil
					continue
				}
				cur = &docxStyle{id: xmlAttr(t, "styleId"), outline: -1}
				inPPr = false
			case "name":
				if cur != nil {
					cur.name = xmlAttr(t, "val")
				}
			case "basedOn":
				if cur != nil {
					cur.basedOn = xmlAttr(t, "val")
				}
			case "pPr":
				inPPr = true
			case "outlineLvl":
				if cur != nil && inPPr {
					if n, convErr := strconv.Atoi(strings.TrimSpace(xmlAttr(t, "val"))); convErr == nil {
						cur.outline = n
					}
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "style":
				if cur != nil && cur.id != "" {
					styles = append(styles, *cur)
				}
				cur = nil
				inPPr = false
			case "pPr":
				inPPr = false
			}
		}
	}
	return resolveHeadingLevels(styles), nil
}

func resolveHeadingLevels(styles []docxStyle) map[string]int {
	byID := make(map[string]docxStyle, len(styles))
	for _, st := range styles {
		byID[st.id] = st
	}
	levels := map[string]int{}
	visiting := map[string]bool{}
	var rec func(id string) int
	rec = func(id string) int {
		if lv, ok := levels[id]; ok {
			return lv
		}
		st, ok := byID[id]
		if !ok {
			return 0
		}
		if visiting[id] {
			return 0
		}
		visiting[id] = true
		lv := 0
		// Word 用 9 表示正文，0 到 8 才是标题大纲。
		if st.outline >= 0 && st.outline <= 8 {
			lv = st.outline + 1
		} else if n := headingLevelFromName(st.name); n > 0 {
			lv = n
		} else if st.basedOn != "" {
			lv = rec(st.basedOn)
		}
		delete(visiting, id)
		levels[id] = capHeading(lv)
		return levels[id]
	}
	out := map[string]int{}
	for id := range byID {
		lv := rec(id)
		out[id] = lv
		if low := strings.ToLower(id); low != id {
			if _, ok := out[low]; !ok {
				out[low] = lv
			}
		}
	}
	return out
}

var (
	headingNameEN = regexp.MustCompile(`(?i)^heading\s*([1-9])$`)
	headingNameCN = regexp.MustCompile(`^标题\s*([1-9])$`)
)

func headingLevelFromName(name string) int {
	name = strings.TrimSpace(name)
	if m := headingNameEN.FindStringSubmatch(name); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil {
			return n
		}
	}
	if m := headingNameCN.FindStringSubmatch(name); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil {
			return n
		}
	}
	switch strings.ToLower(name) {
	case "title", "标题":
		return 1
	case "subtitle", "副标题":
		return 2
	default:
		return 0
	}
}

func capHeading(level int) int {
	if level < 0 {
		return 0
	}
	if level > 6 {
		return 6
	}
	return level
}

func paragraphLevel(styleID string, outline int, levels map[string]int) int {
	if outline >= 0 && outline <= 8 {
		return capHeading(outline + 1)
	}
	if styleID == "" {
		return 0
	}
	if levels != nil {
		if lv, ok := levels[styleID]; ok && lv > 0 {
			return capHeading(lv)
		}
		if lv, ok := levels[strings.ToLower(styleID)]; ok && lv > 0 {
			return capHeading(lv)
		}
	}
	return capHeading(headingLevelFromName(styleID))
}

type docxPara struct {
	level int
	text  string
}

func parseDocumentXML(data []byte, levels map[string]int) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var paras []docxPara
	var buf strings.Builder
	inP := false
	skip := false
	pPrDepth := 0
	delDepth := 0
	fallbackDepth := 0
	inT := 0
	styleID := ""
	outline := -1

	finish := func() {
		if !inP {
			return
		}
		inP = false
		pPrDepth = 0
		inT = 0
		if skip {
			buf.Reset()
			return
		}
		text := strings.Join(strings.Fields(strings.ReplaceAll(buf.String(), "\u00a0", " ")), " ")
		buf.Reset()
		if text == "" {
			return
		}
		paras = append(paras, docxPara{
			level: paragraphLevel(styleID, outline, levels),
			text:  text,
		})
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("clean: docx: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "Fallback":
				fallbackDepth++
			case "del", "moveFrom":
				delDepth++
			case "p":
				if inP {
					finish()
				}
				inP = true
				skip = fallbackDepth > 0
				pPrDepth = 0
				inT = 0
				styleID = ""
				outline = -1
				buf.Reset()
			case "pPr":
				if inP {
					pPrDepth++
				}
			case "pStyle":
				if inP && pPrDepth > 0 {
					styleID = xmlAttr(t, "val")
				}
			case "outlineLvl":
				if inP && pPrDepth > 0 {
					if n, convErr := strconv.Atoi(strings.TrimSpace(xmlAttr(t, "val"))); convErr == nil {
						outline = n
					}
				}
			case "t":
				if inP && pPrDepth == 0 && delDepth == 0 && fallbackDepth == 0 {
					inT++
				}
			case "tab":
				if inP && pPrDepth == 0 && delDepth == 0 && fallbackDepth == 0 {
					buf.WriteByte('\t')
				}
			case "br", "cr":
				if inP && pPrDepth == 0 && delDepth == 0 && fallbackDepth == 0 {
					buf.WriteByte('\n')
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "Fallback":
				if fallbackDepth > 0 {
					fallbackDepth--
				}
			case "del", "moveFrom":
				if delDepth > 0 {
					delDepth--
				}
			case "p":
				finish()
			case "pPr":
				if pPrDepth > 0 {
					pPrDepth--
				}
			case "t":
				if inT > 0 {
					inT--
				}
			}
		case xml.CharData:
			if inT > 0 {
				buf.Write([]byte(t))
			}
		}
	}
	finish()
	return renderDocx(paras), nil
}

func renderDocx(paras []docxPara) string {
	var b strings.Builder
	for _, p := range paras {
		if p.text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		if p.level > 0 {
			b.WriteString(strings.Repeat("#", p.level))
			b.WriteByte(' ')
		}
		b.WriteString(p.text)
	}
	return b.String()
}

func xmlAttr(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}
