package retrieve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuhang113/urag/internal/embed"
)

const (
	// KindQwen4B 是 Qwen3-Reranker-4B。hidden_size 必须是 2560。
	KindQwen4B = "qwen4b"
	// KindCrossEncoder 是本地 sequence-classification 重排，例如 bge-reranker-base。
	KindCrossEncoder = "cross-encoder"
)

// LocalReranker 是本机重排目录的种类和显示名。
// 只读 config.json，不加载权重，也不下载。
type LocalReranker struct {
	Dir    string
	Kind   string
	Name   string
	Hidden int
}

type rerankConfig struct {
	NameOrPath    string   `json:"_name_or_path"`
	ModelType     string   `json:"model_type"`
	Architectures []string `json:"architectures"`
	HiddenSize    int      `json:"hidden_size"`
}

// InspectReranker 判断目录是 4B 因果重排还是小 cross-encoder。
// 目录不存在、快照断掉或配置对不上时返回错误，不下载。
func InspectReranker(dir string) (LocalReranker, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return LocalReranker{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return LocalReranker{}, fmt.Errorf("retrieve: qwen reranker directory %q does not exist", dir)
		}
		return LocalReranker{}, err
	}
	if !info.IsDir() {
		return LocalReranker{}, fmt.Errorf("retrieve: qwen reranker path %q is not a directory", dir)
	}
	snap, err := embed.ResolveSnapshot(abs, "retrieve: qwen reranker")
	if err != nil {
		return LocalReranker{}, err
	}
	cfg, err := readRerankConfig(snap)
	if err != nil {
		return LocalReranker{}, err
	}
	kind, name, err := classifyRerank(snap, cfg)
	if err != nil {
		return LocalReranker{}, err
	}
	return LocalReranker{Dir: snap, Kind: kind, Name: name, Hidden: cfg.HiddenSize}, nil
}

func classifyRerank(dir string, cfg rerankConfig) (string, string, error) {
	arch := strings.Join(cfg.Architectures, " ")
	if strings.Contains(arch, "ForSequenceClassification") {
		return KindCrossEncoder, cleanModelName(cfg.NameOrPath, "cross-encoder"), nil
	}
	// 测试夹具只写 hidden_size。真实 4B 目录是 Qwen3ForCausalLM，hidden_size 为 2560。
	if cfg.HiddenSize == 2560 && (arch == "" || strings.Contains(arch, "ForCausalLM") || cfg.ModelType == "qwen2" || cfg.ModelType == "qwen3") {
		return KindQwen4B, cleanModelName(cfg.NameOrPath, QwenRerankerName), nil
	}
	if cfg.HiddenSize == 0 {
		return "", "", fmt.Errorf("retrieve: qwen reranker config in %q has no hidden size, refusing to download", dir)
	}
	return "", "", fmt.Errorf("retrieve: qwen reranker hidden size %d, want 2560, refusing to change dimension", cfg.HiddenSize)
}

func cleanModelName(raw, fallback string) string {
	raw = strings.Join(strings.Fields(raw), " ")
	if raw == "" {
		return fallback
	}
	if len(raw) > 160 {
		raw = raw[:160]
	}
	return raw
}

// RerankModelStep 是页面日志里的重排模型行。名字里带模型名，起止时间由进度回调补上。
func RerankModelStep(name string) string {
	return "重排模型 " + cleanModelName(name, QwenRerankerName)
}

func readRerankConfig(dir string) (rerankConfig, error) {
	f, err := os.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return rerankConfig{}, fmt.Errorf("retrieve: qwen reranker config missing in %q, refusing to download", dir)
		}
		return rerankConfig{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return rerankConfig{}, err
	}
	var cfg rerankConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return rerankConfig{}, fmt.Errorf("retrieve: qwen reranker config: %w", err)
	}
	return cfg, nil
}

// finishRerank 跑一次重排子进程，并在标准错误和页面日志里写下模型名和耗时。
// 子进程失败时不把这一步标成完成。
func finishRerank(ctx context.Context, name string, argv []string, payload []byte) ([]byte, error) {
	label := RerankModelStep(name)
	Report(ctx, label, false)
	started := time.Now()
	out, err := embed.RunLocalProcess(ctx, embed.KindRerank, argv, payload)
	fmt.Fprintf(os.Stderr, "rerank model=%s elapsed_s=%.3f\n", cleanModelName(name, QwenRerankerName), time.Since(started).Seconds())
	if err != nil {
		return nil, err
	}
	Report(ctx, label, true)
	return out, nil
}
