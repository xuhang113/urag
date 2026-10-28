package retrieve

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/xuhang113/urag/internal/embed"
)

//go:embed cross_rerank.py
var crossRerankScript string

type crossEncoder struct {
	dir  string
	name string
	argv []string
}

// NewCrossEncoderReranker 只从 dir 读取本地 cross-encoder 权重。
// 目录必须是 sequence classification 配置，并且权重和分词器都在本机。
// 不访问网络，不下载，也不加载嵌入模型。前向在 Score 里另起一个子进程。
func NewCrossEncoderReranker(dir string) (PassageReranker, error) {
	return NewCrossEncoderRerankerCommand(dir, nil)
}

// NewCrossEncoderRerankerCommand 与 NewCrossEncoderReranker 做同样的权重检查。
// argv 为空时用本机 python3。测试传入假命令，不加载真实模型。
func NewCrossEncoderRerankerCommand(dir string, argv []string) (PassageReranker, error) {
	if argv != nil && len(argv) == 0 {
		return nil, fmt.Errorf("retrieve: empty rerank command")
	}
	info, err := InspectReranker(dir)
	if err != nil {
		return nil, err
	}
	if info.Kind != KindCrossEncoder {
		return nil, fmt.Errorf("retrieve: reranker in %q is not a cross-encoder, refusing to download", info.Dir)
	}
	if err := embed.WeightsReady(info.Dir, "retrieve: cross encoder"); err != nil {
		return nil, err
	}
	if err := embed.TokenizerReady(info.Dir, "retrieve: cross encoder"); err != nil {
		return nil, err
	}
	return crossEncoder{dir: info.Dir, name: info.Name, argv: append([]string(nil), argv...)}, nil
}

func (c crossEncoder) Score(ctx context.Context, query string, passages []string) ([]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(passages) == 0 {
		return []float64{}, nil
	}
	if len(passages) > embed.MaxForwardBatch {
		return nil, fmt.Errorf("retrieve: batch %d exceeds %d, refusing to keep that many scores in memory", len(passages), embed.MaxForwardBatch)
	}
	argv, err := c.command()
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(struct {
		Task     string   `json:"task"`
		Model    string   `json:"model"`
		ModelDir string   `json:"model_dir"`
		Query    string   `json:"query"`
		Passages []string `json:"passages"`
	}{
		Task:     "rerank",
		Model:    c.name,
		ModelDir: c.dir,
		Query:    rerankQuestion(query),
		Passages: passages,
	})
	if err != nil {
		return nil, err
	}
	out, err := finishRerank(ctx, c.name, argv, payload)
	if err != nil {
		return nil, err
	}
	return parseRerankScores(out, len(passages), "cross encoder")
}

func (c crossEncoder) command() ([]string, error) {
	if len(c.argv) > 0 {
		return c.argv, nil
	}
	return embed.PythonCommand("cross_rerank.py", crossRerankScript)
}

func rerankQuestion(query string) string {
	if strings.HasPrefix(query, embed.QueryInstruct) {
		return query[len(embed.QueryInstruct):]
	}
	return query
}

func parseRerankScores(out []byte, n int, label string) ([]float64, error) {
	var parsed struct {
		Scores []float64 `json:"scores"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("retrieve: %s rerank output: %w", label, err)
	}
	if len(parsed.Scores) != n {
		return nil, fmt.Errorf("retrieve: %s rerank returned %d scores for %d passages", label, len(parsed.Scores), n)
	}
	for _, score := range parsed.Scores {
		if math.IsNaN(score) {
			return nil, fmt.Errorf("retrieve: %s rerank returned NaN", label)
		}
	}
	return parsed.Scores, nil
}
