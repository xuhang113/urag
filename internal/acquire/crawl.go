package acquire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"github.com/xuhang113/urag/internal/storage"
)

const (
	userAgent       = "urag-acquire"
	defaultMaxBytes = 32 << 20

	reasonRobots      = "robots"
	reasonLogin       = "login"
	reasonPayment     = "payment"
	reasonCaptcha     = "captcha"
	reasonUnsupported = "unsupported"
	reasonDownload    = "download"
)

// CrawlLimits 控制公开收录的节奏和单次响应体积。
// PerHostInterval 是同一主机相邻两次请求的最小间隔。
// 0 表示不额外等待，测试使用 0；对真实站点应传入 1s。
// MaxBytes 是单次响应读入上限，0 时使用 32<<20。
type CrawlLimits struct {
	PerHostInterval time.Duration
	MaxBytes        int64
}

// Skip 是一条没有列入结果的地址。
// Reason 取 robots、login、payment、captcha、unsupported、download。
type Skip struct {
	URI    string
	Reason string
}

// Found 是一条不登录就能下载的公开文档。
// SourceURI 是重定向之后的最终 URL。MediaType 取 pdf、docx、html、txt、md。
// Title 和 License 只在页面或响应头里能看到时填写，否则为空。
// Open 在调用时再打开正文流，Crawl 本身不把正文写入对象存储。
type Found struct {
	SourceURI string
	MediaType string
	Title     string
	License   string
	Open      func(ctx context.Context) (io.ReadCloser, error)
}

// Checkpoint 记录已经处理过的来源地址。
// 断点的保存不在本包完成。Done 是已经处理的 SourceURI，交给 WithDone 后不再列出。
type Checkpoint struct {
	Done []string
}

// Crawler 从站点地图或公开目录页列出文档。
type Crawler interface {
	// Crawl 不把正文写入对象存储。同一 SourceURI 只出现一次。
	// 登录、付费、验证码、robots 拒绝、不支持的类型放入 skips，不返回错误。
	// ctx 取消时返回已列出的 found、skips 和 ctx.Err()。
	Crawl(ctx context.Context, seeds []string) (found []Found, skips []Skip, err error)
	// WithDone 返回收录器，uris 里的来源地址不再列出，也不会再请求。
	WithDone(uris []string) Crawler
}

type crawler struct {
	client *http.Client
	limits CrawlLimits
	done   map[string]struct{}

	mu    sync.Mutex
	hosts map[string]*hostState
}

type hostState struct {
	robotsOK bool
	rules    robotsRules
	interval time.Duration
	last     time.Time
}

type fetchKind int

const (
	// fetchAuto 由响应决定是站点地图、目录页还是文档。HTML 目录会继续抽出链接。
	fetchAuto fetchKind = iota
	// fetchDoc 是文档地址。HTML 只作为文档，不再展开页面里的链接。
	fetchDoc
)

type crawlTask struct {
	raw  string
	kind fetchKind
}

type page struct {
	final  string
	status int
	header http.Header
	body   []byte
}

type robotsDeny struct {
	uri string
}

func (e *robotsDeny) Error() string {
	return "acquire: robots disallow " + e.uri
}

var (
	reCaptcha = regexp.MustCompile(`(?i)\bcaptcha\b`)
	rePaywall = regexp.MustCompile(`(?i)\bpaywall\b`)
	reLogin   = regexp.MustCompile(`(?i)\blogin\b`)
)

// NewCrawler 准备公开收录。client 为 nil 时使用 http.DefaultClient。
// 收录过程不携带 Cookie，401、402、403 也不会改用 Cookie 再请求。
// 返回的 Crawler 不调用 storage.Store。
func NewCrawler(client *http.Client, limits CrawlLimits) Crawler {
	return newCrawler(client, limits)
}

func newCrawler(client *http.Client, limits CrawlLimits) *crawler {
	if client == nil {
		client = http.DefaultClient
	}
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = defaultMaxBytes
	}
	return &crawler{
		client: client,
		limits: limits,
		done:   map[string]struct{}{},
		hosts:  map[string]*hostState{},
	}
}

