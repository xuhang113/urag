package acquire_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuhang113/urag/internal/acquire"
	"github.com/xuhang113/urag/internal/storage"
)

func TestCrawlSitemapRobotsAuthAndDone(t *testing.T) {
	book := newHitBook()
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		_, _ = io.WriteString(w, strings.Join([]string{
			"User-agent: otherbot",
			"Disallow: /",
			"Crawl-delay: 10",
			"",
			"User-agent: *",
			"Disallow: /secret/",
			"Allow: /secret/public.pdf",
			"",
		}, "\n"))
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>http://`+r.Host+`/child.xml</loc></sitemap>
</sitemapindex>`)
	})
	mux.HandleFunc("/child.xml", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Content-Type", "application/xml")
		host := "http://" + r.Host
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>`+host+`/a.pdf</loc></url>
  <url><loc>`+host+`/b.html</loc></url>
  <url><loc>`+host+`/a.pdf</loc></url>
  <url><loc>`+host+`/old.pdf</loc></url>
  <url><loc>`+host+`/secret/c.pdf</loc></url>
  <url><loc>`+host+`/secret/public.pdf</loc></url>
  <url><loc>`+host+`/locked.pdf</loc></url>
  <url><loc>`+host+`/done.pdf</loc></url>
  <url><loc>`+host+`/pic.png</loc></url>
  <url><loc>`+host+`/captcha.html</loc></url>
  <url><loc>`+host+`/paid.pdf</loc></url>
  <url><loc>`+host+`/missing.pdf</loc></url>
  <url><loc>`+host+`/forbid.pdf</loc></url>
  <url><loc>`+host+`/member.html</loc></url>
</urlset>`)
	})
	pdfBody := []byte("pdf-body")
	mux.HandleFunc("/a.pdf", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Content-Type", "application/pdf")
		if r.Method == http.MethodGet {
			_, _ = w.Write(pdfBody)
		}
	})
	mux.HandleFunc("/old.pdf", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		http.Redirect(w, r, "/a.pdf", http.StatusFound)
	})
	mux.HandleFunc("/b.html", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("License", "CC-BY-4.0")
		_, _ = io.WriteString(w, "<html><head><title>公开笔记</title></head><body><p>正文</p></body></html>")
	})
	mux.HandleFunc("/secret/public.pdf", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Content-Type", "application/pdf")
	})
	mux.HandleFunc("/secret/c.pdf", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		t.Errorf("robots 拒绝的地址被请求了")
	})
	mux.HandleFunc("/locked.pdf", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Set-Cookie", "session=secret")
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/done.pdf", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		t.Errorf("已完成的地址被请求了")
	})
	mux.HandleFunc("/pic.png", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		t.Errorf("不支持的类型被请求了")
	})
	mux.HandleFunc("/captcha.html", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><head><title>检查</title></head><body><p>captcha</p></body></html>")
	})
	mux.HandleFunc("/paid.pdf", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.WriteHeader(http.StatusPaymentRequired)
	})
	mux.HandleFunc("/missing.pdf", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/forbid.pdf", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/member.html", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><body><p>paywall</p></body></html>")
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := loopbackClient(t, srv)
	doneURL := srv.URL + "/done.pdf"
	cp := acquire.Checkpoint{Done: []string{doneURL}}
	crawler := acquire.NewCrawler(client, acquire.CrawlLimits{PerHostInterval: 0}).WithDone(cp.Done)

	start := time.Now()
	found, skips, err := crawler.Crawl(context.Background(), []string{srv.URL + "/sitemap.xml"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Crawl：%v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("间隔为 0 时收录用了 %s", elapsed)
	}

	byURI := indexFound(found)
	if len(found) != 3 || len(byURI) != 3 {
		t.Fatalf("found = %+v", urlsOf(found))
	}
	pdf := byURI[srv.URL+"/a.pdf"]
	if pdf.MediaType != "pdf" || pdf.Title != "" || pdf.License != "" || pdf.Open == nil {
		t.Fatalf("pdf = %+v", pdf)
	}
	page := byURI[srv.URL+"/b.html"]
	if page.MediaType != "html" || page.Title != "公开笔记" || page.License != "CC-BY-4.0" {
		t.Fatalf("html = %+v", page)
	}
	if _, ok := byURI[srv.URL+"/secret/public.pdf"]; !ok {
		t.Fatalf("Allow 覆盖的文档没有列入：%v", urlsOf(found))
	}
	if _, ok := byURI[srv.URL+"/old.pdf"]; ok {
		t.Fatal("重定向前的地址不应作为 SourceURI")
	}

	reasons := indexSkips(skips)
	wantSkips := map[string]string{
		srv.URL + "/secret/c.pdf": reasonRobots,
		srv.URL + "/locked.pdf":   "login",
		srv.URL + "/pic.png":      "unsupported",
		srv.URL + "/captcha.html": "captcha",
		srv.URL + "/paid.pdf":     "payment",
		srv.URL + "/missing.pdf":  "download",
		srv.URL + "/forbid.pdf":   "login",
		srv.URL + "/member.html":  "payment",
	}
	if len(skips) != len(wantSkips) {
		t.Fatalf("skips = %+v", skips)
	}
	for uri, reason := range wantSkips {
		if reasons[uri] != reason {
			t.Fatalf("%s 原因 = %q，应为 %s；全部 %+v", uri, reasons[uri], reason, skips)
		}
	}
	if _, listed := byURI[doneURL]; listed || reasons[doneURL] != "" {
		t.Fatalf("已完成地址仍出现：found=%v skips=%v", urlsOf(found), skips)
	}
	if book.count("/done.pdf") != 0 || book.count("/secret/c.pdf") != 0 || book.count("/pic.png") != 0 {
		t.Fatalf("不该发生的请求：%v", book.snapshot())
	}
	if book.count("/locked.pdf") != 1 {
		t.Fatalf("401 请求了 %d 次", book.count("/locked.pdf"))
	}
	if !containsMethod(book.methods("/a.pdf"), http.MethodHead) || containsMethod(book.methods("/a.pdf"), http.MethodGet) {
		t.Fatalf("收录阶段应 HEAD pdf，实际 %v", book.methods("/a.pdf"))
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pdf.Open(cancelCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后的 Open = %v", err)
	}
	body, err := pdf.Open(context.Background())
	if err != nil {
		t.Fatalf("Open：%v", err)
	}
	defer body.Close()
	var got bytes.Buffer
	if _, err := io.CopyBuffer(&got, body, make([]byte, storage.CopyBufSize)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), pdfBody) {
		t.Fatalf("Open 读到 %q", got.Bytes())
	}
	if !containsMethod(book.methods("/a.pdf"), http.MethodGet) {
		t.Fatalf("Open 没有 GET：%v", book.methods("/a.pdf"))
	}
}

func TestCrawlDirectoryPage(t *testing.T) {
	book := newHitBook()
	const docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	docxBody := []byte("docx-body")
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/dir.html", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>目录</title>
<meta name="license" content="CC0"><base href="/files/"></head><body>
<a href="a.docx">word</a>
<a href="b.txt">txt</a>
<a href="c.md">md</a>
<a href="d.png">img</a>
<a href="mailto:a@example.com">mail</a>
</body></html>`)
	})
	mux.HandleFunc("/files/a.docx", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Content-Type", docxType)
		if r.Method == http.MethodGet {
			_, _ = w.Write(docxBody)
		}
	})
	mux.HandleFunc("/files/b.txt", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "txt-body")
		}
	})
	mux.HandleFunc("/files/c.md", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	})
	mux.HandleFunc("/files/d.png", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		t.Errorf("图片被请求了")
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	found, skips, err := acquire.NewCrawler(loopbackClient(t, srv), acquire.CrawlLimits{PerHostInterval: 0}).
		Crawl(context.Background(), []string{srv.URL + "/dir.html"})
	if err != nil {
		t.Fatalf("Crawl：%v", err)
	}
	byURI := indexFound(found)
	if len(found) != 4 {
		t.Fatalf("found = %v", urlsOf(found))
	}
	dir := byURI[srv.URL+"/dir.html"]
	if dir.MediaType != "html" || dir.Title != "目录" || dir.License != "CC0" {
		t.Fatalf("目录页 = %+v", dir)
	}
	if byURI[srv.URL+"/files/a.docx"].MediaType != "docx" ||
		byURI[srv.URL+"/files/b.txt"].MediaType != "txt" ||
		byURI[srv.URL+"/files/c.md"].MediaType != "md" {
		t.Fatalf("文档类型不对：%+v", byURI)
	}
	reasons := indexSkips(skips)
	if len(skips) != 1 || reasons[srv.URL+"/files/d.png"] != "unsupported" {
		t.Fatalf("skips = %+v", skips)
	}
	if book.count("/files/d.png") != 0 {
		t.Fatal("不支持的文件被请求了")
	}
	if containsMethod(book.methods("/files/a.docx"), http.MethodGet) {
		t.Fatalf("收录阶段下载了 docx：%v", book.methods("/files/a.docx"))
	}
	body, err := byURI[srv.URL+"/files/a.docx"].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	var got bytes.Buffer
	if _, err := io.CopyBuffer(&got, body, make([]byte, storage.CopyBufSize)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), docxBody) {
		t.Fatalf("docx = %q", got.Bytes())
	}
}

func TestCrawlDelayOverridesInterval(t *testing.T) {
	var mu sync.Mutex
	var stamps []time.Time
	mark := func() {
		mu.Lock()
		stamps = append(stamps, time.Now())
		mu.Unlock()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		mark()
		_, _ = io.WriteString(w, "User-agent: *\nCrawl-delay: 0.2\n")
	})
	mux.HandleFunc("/page.html", func(w http.ResponseWriter, r *http.Request) {
		mark()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><head><title>延迟</title></head><body>ok</body></html>")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	found, skips, err := acquire.NewCrawler(loopbackClient(t, srv), acquire.CrawlLimits{PerHostInterval: 0}).
		Crawl(context.Background(), []string{srv.URL + "/page.html"})
	if err != nil {
		t.Fatalf("Crawl：%v", err)
	}
	if len(skips) != 0 || len(found) != 1 || found[0].Title != "延迟" || found[0].MediaType != "html" {
		t.Fatalf("found=%+v skips=%+v", found, skips)
	}
	if len(stamps) < 2 {
		t.Fatalf("请求次数 %d", len(stamps))
	}
	gap := stamps[1].Sub(stamps[0])
	if gap < 150*time.Millisecond || gap > 900*time.Millisecond {
		t.Fatalf("Crawl-delay 间隔 = %s", gap)
	}
}

func TestCrawlCancelReturnsPartial(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>http://`+r.Host+`/a.pdf</loc></url>
  <url><loc>http://`+r.Host+`/slow.pdf</loc></url>
</urlset>`)
	})
	mux.HandleFunc("/a.pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
	})
	mux.HandleFunc("/slow.pdf", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var found []acquire.Found
	var err error
	go func() {
		defer close(done)
		found, _, err = acquire.NewCrawler(loopbackClient(t, srv), acquire.CrawlLimits{PerHostInterval: 0}).
			Crawl(ctx, []string{srv.URL + "/sitemap.xml"})
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("slow.pdf 没有开始")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 Crawl 没有返回")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(found) != 1 || found[0].SourceURI != srv.URL+"/a.pdf" || found[0].MediaType != "pdf" {
		t.Fatalf("found = %+v", found)
	}
}

