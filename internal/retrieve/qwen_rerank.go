package retrieve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuhang113/urag/internal/embed"
)

// PassageReranker 给一批子块打分。
// 分数与 passages 等长、顺序相同。只产出分数，不生成答案，也不加载嵌入模型。
type PassageReranker interface {
	Score(ctx context.Context, query string, passages []string) ([]float64, error)
}

// NewQwenReranker 只从 dir 读取本地重排权重。
// 目录不存在时返回错误。不访问网络，不下载，也不加载嵌入模型。
func NewQwenReranker(dir string) (PassageReranker, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("retrieve: qwen reranker directory %q does not exist", dir)
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("retrieve: qwen reranker path %q is not a directory", dir)
	}
	weight, err := findRerankWeight(dir)
	if err != nil {
		return nil, err
	}
	err = embed.GuardLocalModel("rerank", func() error {
		return fmt.Errorf("retrieve: qwen reranker %q has no in-process runtime, refusing to download", weight)
	})
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("retrieve: qwen reranker %q was not loaded", weight)
}

func findRerankWeight(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("retrieve: qwen reranker config missing in %q, refusing to download", dir)
		}
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.ToLower(entry.Name())
		if strings.HasSuffix(name, ".safetensors") || strings.HasSuffix(name, ".gguf") || name == "pytorch_model.bin" {
			return filepath.Join(dir, entry.Name()), nil
		}
	}
	return "", fmt.Errorf("retrieve: qwen reranker weight file missing in %q, refusing to download", dir)
}