func (c *crawler) WithDone(uris []string) Crawler {
	done := make(map[string]struct{}, len(uris)*2)
	for _, raw := range uris {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		done[raw] = struct{}{}
		if abs, ok := normalize(raw, nil); ok {
			done[abs] = struct{}{}
		}
	}
	return &crawler{
		client: c.client,
		limits: c.limits,
		done:   done,
		hosts:  map[string]*hostState{},
	}
}

func (c *crawler) Crawl(ctx context.Context, seeds []string) ([]Found, []Skip, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var found []Found
	var skips []Skip
	seen := map[string]struct{}{}
	queued := map[string]struct{}{}
	var queue []crawlTask

	enqueue := func(raw string, kind fetchKind) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		abs, ok := normalize(raw, nil)
		if !ok {
			skips = append(skips, Skip{URI: raw, Reason: reasonDownload})
			return
		}
		key := canonURL(abs)
		if _, ok := queued[key]; ok {
			return
		}
		queued[key] = struct{}{}
		if c.isDone(key) {
			return
		}
		queue = append(queue, crawlTask{raw: abs, kind: kind})
	}

	for _, seed := range seeds {
		if err := ctx.Err(); err != nil {
			return found, skips, err
		}
		enqueue(seed, fetchAuto)
	}

	for i := 0; i < len(queue); i++ {
		if err := ctx.Err(); err != nil {
			return found, skips, err
		}
		task := queue[i]
		if _, ok := seen[canonURL(task.raw)]; ok {
			continue
		}
		extra, item, skip, err := c.visit(ctx, task)
		if err != nil {
			if isCtxErr(err) {
				return found, skips, err
			}
			skips = append(skips, Skip{URI: task.raw, Reason: reasonDownload})
			continue
		}
		if skip != nil {
			skips = append(skips, *skip)
		}
		if item != nil {
			key := canonURL(item.SourceURI)
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				found = append(found, *item)
			}
		}
		for _, ex := range extra {
			enqueue(ex.raw, ex.kind)
		}
	}
	return found, skips, nil
}

func (c *crawler) visit(ctx context.Context, task crawlTask) ([]crawlTask, *Found, *Skip, error) {
	u, err := url.Parse(task.raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, nil, &Skip{URI: task.raw, Reason: reasonDownload}, nil
	}
	if c.isDone(task.raw) {
		return nil, nil, nil, nil
	}
	if err := c.ensureRobots(ctx, u); err != nil {
		return nil, nil, nil, err
	}
	if c.blocked(u) {
		return nil, nil, &Skip{URI: task.raw, Reason: reasonRobots}, nil
	}
	ext := strings.ToLower(path.Ext(u.Path))
	// 后缀已经说明不是文档时不再请求，避免把图片和压缩包下载下来。
	if unsupportedExt(ext) {
		return nil, nil, &Skip{URI: task.raw, Reason: reasonUnsupported}, nil
	}

	method := http.MethodGet
	if media, ok := mediaByExtName(ext); ok && media != "html" {
		method = http.MethodHead
	}
	p, skip, err := c.load(ctx, task.raw, method)
	if skip != nil || err != nil {
		return nil, nil, skip, err
	}
	if method == http.MethodHead && shouldGet(p) {
		p, skip, err = c.load(ctx, task.raw, http.MethodGet)
		if skip != nil || err != nil {
			return nil, nil, skip, err
		}
	}
	return c.classify(ctx, task, p)
}

func (c *crawler) load(ctx context.Context, raw, method string) (page, *Skip, error) {
	p, err := c.fetch(ctx, raw, method)
	if err == nil {
		return p, nil, nil
	}
	var deny *robotsDeny
	if errors.As(err, &deny) {
		uri := raw
		if deny.uri != "" {
			uri = deny.uri
		}
		return page{}, &Skip{URI: uri, Reason: reasonRobots}, nil
	}
	return page{}, nil, err
}

