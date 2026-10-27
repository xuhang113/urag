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
	var b strings.Builder
	var removed []string
	extractHTML(doc, &b, &removed)
	text := normalizeHTMLText(b.String())
	detail := fmt.Sprintf("removed=%s bytes=%d", joinUnique(removed), len(text))
	if len(removed) == 0 {
		detail = fmt.Sprintf("removed=none bytes=%d", len(text))
	}
	return text, detail, nil
}

func extractHTML(n *html.Node, b *strings.Builder, removed *[]string) {
	if n == nil {
		return
	}
	if n.Type == html.ElementNode {
		if why, drop := dropReason(n); drop {
			*removed = append(*removed, why)
			return
		}
	}
	if n.Type == html.TextNode {
		b.WriteString(n.Data)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		extractHTML(c, b, removed)
	}
	if n.Type == html.ElementNode && isBlock(n.Data) {
		b.WriteByte('\n')
	}
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

func normalizeHTMLText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\u00a0", " ")
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
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
