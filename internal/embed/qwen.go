package embed

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// QueryInstruct 是查询侧嵌入前缀。Query: 后面没有空格。
// 文档侧 Embed 不添加这段文字。
// 拼法与本机嵌入模型卡一致：Instruct: {任务}\nQuery:{问题}。
const QueryInstruct = "Instruct: Given a web search query, retrieve relevant passages that answer the query\nQuery:"

// QwenEmbeddingName 是本地嵌入模型。维度固定 2560，不能改成 0.6B。
const QwenEmbeddingName = "Qwen/Qwen3-Embedding-4B"

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

//go:embed qwen_forward.py
var qwenForwardScript string

type qwenEmbedder struct {
	dir  string
	argv []string

	mu       sync.Mutex
	proc     *residentProc
	opened   bool
	starting bool
	stopped  bool
	held     bool
	loaded   bool
}

// NewQwen 只从 dir 读取本地 Qwen 嵌入权重。
// dir 可以是快照目录，也可以是 Hugging Face 缓存根目录；后者只用 refs/main 指向的快照。
// 目录不存在、模型名不是 Qwen/Qwen3-Embedding-4B、meta.Dim 或 config 的 hidden_size 不是 2560、
// 权重或分词器缺失时返回错误。不访问网络，不下载，也不改维度。
// 文档侧的 Embed 按调用方传入的文本编码，不追加 QueryInstruct。
// 查询由调用方先用 QueryText 拼好再交给 Embed。
// 没有 Start 时，Embed 每一批启动一次短生命周期的本机 Python。
// Start 之后，同一次收录里的 Embed 复用这一个进程，直到 Close。
func NewQwen(dir string, meta ModelMeta) (Embedder, error) {
	return NewQwenCommand(dir, meta, nil)
}

// NewQwenCommand 与 NewQwen 做同样的权重检查。
// argv 为空时用本机 python3 和内置前向脚本。测试传入假命令，不加载真实 4B。
func NewQwenCommand(dir string, meta ModelMeta, argv []string) (Embedder, error) {
	if meta.Name != QwenEmbeddingName {
		return nil, fmt.Errorf("embed: qwen model %q, want %s", meta.Name, QwenEmbeddingName)
	}
	if meta.Dim != qwenDim {
		return nil, fmt.Errorf("embed: qwen dimension %d, want %d", meta.Dim, qwenDim)
	}
	if argv != nil && len(argv) == 0 {
		return nil, fmt.Errorf("embed: empty forward command")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("embed: qwen weights directory %q does not exist", dir)
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("embed: qwen weights path %q is not a directory", dir)
	}
	snap, err := ResolveSnapshot(abs, "embed: qwen")
	if err != nil {
		return nil, err
	}
	hidden, err := readHiddenSize(snap)
	if err != nil {
		return nil, err
	}
	if hidden != qwenDim {
		return nil, fmt.Errorf("embed: qwen hidden size %d, want %d, refusing to change dimension", hidden, qwenDim)
	}
	if err := WeightsReady(snap, "embed: qwen"); err != nil {
		return nil, err
	}
	if err := TokenizerReady(snap, "embed: qwen"); err != nil {
		return nil, err
	}
	return &qwenEmbedder{dir: snap, argv: append([]string(nil), argv...)}, nil
}

func (q *qwenEmbedder) Dim() int { return qwenDim }

func (q *qwenEmbedder) Loaded() bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.loaded
}

func (q *qwenEmbedder) Holding() bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.held
}

func (q *qwenEmbedder) ProcessID() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.proc == nil || q.proc.cmd == nil || q.proc.cmd.Process == nil {
		return 0
	}
	return q.proc.cmd.Process.Pid
}

func (q *qwenEmbedder) Embed(ctx context.Context, texts []string) ([]Vector, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return []Vector{}, nil
	}
	if len(texts) > MaxForwardBatch {
		return nil, fmt.Errorf("embed: batch %d exceeds %d, refusing to keep that many vectors in memory", len(texts), MaxForwardBatch)
	}
	q.mu.Lock()
	stopped := q.stopped
	opened := q.opened
	proc := q.proc
	q.mu.Unlock()
	if stopped {
		return nil, errors.New("embed: forward process is closed")
	}
	payload, err := q.payload(texts)
	if err != nil {
		return nil, err
	}
	if opened {
		return q.embedResident(ctx, proc, payload, len(texts))
	}
	return q.embedOnce(ctx, payload, len(texts))
}

func (q *qwenEmbedder) embedOnce(ctx context.Context, payload []byte, n int) ([]Vector, error) {
	argv, err := q.command()
	if err != nil {
		return nil, err
	}
	out, err := RunLocalProcess(ctx, KindEmbed, argv, payload)
	if err != nil {
		return nil, err
	}
	return parseVectors(out, n)
}

func (q *qwenEmbedder) embedResident(ctx context.Context, proc *residentProc, payload []byte, n int) ([]Vector, error) {
	if proc == nil {
		return nil, errors.New("embed: forward process is closed")
	}
	out, err := proc.roundTrip(ctx, payload)
	if err != nil {
		return nil, err
	}
	vecs, err := parseVectors(out, n)
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	q.loaded = true
	q.mu.Unlock()
	return vecs, nil
}

func (q *qwenEmbedder) payload(texts []string) ([]byte, error) {
	return json.Marshal(struct {
		Task     string   `json:"task"`
		ModelDir string   `json:"model_dir"`
		Dim      int      `json:"dim"`
		Texts    []string `json:"texts"`
	}{
		Task:     "embed",
		ModelDir: q.dir,
		Dim:      qwenDim,
		Texts:    texts,
	})
}

func parseVectors(out []byte, n int) ([]Vector, error) {
	var parsed struct {
		Vectors [][]float32 `json:"vectors"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("embed: qwen forward output: %w", err)
	}
	if len(parsed.Vectors) != n {
		return nil, fmt.Errorf("embed: qwen forward returned %d vectors for %d texts", len(parsed.Vectors), n)
	}
	vecs := make([]Vector, len(parsed.Vectors))
	for i, row := range parsed.Vectors {
		if len(row) != qwenDim {
			return nil, fmt.Errorf("embed: qwen vector %d has length %d, want %d, refusing to change dimension", i, len(row), qwenDim)
		}
		vecs[i] = row
	}
	return vecs, nil
}

func (q *qwenEmbedder) command() ([]string, error) {
	if len(q.argv) > 0 {
		return q.argv, nil
	}
	return PythonCommand("qwen_forward.py", qwenForwardScript)
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