func (c *crawler) classify(ctx context.Context, task crawlTask, p page) ([]crawlTask, *Found, *Skip, error) {
	if p.final == "" {
		p.final = task.raw
	}
	finalU, err := url.Parse(p.final)
	if err != nil {
		return nil, nil, &Skip{URI: p.final, Reason: reasonDownload}, nil
	}
	if canonURL(p.final) != canonURL(task.raw) {
		if err := c.ensureRobots(ctx, finalU); err != nil {
			return nil, nil, nil, err
		}
		if c.blocked(finalU) {
			return nil, nil, &Skip{URI: p.final, Reason: reasonRobots}, nil
		}
	}
	if c.isDone(p.final) {
		return nil, nil, nil, nil
	}
	if reason, ok := statusReason(p.status); ok {
		return nil, nil, &Skip{URI: p.final, Reason: reason}, nil
	}
	if p.status < 200 || p.status >= 300 {
		return nil, nil, &Skip{URI: p.final, Reason: reasonDownload}, nil
	}

	// 站点地图本身不是文档。先于登录字样判断，避免 loc 里的词误伤整张地图。
	if len(p.body) > 0 {
		docs, children, isMap, perr := parseSitemap(p.body)
		if isMap {
			if perr != nil {
				return nil, nil, &Skip{URI: p.final, Reason: reasonDownload}, nil
			}
			extra := make([]crawlTask, 0, len(docs)+len(children))
			for _, loc := range children {
				if abs, ok := normalize(loc, finalU); ok {
					extra = append(extra, crawlTask{raw: abs, kind: fetchAuto})
				}
			}
			for _, loc := range docs {
				if abs, ok := normalize(loc, finalU); ok {
					extra = append(extra, crawlTask{raw: abs, kind: fetchDoc})
				}
			}
			return extra, nil, nil, nil
		}
	}

	license := licenseFromHeaders(p.header)
	var title string
	var links []string
	if pageIsHTML(p) {
		var pageLic string
		var visible string
		title, pageLic, visible, links = inspectHTML(finalU, p.body)
		if license == "" {
			license = pageLic
		}
		if reason, ok := wallText(visible); ok {
			return nil, nil, &Skip{URI: p.final, Reason: reason}, nil
		}
	}

	media, ok := decideMedia(p.final, p.header.Get("Content-Type"), p.body)
	if !ok {
		return nil, nil, &Skip{URI: p.final, Reason: reasonUnsupported}, nil
	}
	var extra []crawlTask
	if media == "html" && task.kind == fetchAuto {
		for _, link := range links {
			extra = append(extra, crawlTask{raw: link, kind: fetchDoc})
		}
	}
	if media != "html" {
		title = ""
	}
	item := &Found{
		SourceURI: p.final,
		MediaType: media,
		Title:     title,
		License:   license,
		Open:      c.opener(p.final),
	}
	return extra, item, nil, nil
}

func (c *crawler) fetch(ctx context.Context, raw, method string) (page, error) {
	resp, err := c.do(ctx, method, raw)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return page{}, err
	}
	defer resp.Body.Close()
	final := raw
	if resp.Request != nil && resp.Request.URL != nil {
		u := *resp.Request.URL
		u.Fragment = ""
		if abs, ok := normalize(u.String(), nil); ok {
			final = abs
		} else {
			final = u.String()
		}
	}
	p := page{
		final:  final,
		status: resp.StatusCode,
		header: resp.Header.Clone(),
	}
	if method != http.MethodHead && resp.Body != nil {
		body, err := readLimited(resp.Body, c.limits.MaxBytes)
		p.body = body
		if err != nil {
			return p, err
		}
	}
	return p, nil
}

func (c *crawler) do(ctx context.Context, method, raw string) (*http.Response, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if err := c.waitTurn(ctx, u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	client := c.bareClient()
	prev := c.client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("acquire: too many redirects")
		}
		if err := c.ensureRobots(req.Context(), req.URL); err != nil {
			return err
		}
		if c.blocked(req.URL) {
			return &robotsDeny{uri: req.URL.String()}
		}
		if err := c.waitTurn(req.Context(), req.URL); err != nil {
			return err
		}
		if prev != nil {
			return prev(req, via)
		}
		return nil
	}
	return client.Do(req)
}

