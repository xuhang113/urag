package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
)

// 单次响应最多读取 32 MiB，避免异常端点撑满内存。
const maxResponseBytes = 32 << 20

type httpEmbedder struct {
	cfg Config
	do  func(req *http.Request) (*http.Response, error)

	mu  sync.Mutex
	dim int
}

type embedRequest struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

type embedResponse struct {
	Vectors [][]float32 `json:"vectors"`
}

// NewHTTP 返回按批调用端点的嵌入器。
// 批大小取 Config.BatchSize，小于等于 0 时为 32。并发取 Config.Concurrency，小于等于 0 时为 2。
// do 发送请求。APIKeyEnv 为空时使用 URAG_EMBED_API_KEY。密钥从该环境变量读取，放在 Authorization: Bearer。
// 密钥不得出现在错误字符串里，也不写入日志。端点失败时 Embed 返回错误。空输入返回空切片，不调用 do。
// 尚未成功嵌入时 Dim 返回 0；成功后返回向量维度。
func NewHTTP(cfg Config, do func(req *http.Request) (*http.Response, error)) Embedder {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 32
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	if cfg.APIKeyEnv == "" {
		cfg.APIKeyEnv = "URAG_EMBED_API_KEY"
	}
	return &httpEmbedder{cfg: cfg, do: do}
}

func (h *httpEmbedder) Dim() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dim
}

func (h *httpEmbedder) Embed(ctx context.Context, texts []string) ([]Vector, error) {
	if len(texts) == 0 {
		return []Vector{}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h.do == nil {
		return nil, errors.New("embed: nil transport")
	}
	secret := os.Getenv(h.cfg.APIKeyEnv)

	type job struct {
		start int
		texts []string
	}
	jobs := make([]job, 0, (len(texts)+h.cfg.BatchSize-1)/h.cfg.BatchSize)
	for start := 0; start < len(texts); start += h.cfg.BatchSize {
		end := start + h.cfg.BatchSize
		if end > len(texts) {
			end = len(texts)
		}
		jobs = append(jobs, job{start: start, texts: texts[start:end]})
	}

	out := make([]Vector, len(texts))
	sem := make(chan struct{}, h.cfg.Concurrency)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var first error
	record := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		defer errMu.Unlock()
		if first == nil {
			first = scrub(err, secret)
		}
	}

	var dimMu sync.Mutex
	gotDim := 0
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				record(ctx.Err())
				return
			}
			defer func() { <-sem }()

			vecs, dim, err := h.post(ctx, j.texts, secret)
			if err != nil {
				record(err)
				return
			}
			dimMu.Lock()
			if gotDim == 0 {
				gotDim = dim
			} else if dim != gotDim {
				dimMu.Unlock()
				record(fmt.Errorf("embed: dimension %d, want %d", dim, gotDim))
				return
			}
			dimMu.Unlock()
			copy(out[j.start:j.start+len(vecs)], vecs)
		}(j)
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	if gotDim == 0 {
		return nil, errors.New("embed: empty vectors")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dim == 0 {
		h.dim = gotDim
	} else if h.dim != gotDim {
		return nil, fmt.Errorf("embed: dimension %d, want %d", gotDim, h.dim)
	}
	return out, nil
}

func (h *httpEmbedder) post(ctx context.Context, texts []string, secret string) ([]Vector, int, error) {
	payload, err := json.Marshal(embedRequest{Input: texts, Model: h.cfg.Model})
	if err != nil {
		return nil, 0, scrub(err, secret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, scrub(err, secret)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)

	resp, err := h.do(req)
	if err != nil {
		return nil, 0, scrub(err, secret)
	}
	if resp == nil {
		return nil, 0, errors.New("embed: nil response")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, 0, scrub(err, secret)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		safe := body
		if secret != "" {
			safe = bytes.ReplaceAll(body, []byte(secret), []byte("redacted"))
		}
		return nil, 0, fmt.Errorf("embed: status %d: %s", resp.StatusCode, safe)
	}

	var parsed embedResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, 0, scrub(err, secret)
	}
	if len(parsed.Vectors) != len(texts) {
		return nil, 0, fmt.Errorf("embed: got %d vectors for %d inputs", len(parsed.Vectors), len(texts))
	}
	dim := 0
	out := make([]Vector, len(parsed.Vectors))
	for i, raw := range parsed.Vectors {
		if len(raw) == 0 {
			return nil, 0, errors.New("embed: empty vector")
		}
		if dim == 0 {
			dim = len(raw)
		} else if len(raw) != dim {
			return nil, 0, fmt.Errorf("embed: vector length %d, want %d", len(raw), dim)
		}
		out[i] = append(Vector(nil), raw...)
	}
	return out, dim, nil
}

// scrub 去掉错误文本中的密钥。空密钥不能替换，否则会改写每个字符之间的内容。
func scrub(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, secret, "redacted"))
}