func TestNewSitemapListsDocumentURLs(t *testing.T) {
	if _, err := acquire.NewSitemap("ftp://127.0.0.1/sitemap.xml"); err == nil {
		t.Fatal("ftp 不应被接受")
	}
	if _, err := acquire.NewSitemap("http:///sitemap.xml"); err == nil {
		t.Fatal("没有主机名不应被接受")
	}

	book := newHitBook()
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		_, _ = io.WriteString(w, "User-agent: *\nDisallow: /secret/\n")
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>http://`+r.Host+`/child.xml</loc></sitemap>
</sitemapindex>`)
	})
	mux.HandleFunc("/child.xml", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Content-Type", "application/xml")
		host := "http://" + r.Host
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>`+host+`/a.pdf</loc></url>
  <url><loc>`+host+`/b.html</loc></url>
  <url><loc>`+host+`/a.pdf</loc></url>
  <url><loc>`+host+`/note.txt</loc></url>
  <url><loc>`+host+`/secret/c.pdf</loc></url>
  <url><loc>`+host+`/pic.png</loc></url>
</urlset>`)
	})
	pdfBody := []byte("sitemap-pdf")
	mux.HandleFunc("/a.pdf", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfBody)
	})
	mux.HandleFunc("/b.html", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		t.Errorf("List 下载了 html")
	})
	mux.HandleFunc("/note.txt", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		t.Errorf("List 下载了 txt")
	})
	mux.HandleFunc("/secret/c.pdf", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		t.Errorf("List 请求了 robots 拒绝的地址")
	})
	mux.HandleFunc("/pic.png", func(w http.ResponseWriter, r *http.Request) {
		book.add(r)
		t.Errorf("List 请求了不支持的类型")
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	guardDefaultTransport(t)

	src, err := acquire.NewSitemap(srv.URL + "/sitemap.xml")
	if err != nil {
		t.Fatalf("NewSitemap：%v", err)
	}
	if src.Kind() != acquire.KindSitemap {
		t.Fatalf("Kind = %q", src.Kind())
	}
	items, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List：%v", err)
	}
	if book.count("/a.pdf") != 0 || book.count("/b.html") != 0 || book.count("/note.txt") != 0 {
		t.Fatalf("List 下载了文档：%v", book.snapshot())
	}
	got := map[string]string{}
	for _, item := range items {
		got[item.SourceURI] = item.ContentType
		if item.Open == nil || item.Filename == "" {
			t.Fatalf("条目不完整：%+v", item)
		}
	}
	if len(items) != 3 || len(got) != 3 {
		t.Fatalf("items = %+v", items)
	}
	if !strings.Contains(got[srv.URL+"/a.pdf"], "pdf") ||
		!strings.Contains(got[srv.URL+"/b.html"], "html") ||
		!strings.Contains(got[srv.URL+"/note.txt"], "text/plain") {
		t.Fatalf("内容类型 = %+v", got)
	}
	var pdf acquire.Item
	for _, item := range items {
		if item.SourceURI == srv.URL+"/a.pdf" {
			pdf = item
		}
	}
	body, err := pdf.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	buf := make([]byte, storage.CopyBufSize)
	var out bytes.Buffer
	if _, err := io.CopyBuffer(&out, body, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), pdfBody) {
		t.Fatalf("Open = %q", out.Bytes())
	}
}

func loopbackClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	client := srv.Client()
	base := client.Transport
	client.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		ip := net.ParseIP(r.URL.Hostname())
		if ip == nil || !ip.IsLoopback() {
			t.Errorf("请求了回环以外的地址 %s", r.URL.String())
			return nil, errors.New("acquire test: non-loopback")
		}
		if cookie := r.Header.Get("Cookie"); cookie != "" {
			t.Errorf("请求带了 Cookie：%s", cookie)
		}
		return base.RoundTrip(r)
	})
	return client
}

func guardDefaultTransport(t *testing.T) {
	t.Helper()
	orig := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		ip := net.ParseIP(r.URL.Hostname())
		if ip == nil || !ip.IsLoopback() {
			t.Errorf("请求了回环以外的地址 %s", r.URL.String())
			return nil, errors.New("acquire test: non-loopback")
		}
		return orig.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = orig })
}

func indexFound(found []acquire.Found) map[string]acquire.Found {
	out := make(map[string]acquire.Found, len(found))
	for _, item := range found {
		out[item.SourceURI] = item
	}
	return out
}

func indexSkips(skips []acquire.Skip) map[string]string {
	out := make(map[string]string, len(skips))
	for _, item := range skips {
		out[item.URI] = item.Reason
	}
	return out
}

func urlsOf(found []acquire.Found) []string {
	out := make([]string, len(found))
	for i, item := range found {
		out[i] = item.SourceURI
	}
	return out
}

func containsMethod(methods []string, want string) bool {
	for _, method := range methods {
		if method == want {
			return true
		}
	}
	return false
}

type hitBook struct {
	mu     sync.Mutex
	hits   map[string]int
	method map[string][]string
	paths  []string
}

func newHitBook() *hitBook {
	return &hitBook{
		hits:   map[string]int{},
		method: map[string][]string{},
	}
}

func (b *hitBook) add(r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hits[r.URL.Path]++
	b.method[r.URL.Path] = append(b.method[r.URL.Path], r.Method)
	b.paths = append(b.paths, r.Method+" "+r.URL.Path)
}

func (b *hitBook) count(path string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hits[path]
}

func (b *hitBook) methods(path string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.method[path]...)
}

func (b *hitBook) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.paths...)
}

const reasonRobots = "robots"