func (c *crawler) opener(raw string) func(context.Context) (io.ReadCloser, error) {
	return func(ctx context.Context) (io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, err
		}
		if err := c.ensureRobots(ctx, u); err != nil {
			return nil, err
		}
		resp, err := c.do(ctx, http.MethodGet, raw)
		if err != nil {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, fmt.Errorf("acquire: GET %s: %s", raw, resp.Status)
		}
		return &limitedReadCloser{rc: resp.Body, left: c.limits.MaxBytes}, nil
	}
}

// ensureRobots 每个主机只取一次 /robots.txt。
// 没有这份文件或取不到时，只保留 PerHostInterval，不因此失败。
// robots.txt 返回 401 或 403 时，该主机后续地址记为 robots。
func (c *crawler) ensureRobots(ctx context.Context, u *url.URL) error {
	if u == nil {
		return nil
	}
	key := hostKey(u)
	c.mu.Lock()
	st := c.hostLocked(key)
	if st.robotsOK {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	robotsURL := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/robots.txt"}
	if err := c.waitTurn(ctx, robotsURL); err != nil {
		return err
	}
	body, status, err := c.getRobots(ctx, robotsURL.String())
	if err != nil {
		if isCtxErr(err) {
			return err
		}
		c.mu.Lock()
		c.hostLocked(key).robotsOK = true
		c.mu.Unlock()
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st = c.hostLocked(key)
	st.robotsOK = true
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		st.rules = robotsRules{disallowAll: true}
		return nil
	}
	if status != http.StatusOK {
		return nil
	}
	rules := parseRobots(string(body), userAgent)
	st.rules = rules
	if rules.delay > st.interval {
		st.interval = rules.delay
	}
	return nil
}

func (c *crawler) getRobots(ctx context.Context, raw string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	client := c.bareClient()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("acquire: too many redirects")
		}
		return c.waitTurn(req.Context(), req.URL)
	}
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return nil, 0, err
	}
	defer resp.Body.Close()
	limit := c.limits.MaxBytes
	if limit > 512<<10 {
		limit = 512 << 10
	}
	body, err := readLimited(resp.Body, limit)
	return body, resp.StatusCode, err
}

func (c *crawler) blocked(u *url.URL) bool {
	if u == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.hosts[hostKey(u)]
	if st == nil {
		return false
	}
	return robotsDisallow(u, st.rules)
}

// waitTurn 保证同一主机相邻两次请求至少间隔当前生效的间隔。
// 间隔为 0 时直接返回。取消时返回 ctx.Err()。
func (c *crawler) waitTurn(ctx context.Context, u *url.URL) error {
	if u == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	key := hostKey(u)
	c.mu.Lock()
	st := c.hostLocked(key)
	var wait time.Duration
	if st.interval > 0 && !st.last.IsZero() {
		if d := time.Until(st.last.Add(st.interval)); d > 0 {
			wait = d
		}
	}
	st.last = time.Now().Add(wait)
	c.mu.Unlock()
	if wait == 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *crawler) hostLocked(key string) *hostState {
	st := c.hosts[key]
	if st == nil {
		st = &hostState{interval: c.limits.PerHostInterval}
		c.hosts[key] = st
	}
	return st
}

func (c *crawler) bareClient() *http.Client {
	base := c.client
	if base == nil {
		base = http.DefaultClient
	}
	clone := *base
	clone.Jar = nil
	return &clone
}

func (c *crawler) isDone(raw string) bool {
	if len(c.done) == 0 {
		return false
	}
	if _, ok := c.done[raw]; ok {
		return true
	}
	if _, ok := c.done[canonURL(raw)]; ok {
		return true
	}
	return false
}

func hostKey(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

func shouldGet(p page) bool {
	if p.status == http.StatusMethodNotAllowed || p.status == http.StatusNotImplemented {
		return true
	}
	if p.status != http.StatusOK {
		return false
	}
	if media, ok := mediaByType(p.header.Get("Content-Type")); ok {
		return media == "html"
	}
	if _, ok := mediaByExt(p.final); ok {
		return false
	}
	return true
}

func statusReason(code int) (string, bool) {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return reasonLogin, true
	case http.StatusPaymentRequired:
		return reasonPayment, true
	default:
		return "", false
	}
}

