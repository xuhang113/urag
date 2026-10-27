package embed

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxForwardBytes = 64 << 20

// RunLocalProcess 启动一次短生命周期前向。
// 子进程读完 stdin 上的这一批，把结果写到 stdout，然后退出。
// kind 为 KindEmbed 或 KindRerank。另一种模型还在跑时，要等它退出才能启动。
func RunLocalProcess(ctx context.Context, kind string, argv []string, stdin []byte) ([]byte, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("embed: empty forward command")
	}
	var out []byte
	err := WithLocalModel(ctx, kind, func() error {
		got, runErr := execForward(ctx, argv, stdin)
		out = got
		return runErr
	})
	if err != nil {
		return nil, err
	}
	// 进程已经退出，模型不再占内存。
	// 记号在读完阶段管道之后才发，所以会排在这一批前向完成之后。
	switch kind {
	case KindEmbed:
		EmitStep(ctx, "unload_embed")
		EmitStep(ctx, "unload_embed_done")
	case KindRerank:
		EmitStep(ctx, "unload_rerank")
		EmitStep(ctx, "unload_rerank_done")
	}
	return out, nil
}

// PythonCommand 用本机 python3 执行脚本。脚本写在临时目录，不写对象存储。
// 找不到 python3 时返回错误，不下载运行时。
func PythonCommand(scriptName, body string) ([]string, error) {
	if scriptName == "" || strings.Contains(scriptName, "/") || strings.Contains(scriptName, "..") {
		return nil, fmt.Errorf("embed: invalid script name %q", scriptName)
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		return nil, fmt.Errorf("embed: python3 not found, refusing to download a runtime")
	}
	dir := filepath.Join(os.TempDir(), "urag-qwen")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, scriptName)
	current, readErr := os.ReadFile(path)
	if readErr != nil || string(current) != body {
		tmp, err := os.CreateTemp(dir, scriptName+".")
		if err != nil {
			return nil, err
		}
		tmpName := tmp.Name()
		_, werr := tmp.WriteString(body)
		cerr := tmp.Close()
		if werr != nil || cerr != nil {
			_ = os.Remove(tmpName)
			if werr != nil {
				return nil, werr
			}
			return nil, cerr
		}
		if err := os.Chmod(tmpName, 0o644); err != nil {
			_ = os.Remove(tmpName)
			return nil, err
		}
		if err := os.Rename(tmpName, path); err != nil {
			_ = os.Remove(tmpName)
			return nil, err
		}
	}
	return []string{py, path}, nil
}

func execForward(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// 上下文结束后，子进程或它留下的管道最多少等这段时间。
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = forwardEnv()
	// 阶段记号走额外描述符，不进 stdout，也不混进错误信息。
	// 父进程在启动后关掉写端，子进程退出时读端才会结束。
	stepR, stepW, pipeErr := os.Pipe()
	var readerDone chan struct{}
	if pipeErr == nil {
		cmd.ExtraFiles = []*os.File{stepW}
		cmd.Env = append(cmd.Env, "URAG_STEP_FD=3")
		readerDone = make(chan struct{})
		go func() {
			defer close(readerDone)
			readStepFD(ctx, stepR)
			_ = stepR.Close()
		}()
	}
	defer func() {
		if stepW != nil {
			_ = stepW.Close()
		}
		if readerDone != nil {
			<-readerDone
		}
	}()
	var stderr tailBuf
	stderr.max = 8192
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("embed: qwen forward: %w", err)
	}
	if stepW != nil {
		_ = stepW.Close()
		stepW = nil
	}
	type readResult struct {
		out []byte
		err error
	}
	done := make(chan readResult, 1)
	go func() {
		out, readErr := readLimited(stdout, maxForwardBytes)
		done <- readResult{out: out, err: readErr}
	}()
	select {
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		// 关掉读端，避免子进程退出后仍有人占着写端，读取一直不返回。
		_ = stdout.Close()
		<-done
		_ = cmd.Wait()
		return nil, ctx.Err()
	case got := <-done:
		if got.err != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = io.Copy(io.Discard, stdout)
		}
		waitErr := cmd.Wait()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if got.err != nil {
			return nil, got.err
		}
		if waitErr != nil {
			return nil, forwardErr(waitErr, stderr.String())
		}
		return got.out, nil
	}
}

func readStepFD(ctx context.Context, r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		token := strings.TrimSpace(sc.Text())
		if token == "" {
			continue
		}
		EmitStep(ctx, token)
	}
}

func readLimited(r io.Reader, limit int) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 8192)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > limit {
				return nil, fmt.Errorf("embed: forward output exceeds %d bytes", limit)
			}
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func forwardErr(err error, stderr string) error {
	msg := strings.TrimSpace(stderr)
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	if msg == "" {
		return fmt.Errorf("embed: qwen forward: %w", err)
	}
	return fmt.Errorf("embed: qwen forward: %w: %s", err, msg)
}

type tailBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.buf.Len() < t.max {
		room := t.max - t.buf.Len()
		chunk := p
		if len(chunk) > room {
			chunk = chunk[:room]
		}
		_, _ = t.buf.Write(chunk)
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

func forwardEnv() []string {
	skip := map[string]struct{}{
		"URAG_MYSQL_DSN":               {},
		"HF_TOKEN":                     {},
		"HUGGING_FACE_HUB_TOKEN":       {},
		"HUGGINGFACE_HUB_TOKEN":        {},
		"HF_HUB_OFFLINE":               {},
		"TRANSFORMERS_OFFLINE":         {},
		"HF_DATASETS_OFFLINE":          {},
		"HF_HUB_DISABLE_TELEMETRY":     {},
		"HF_HUB_DISABLE_PROGRESS_BARS": {},
		"TOKENIZERS_PARALLELISM":       {},
		"PYTHONIOENCODING":             {},
	}
	out := make([]string, 0, len(os.Environ())+8)
	for _, kv := range os.Environ() {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, drop := skip[key]; drop {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"HF_HUB_OFFLINE=1",
		"TRANSFORMERS_OFFLINE=1",
		"HF_DATASETS_OFFLINE=1",
		"HF_HUB_DISABLE_TELEMETRY=1",
		"HF_HUB_DISABLE_PROGRESS_BARS=1",
		"TOKENIZERS_PARALLELISM=false",
		"PYTHONUNBUFFERED=1",
		"PYTHONIOENCODING=utf-8",
	)
	return out
}
