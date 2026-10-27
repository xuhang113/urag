package embed

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// QueryInstruct 是查询侧嵌入前缀。Query: 后面没有空格。
// 文档侧 Embed 不添加这段文字。
const QueryInstruct = "Instruct: Given a web search query, retrieve relevant passages that answer the query\nQuery:"

const qwenDim = 2560

// QueryText 把查询前缀和问题直接拼在一起。
func QueryText(question string) string {
	return QueryInstruct + question
}

// ModelMeta 是嵌入模型或磁盘索引记下的模型。
// 本地 Qwen3 嵌入的 Name 是 Qwen/Qwen3-Embedding-4B，Dim 是 2560。
type ModelMeta struct {
	Name string
	Dim  int
}

// NewQwen 只从 dir 读取本地 Qwen 嵌入权重。
// 目录不存在，或 meta.Dim 不是 2560，返回错误。
// 不访问网络，不下载缺失权重，也不把隐藏层改成别的维度。
// 文档侧的 Embed 按调用方传入的文本编码，不追加 QueryInstruct。
// 查询由调用方先用 QueryText 拼好再交给 Embed。
func NewQwen(dir string, meta ModelMeta) (Embedder, error) {
	if meta.Dim != qwenDim {
		return nil, fmt.Errorf("embed: qwen dimension %d, want %d", meta.Dim, qwenDim)
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("embed: qwen weights directory %q does not exist", dir)
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("embed: qwen weights path %q is not a directory", dir)
	}
	hidden, err := readHiddenSize(dir)
	if err != nil {
		return nil, err
	}
	if hidden != qwenDim {
		return nil, fmt.Errorf("embed: qwen hidden size %d, want %d, refusing to change dimension", hidden, qwenDim)
	}
	weight, err := findWeightFile(dir)
	if err != nil {
		return nil, err
	}
	// 没有进程内前向实现时 load 返回错误，模型闸不会把嵌入模型留下。
	// 因此重排模型不会和它同时驻留，也不会去下载权重。
	return nil, GuardLocalModel(localEmbedKind, func() error {
		return fmt.Errorf("embed: qwen weights %q have no in-process runtime, refusing to download", weight)
	})
}

func readHiddenSize(dir string) (int, error) {
	f, err := os.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("embed: qwen config missing in %q, refusing to download", dir)
		}
		return 0, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return 0, err
	}
	var cfg struct {
		HiddenSize int `json:"hidden_size"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return 0, fmt.Errorf("embed: qwen config: %w", err)
	}
	if cfg.HiddenSize == 0 {
		return 0, fmt.Errorf("embed: qwen config in %q has no hidden size, refusing to download", dir)
	}
	return cfg.HiddenSize, nil
}

func findWeightFile(dir string) (string, error) {
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
	return "", fmt.Errorf("embed: qwen weight file missing in %q, refusing to download", dir)
}
