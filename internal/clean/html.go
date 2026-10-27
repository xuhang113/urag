package clean

import (
	"fmt"
	"strings"

	"golang.org/x/net/html"
)

// boilerplateHints 出现在 class 或 id 里时，整块不算正文。
var boilerplateHints = []string{"nav", "header", "footer", "menu", "sidebar"}

func stripHTML(s string) (string, string, error) {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return "", "", err
	}
	var blocks []string
	var removed []string
	var inline strings.Builder
	flushInline := func() {
		text := cleanBlock(inline.String())
		inline.Reset()
		if text != "" {
			blocks = append(blocks, text)
		}
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode {
			if why, drop := dropReason(n); drop {
				removed = append(removed, why)
				return
			}
			// 标题单独成 markdown 行，后面的切分才认得出节。
			if level, ok := headingLevel(n.Data); ok {
				flushInline()
				if text := oneLine(innerText(n, &removed)); text != "" {
					blocks = append(blocks, strings.Repeat("#", level)+" "+text)
				}
				return
			}
			if isParagraphTag(n.Data) {
				flushInline()
				if text := cleanBlock(innerText(n, &removed)); text != "" {
					blocks = append(blocks, text)
				}
				return
			}
			if n.Data == "br" {
				inline.WriteByte('\n')
				return
			}
		}
		if n.Type == html.TextNode {
			inline.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && isBlock(n.Data) {
			flushInline()
		}
	}
	walk(doc)
	flushInline()
	text := strings.Join(blocks, "\n\n")
	detail := fmt.Sprintf("removed=%s bytes=%d", joinUnique(removed), len(text))
	if len(removed) == 0 {
		detail = fmt.Sprintf("removed=none bytes=%d", len(text))
	}
	return text, detail, nil
}

func innerText(n *html.Node, removed *[]string) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if why, drop := dropReason(n); drop {
				if removed != nil {
					*removed = append(*removed, why)
				}
				return
			}
			if n.Data == "br" {
				b.WriteByte('\n')
				return
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

func headingLevel(tag string) (int, bool) {
	switch tag {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		return int(tag[1] - '0'), true
	default:
		return 0, false
	}
}

func isParagraphTag(tag string) bool {
	switch tag {
	case "p", "pre", "blockquote", "figcaption", "address", "dd", "dt":
		return true
	default:
		return false
	}
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\u00a0", " ")
	return strings.Join(strings.Fields(s), " ")
}

func cleanBlock(s string) string {
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func dropReason(n *html.Node) (string, bool) {
	// script、style、noscript 里的文字不是可见正文。
	switch n.Data {
	case "nav", "header", "footer", "script", "style", "noscript":
		return n.Data, true
	}
	blob := strings.ToLower(attr(n, "class") + " " + attr(n, "id"))
	for _, hint := range boilerplateHints {
		if strings.Contains(blob, hint) {
			return hint, true
		}
	}
	return "", false
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func isBlock(tag string) bool {
	switch tag {
	case "address", "article", "aside", "blockquote", "br", "dd", "div", "dl", "dt",
		"figcaption", "figure", "footer", "h1", "h2", "h3", "h4", "h5", "h6",
		"header", "hr", "li", "main", "nav", "ol", "p", "pre", "section",
		"table", "tr", "ul":
		return true
	default:
		return false
	}
}

func joinUnique(items []string) string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return strings.Join(out, ",")
}