func wallText(visible string) (string, bool) {
	if reCaptcha.MatchString(visible) {
		return reasonCaptcha, true
	}
	if rePaywall.MatchString(visible) {
		return reasonPayment, true
	}
	if reLogin.MatchString(visible) {
		return reasonLogin, true
	}
	return "", false
}

func pageIsHTML(p page) bool {
	if media, ok := mediaByType(headerContentType(p.header)); ok && media == "html" {
		return true
	}
	if media, ok := mediaByExt(p.final); ok && media == "html" {
		return true
	}
	return looksLikeHTML(p.body)
}

func headerContentType(h http.Header) string {
	if h == nil {
		return ""
	}
	return h.Get("Content-Type")
}

func decideMedia(rawURL, contentType string, body []byte) (string, bool) {
	extMedia, hasExt := mediaByExt(rawURL)
	typeMedia, hasType := mediaByType(contentType)
	if hasType && typeMedia == "html" && hasExt && extMedia != "html" {
		return "html", true
	}
	if hasExt {
		return extMedia, true
	}
	if hasType {
		return typeMedia, true
	}
	return sniffMedia(body)
}

func mediaByExt(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	return mediaByExtName(path.Ext(u.Path))
}

func mediaByExtName(ext string) (string, bool) {
	switch strings.ToLower(ext) {
	case ".pdf":
		return "pdf", true
	case ".docx":
		return "docx", true
	case ".html", ".htm":
		return "html", true
	case ".txt":
		return "txt", true
	case ".md", ".markdown":
		return "md", true
	default:
		return "", false
	}
}

func mediaByType(contentType string) (string, bool) {
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		return "", false
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mt = contentType
		if i := strings.IndexByte(mt, ';'); i >= 0 {
			mt = mt[:i]
		}
		mt = strings.TrimSpace(mt)
	}
	switch strings.ToLower(mt) {
	case "application/pdf":
		return "pdf", true
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "docx", true
	case "text/html", "application/xhtml+xml":
		return "html", true
	case "text/plain":
		return "txt", true
	case "text/markdown", "text/x-markdown", "application/markdown":
		return "md", true
	default:
		return "", false
	}
}

func sniffMedia(body []byte) (string, bool) {
	b := bytes.TrimSpace(body)
	if bytes.HasPrefix(b, []byte("%PDF")) {
		return "pdf", true
	}
	if len(b) >= 4 && b[0] == 'P' && b[1] == 'K' {
		window := b
		if len(window) > 4096 {
			window = window[:4096]
		}
		if bytes.Contains(window, []byte("word/")) {
			return "docx", true
		}
	}
	if looksLikeHTML(b) {
		return "html", true
	}
	return "", false
}

func looksLikeHTML(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	sample := raw
	if len(sample) > 1024 {
		sample = sample[:1024]
	}
	s := strings.ToLower(string(bytes.TrimSpace(sample)))
	if strings.HasPrefix(s, "<!doctype html") || strings.HasPrefix(s, "<html") {
		return true
	}
	return strings.Contains(s, "<html") || strings.Contains(s, "<head") || strings.Contains(s, "<body")
}

func unsupportedExt(ext string) bool {
	ext = strings.ToLower(ext)
	if ext == "" || ext == ".xml" {
		return false
	}
	if _, ok := mediaByExtName(ext); ok {
		return false
	}
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".ico",
		".zip", ".gz", ".tgz", ".rar", ".7z",
		".css", ".js", ".mjs", ".json",
		".mp3", ".mp4", ".wav", ".webm", ".ogg",
		".woff", ".woff2", ".ttf", ".eot",
		".doc", ".ppt", ".pptx", ".xls", ".xlsx", ".csv", ".rtf", ".odt":
		return true
	default:
		return false
	}
}

