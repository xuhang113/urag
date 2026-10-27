package acquire

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
)

type httpSource struct {
	rawURL string
	client *http.Client
}

// NewHTTP 准备 rawURL 上的 HTTP 来源。Kind 为 KindHTTP。
// 只接受 http 和 https。其他协议、无法解析的地址或没有主机名时返回错误，不拨号。
// List 返回一条，SourceURI 是调用方传入的 rawURL，Filename 是 URL 路径的基名。List 本身不发请求。
// Open 用 client 发出带 context 的 GET，调用方负责关闭正文。client 为 nil 时使用 http.DefaultClient。
// 响应状态不是 2xx 时返回错误，并关闭响应体。
func NewHTTP(rawURL string, client *http.Client) (Source, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("acquire: scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("acquire: http url %q has no host", rawURL)
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &httpSource{rawURL: rawURL, client: client}, nil
}

func (h *httpSource) Kind() string { return KindHTTP }

func (h *httpSource) List(ctx context.Context) ([]Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := filenameFromURL(h.rawURL)
	return []Item{{
		SourceURI:   h.rawURL,
		Filename:    name,
		ContentType: contentTypeOf(name),
		Open:        h.open,
	}}, nil
}

func (h *httpSource) open(ctx context.Context) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("acquire: GET %s: %s", h.rawURL, resp.Status)
	}
	return resp.Body, nil
}

func filenameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "body"
	}
	base := path.Base(u.Path)
	if base == "." || base == "/" || base == "" {
		return "body"
	}
	return base
}
