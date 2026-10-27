// Package embed 定义文本向量化契约。
// 本包不切分文档，也不建索引。密钥只通过环境变量名引用，禁止写入日志。
package embed

import (
	"context"
	"errors"
	"net/http"
)

// Vector 是一条向量。夹具嵌入器产出的长度等于 Dim。
type Vector []float32

var errNotImplemented = errors.New("embed: not implemented")

// Embedder 把一批文本变成向量。
// Embed 的结果与 texts 等长、顺序相同。texts 为空时得到空切片。
// 失败时返回错误，不返回缺维的向量。
type Embedder interface {
	// Dim 返回向量维度。
	Dim() int
	// Embed 向量化 texts。
	Embed(ctx context.Context, texts []string) ([]Vector, error)
}

// Config 是 HTTP 嵌入器的调用参数。
type Config struct {
	BatchSize   int
	Concurrency int
	Endpoint    string
	Model       string
	APIKeyEnv   string // 只存变量名，默认 URAG_EMBED_API_KEY
}

type fixtureEmbedder struct {
	dim int
}

// NewFixture 返回确定性的夹具嵌入器，供测试手算。
// dim 小于等于 0 时使用 64。Dim 返回这个维度。
// 接通后的算法固定为：按非字母切词并转成小写；对每个词计算 FNV-64a，
// 下标取 hash % dim，该维加 1；再做 L2 归一化。空文本得到零向量。
// 当前 Embed 返回未实现错误，不访问网络。
func NewFixture(dim int) Embedder {
	if dim <= 0 {
		dim = 64
	}
	return fixtureEmbedder{dim: dim}
}

func (f fixtureEmbedder) Dim() int { return f.dim }

func (fixtureEmbedder) Embed(context.Context, []string) ([]Vector, error) {
	return nil, errNotImplemented
}

type httpEmbedder struct {
	cfg Config
	do  func(req *http.Request) (*http.Response, error)
}

// NewHTTP 返回按批调用端点的嵌入器。
// 批大小取 Config.BatchSize，并发取 Config.Concurrency。
// do 是接通后发送请求的函数。APIKeyEnv 为空时保存 URAG_EMBED_API_KEY。
// 禁止在日志里输出密钥。端点失败时 Embed 返回该错误。
// 当前 Embed 返回未实现错误，不会调用 do。接通前 Dim 返回 0。
func NewHTTP(cfg Config, do func(req *http.Request) (*http.Response, error)) Embedder {
	if cfg.APIKeyEnv == "" {
		cfg.APIKeyEnv = "URAG_EMBED_API_KEY"
	}
	return httpEmbedder{cfg: cfg, do: do}
}

func (httpEmbedder) Dim() int { return 0 }

func (httpEmbedder) Embed(context.Context, []string) ([]Vector, error) {
	return nil, errNotImplemented
}
