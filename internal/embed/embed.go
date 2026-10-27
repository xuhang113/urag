// Package embed 定义文本向量化契约。
// 本包不切分文档，也不建索引。密钥只通过环境变量名引用，禁止写入日志。
package embed

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// Vector 是一条向量。夹具嵌入器产出的长度等于 Dim。
type Vector []float32

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

// Tokens 按非字母切词，并把每个词转成小写。
// 连续的字母算一个词，标点、数字和空白都是分隔符。空文本得到空切片。
func Tokens(text string) []string {
	var b strings.Builder
	var out []string
	flush := func() {
		if b.Len() == 0 {
			return
		}
		out = append(out, strings.ToLower(b.String()))
		b.Reset()
	}
	for _, r := range text {
		if unicode.IsLetter(r) {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

type fixtureEmbedder struct {
	dim int
}

// NewFixture 返回确定性的夹具嵌入器，供测试手算。
// dim 小于等于 0 时使用 64。Dim 返回这个维度。
// 算法固定为：Tokens 切词；对每个词计算 FNV-64a，下标取 hash % dim，该维加 1；再做 L2 归一化。
// 空文本，或切词后没有词，得到零向量。相同文本得到相同向量。不访问网络。
func NewFixture(dim int) Embedder {
	if dim <= 0 {
		dim = 64
	}
	return fixtureEmbedder{dim: dim}
}

func (f fixtureEmbedder) Dim() int { return f.dim }

func (f fixtureEmbedder) Embed(ctx context.Context, texts []string) ([]Vector, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return []Vector{}, nil
	}
	out := make([]Vector, len(texts))
	for i, text := range texts {
		out[i] = f.embedOne(text)
	}
	return out, nil
}

func (f fixtureEmbedder) embedOne(text string) Vector {
	vec := make(Vector, f.dim)
	for _, tok := range Tokens(text) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(tok))
		vec[int(h.Sum64()%uint64(f.dim))]++
	}
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return vec
	}
	norm := math.Sqrt(sum)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}