func mimeOfMedia(media string) string {
	switch media {
	case "pdf":
		return "application/pdf"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "html":
		return "text/html; charset=utf-8"
	case "txt":
		return "text/plain; charset=utf-8"
	case "md":
		return "text/markdown; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func licenseFromHeaders(h http.Header) string {
	if len(h) == 0 {
		return ""
	}
	if v := strings.TrimSpace(h.Get("License")); v != "" {
		return v
	}
	for _, value := range h.Values("Link") {
		if lic := licenseLink(value); lic != "" {
			return lic
		}
	}
	return ""
}

func licenseLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "<") {
			continue
		}
		end := strings.IndexByte(part, '>')
		if end <= 1 {
			continue
		}
		rest := strings.ToLower(part[end:])
		if strings.Contains(rest, "rel=\"license\"") || strings.Contains(rest, "rel='license'") || strings.Contains(rest, "rel=license") {
			return part[1:end]
		}
	}
	return ""
}

func inspectHTML(base *url.URL, body []byte) (title, license, visible string, links []string) {
	if len(body) == 0 || base == nil {
		return "", "", "", nil
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil || doc == nil {
		return "", "", "", nil
	}
	pageBase := base
	var findBase func(*html.Node)
	findBase = func(n *html.Node) {
		if n.Type == html.ElementNode && strings.EqualFold(n.Data, "base") {
			if href := strings.TrimSpace(attr(n, "href")); href != "" {
				if ref, err := url.Parse(href); err == nil {
					pageBase = base.ResolveReference(ref)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findBase(c)
		}
	}
	findBase(doc)

	seen := map[string]struct{}{}
	var text strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch strings.ToLower(n.Data) {
			case "script", "style":
				return
			case "title":
				if title == "" {
					title = nodeText(n)
				}
			case "meta":
				if license == "" && isLicenseMeta(n) {
					license = strings.TrimSpace(attr(n, "content"))
				}
			case "link":
				if license == "" && strings.Contains(strings.ToLower(attr(n, "rel")), "license") {
					if href := strings.TrimSpace(attr(n, "href")); href != "" {
						if abs, ok := normalize(href, pageBase); ok {
							license = abs
						} else {
							license = href
						}
					}
				}
			case "a":
				if href := strings.TrimSpace(attr(n, "href")); href != "" {
					if abs, ok := normalize(href, pageBase); ok {
						if _, dup := seen[abs]; !dup {
							seen[abs] = struct{}{}
							links = append(links, abs)
						}
					}
				}
			}
		}
		if n.Type == html.TextNode {
			text.WriteString(n.Data)
			text.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return strings.TrimSpace(title), license, text.String(), links
}

func isLicenseMeta(n *html.Node) bool {
	name := strings.ToLower(attr(n, "name"))
	if name == "" {
		name = strings.ToLower(attr(n, "property"))
	}
	switch name {
	case "license", "dcterms.license", "dc.rights":
		return true
	default:
		return false
	}
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(strings.Join(strings.Fields(b.String()), " "))
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func normalize(raw string, base *url.URL) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return "", false
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "mailto:") || strings.HasPrefix(lower, "data:") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return "", false
	}
	u.Fragment = ""
	return u.String(), true
}

func canonURL(raw string) string {
	if abs, ok := normalize(raw, nil); ok {
		return abs
	}
	return raw
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	if r == nil || max <= 0 {
		return nil, nil
	}
	buf := make([]byte, storage.CopyBufSize)
	var out bytes.Buffer
	var got int64
	for got < max {
		want := int64(len(buf))
		if remain := max - got; remain < want {
			want = remain
		}
		n, err := r.Read(buf[:want])
		if n > 0 {
			out.Write(buf[:n])
			got += int64(n)
		}
		if err == io.EOF {
			return out.Bytes(), nil
		}
		if err != nil {
			return out.Bytes(), err
		}
	}
	return out.Bytes(), nil
}

type limitedReadCloser struct {
	rc   io.ReadCloser
	left int64
}

func (b *limitedReadCloser) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	if len(p) > storage.CopyBufSize {
		p = p[:storage.CopyBufSize]
	}
	if len(p) == 0 {
		return 0, io.EOF
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedReadCloser) Close() error {
	if b.rc == nil {
		return nil
	}
	return b.rc.Close()
}

func isCtxErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

var _ Crawler = (*crawler)(nil)
