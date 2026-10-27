package acquire

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type sitemapSource struct {
	rawURL string
}

// NewSitemap 准备 rawURL 上的站点地图来源。Kind 为 KindSitemap。
// 只接受 http 和 https。其他协议、无法解析的地址或没有主机名时返回错误，不拨号。
// List 读取 urlset 和 sitemapindex，只返回 pdf、docx、html、txt、md 的文档 URL。
// 同一地址只出现一次。robots.txt 拒绝的地址不列入。List 不下载文档正文，也不写入对象存储。
// 正文要等调用方执行 Open 才按需打开。站点地图本身取不到或不是站点地图时返回错误。
func NewSitemap(rawURL string) (Source, error) {
	if _, err := parseHTTPURL(rawURL); err != nil {
		return nil, err
	}
	return &sitemapSource{rawURL: rawURL}, nil
}

func (s *sitemapSource) Kind() string { return KindSitemap }

func (s *sitemapSource) List(ctx context.Context) ([]Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 站点地图来源没有单独的间隔参数，按 1s 礼貌访问 robots 和地图文件。
	c := newCrawler(http.DefaultClient, CrawlLimits{PerHostInterval: time.Second})
	locs, err := c.walkSitemap(ctx, s.rawURL, 0, map[string]struct{}{})
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(locs))
	items := make([]Item, 0, len(locs))
	for _, loc := range locs {
		key := canonURL(loc)
		if _, ok := seen[key]; ok {
			continue
		}
		media, ok := mediaByExt(loc)
		if !ok {
			continue
		}
		u, err := url.Parse(loc)
		if err != nil {
			continue
		}
		if err := c.ensureRobots(ctx, u); err != nil {
			return nil, err
		}
		if c.blocked(u) {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, Item{
			SourceURI:   loc,
			Filename:    filenameFromURL(loc),
			ContentType: mimeOfMedia(media),
			Open:        c.opener(loc),
		})
	}
	return items, nil
}

// walkSitemap 只抓取站点地图文件，把文档地址留在结果里，不打开文档。
func (c *crawler) walkSitemap(ctx context.Context, raw string, depth int, visited map[string]struct{}) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if depth > 8 {
		return nil, fmt.Errorf("acquire: sitemap %s is nested too deep", raw)
	}
	abs, ok := normalize(raw, nil)
	if !ok {
		return nil, fmt.Errorf("acquire: invalid sitemap url %q", raw)
	}
	if _, seen := visited[abs]; seen {
		return nil, nil
	}
	visited[abs] = struct{}{}

	u, err := url.Parse(abs)
	if err != nil {
		return nil, err
	}
	if err := c.ensureRobots(ctx, u); err != nil {
		return nil, err
	}
	if c.blocked(u) {
		return nil, fmt.Errorf("acquire: robots disallow %s", abs)
	}
	p, err := c.fetch(ctx, abs, http.MethodGet)
	if err != nil {
		return nil, err
	}
	if p.status < 200 || p.status >= 300 {
		return nil, fmt.Errorf("acquire: GET %s: status %d", abs, p.status)
	}
	base, err := url.Parse(p.final)
	if err != nil {
		return nil, err
	}
	docs, children, isMap, perr := parseSitemap(p.body)
	if perr != nil || !isMap {
		return nil, fmt.Errorf("acquire: %s is not a sitemap", abs)
	}
	out := make([]string, 0, len(docs))
	for _, loc := range docs {
		absLoc, ok := normalize(loc, base)
		if !ok {
			continue
		}
		out = append(out, absLoc)
	}
	for _, child := range children {
		absChild, ok := normalize(child, base)
		if !ok {
			return nil, fmt.Errorf("acquire: invalid sitemap url %q", child)
		}
		more, err := c.walkSitemap(ctx, absChild, depth+1, visited)
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	return out, nil
}

type sitemapFile struct {
	URLs     []sitemapLoc `xml:"url"`
	Sitemaps []sitemapLoc `xml:"sitemap"`
}

type sitemapLoc struct {
	Loc string `xml:"loc"`
}

// parseSitemap 识别 urlset 与 sitemapindex。不是站点地图时 isMap 为 false。
// 看起来是站点地图但 XML 无法解析时返回错误，调用方记为 download。
func parseSitemap(body []byte) (docs, children []string, isMap bool, err error) {
	trimmed := bytes.TrimSpace(body)
	trimmed = bytes.TrimPrefix(trimmed, []byte{0xEF, 0xBB, 0xBF})
	head := trimmed
	if len(head) > 2048 {
		head = head[:2048]
	}
	lower := bytes.ToLower(head)
	if !bytes.Contains(lower, []byte("<urlset")) && !bytes.Contains(lower, []byte("<sitemapindex")) {
		return nil, nil, false, nil
	}
	var doc sitemapFile
	if err := xml.Unmarshal(trimmed, &doc); err != nil {
		return nil, nil, true, err
	}
	for _, u := range doc.URLs {
		loc := strings.TrimSpace(u.Loc)
		if loc != "" {
			docs = append(docs, loc)
		}
	}
	for _, u := range doc.Sitemaps {
		loc := strings.TrimSpace(u.Loc)
		if loc != "" {
			children = append(children, loc)
		}
	}
	return docs, children, true, nil
}

func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("acquire: scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("acquire: http url %q has no host", raw)
	}
	return u, nil
}

var _ Source = (*sitemapSource)(nil)
