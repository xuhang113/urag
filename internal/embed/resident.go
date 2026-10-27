package embed

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// residentProc 是一次收录里常驻的前向进程。
// 调用方按批写一行 JSON，读回一行向量。进程自己不保存整库文本。
type residentProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *tailBuf

	call    sync.Mutex
	once    sync.Once
	waitErr error
}

// Start 启动嵌入进程并占住模型闸，直到 Close。
// 这期间 Embed 把批次写到该进程的 stdin，从 stdout 读 2560 维向量。
// 进程一直留到 Close。不在这里加载重排模型。
// 重复 Start 在进程还活着时直接返回。Close 之后不能再 Start。
func (q *qwenEmbedder) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return errors.New("embed: forward process is closed")
	}
	if q.opened {
		q.mu.Unlock()
		return nil
	}
	if q.starting {
		q.mu.Unlock()
		return errors.New("embed: forward process is starting")
	}
	q.starting = true
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		q.starting = false
		q.mu.Unlock()
	}()

	if err := acquireLocal(ctx, KindEmbed); err != nil {
		return err
	}
	argv, err := q.command()
	if err != nil {
		releaseLocal()
		return err
	}
	proc, err := startResident(ctx, argv)
	if err != nil {
		releaseLocal()
		return err
	}
	if err := ctx.Err(); err != nil {
		proc.shutdown()
		releaseLocal()
		return err
	}
	q.mu.Lock()
	q.proc = proc
	q.opened = true
	q.held = true
	q.mu.Unlock()
	return nil
}

// Close 关掉 Start 打开的嵌入进程并放开模型闸。
// 收录结束、失败或取消时调用。重复调用没有效果。
func (q *qwenEmbedder) Close() error {
	q.mu.Lock()
	proc := q.proc
	held := q.held
	opened := q.opened
	q.proc = nil
	q.held = false
	q.opened = false
	if opened || proc != nil {
		q.stopped = true
	}
	q.mu.Unlock()
	if proc != nil {
		proc.shutdown()
	}
	if held {
		releaseLocal()
	}
	return nil
}

func startResident(ctx context.Context, argv []string) (*residentProc, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("embed: empty forward command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = forwardEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr := &tailBuf{max: 8192}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("embed: qwen forward: %w", err)
	}
	return &residentProc{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		stderr: stderr,
	}, nil
}

func (p *residentProc) roundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	p.call.Lock()
	defer p.call.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := p.stdin.Write(payload); err != nil {
		return nil, p.ioErr(ctx, err)
	}
	if _, err := p.stdin.Write([]byte{'\n'}); err != nil {
		return nil, p.ioErr(ctx, err)
	}
	stop := context.AfterFunc(ctx, func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
	defer stop()
	line, err := readLineLimited(p.stdout, maxForwardBytes)
	if err != nil {
		return nil, p.ioErr(ctx, err)
	}
	return line, nil
}

func (p *residentProc) ioErr(ctx context.Context, cause error) error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	if ctx.Err() != nil {
		_ = p.reap()
		return ctx.Err()
	}
	exitErr := p.reap()
	if tooBig(cause) {
		return cause
	}
	if exitErr != nil {
		return forwardErr(exitErr, p.stderr.String())
	}
	if cause == nil {
		return errors.New("embed: qwen forward: process exited")
	}
	return fmt.Errorf("embed: qwen forward: %w", cause)
}

func (p *residentProc) reap() error {
	p.once.Do(func() {
		p.waitErr = p.cmd.Wait()
	})
	return p.waitErr
}

func (p *residentProc) shutdown() {
	_ = p.stdin.Close()
	timer := time.AfterFunc(3*time.Second, func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	})
	defer timer.Stop()
	_ = p.reap()
}

func readLineLimited(r *bufio.Reader, limit int) ([]byte, error) {
	var buf bytes.Buffer
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 {
			if buf.Len()+len(chunk) > limit {
				return nil, fmt.Errorf("embed: forward output exceeds %d bytes", limit)
			}
			_, _ = buf.Write(chunk)
		}
		if err == nil {
			return buf.Bytes(), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && buf.Len() > 0 {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
}

func tooBig(err error) bool {
	return err != nil && bytes.Contains([]byte(err.Error()), []byte("exceeds"))
}
