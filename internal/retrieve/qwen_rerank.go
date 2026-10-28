package retrieve

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/xuhang113/urag/internal/embed"
)

// QwenRerankerName 是本地重排模型。它和嵌入模型分开加载。
const QwenRerankerName = "Qwen/Qwen3-Reranker-4B"

// rerankTask 与嵌入指令是同一句。
// 本机没有 Qwen3-Reranker 模型卡，不联网读取。下面按嵌入模型卡的拼法使用：
// Instruct: Given a web search query, retrieve relevant passages that answer the query
// Query: 后面直接接问题，不加空格。
const rerankTask = "Given a web search query, retrieve relevant passages that answer the query"

//go:embed qwen_rerank.py
var qwenRerankScript string

// PassageReranker 给一批子块打分。
// 分数与 passages 等长、顺序相同。只产出分数，不生成答案，也不加载嵌入模型。
type PassageReranker interface {
	Score(ctx context.Context, query string, passages []string) ([]float64, error)
}

type qwenReranker struct {
	dir  string
	name string
	argv []string
}

// NewQwenReranker 只从 dir 读取本地重排权重。
// 目录不存在、hidden_size 不是 2560 或缺少权重文件时返回错误。
// 不访问网络，不下载，也不加载嵌入模型。前向在 Score 里另起一个子进程。
func NewQwenReranker(dir string) (PassageReranker, error) {
	return NewQwenRerankerCommand(dir, nil)
}

// NewQwenRerankerCommand 与 NewQwenReranker 做同样的权重检查。
// argv 为空时用本机 python3。测试传入假命令，不加载真实 4B。
func NewQwenRerankerCommand(dir string, argv []string) (PassageReranker, error) {
	if argv != nil && len(argv) == 0 {
		return nil, fmt.Errorf("retrieve: empty rerank command")
	}
	info, err := InspectReranker(dir)
	if err != nil {
		return nil, err
	}
	if info.Kind != KindQwen4B {
		hidden := info.Hidden
		if hidden == 0 {
			hidden = -1
		}
		return nil, fmt.Errorf("retrieve: qwen reranker hidden size %d, want 2560, refusing to change dimension", hidden)
	}
	if err := embed.WeightsReady(info.Dir, "retrieve: qwen reranker"); err != nil {
		return nil, err
	}
	if err := embed.TokenizerReady(info.Dir, "retrieve: qwen reranker"); err != nil {
		return nil, err
	}
	return qwenReranker{dir: info.Dir, name: info.Name, argv: append([]string(nil), argv...)}, nil
}

func (q qwenReranker) Score(ctx context.Context, query string, passages []string) ([]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(passages) == 0 {
		return []float64{}, nil
	}
	if len(passages) > embed.MaxForwardBatch {
		return nil, fmt.Errorf("retrieve: batch %d exceeds %d, refusing to keep that many scores in memory", len(passages), embed.MaxForwardBatch)
	}
	argv, err := q.command()
	if err != nil {
		return nil, err
	}
	pairs := make([]string, len(passages))
	for i, passage := range passages {
		pairs[i] = rerankPair(query, passage)
	}
	payload, err := json.Marshal(struct {
		Task     string   `json:"task"`
		Model    string   `json:"model"`
		ModelDir string   `json:"model_dir"`
		Query    string   `json:"query"`
		Passages []string `json:"passages"`
		Pairs    []string `json:"pairs"`
	}{
		Task:     "rerank",
		Model:    q.name,
		ModelDir: q.dir,
		Query:    query,
		Passages: passages,
		Pairs:    pairs,
	})
	if err != nil {
		return nil, err
	}
	out, err := finishRerank(ctx, q.name, argv, payload)
	if err != nil {
		return nil, err
	}
	return parseRerankScores(out, len(passages), "qwen")
}

func (q qwenReranker) command() ([]string, error) {
	if len(q.argv) > 0 {
		return q.argv, nil
	}
	return embed.PythonCommand("qwen_rerank.py", qwenRerankScript)
}

func rerankPair(query, passage string) string {
	return "<Instruct>: " + rerankTask + "\n<Query>:" + rerankQuestion(query) + "\n<Document>:" + passage
}
