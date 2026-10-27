package embed_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuhang113/urag/internal/embed"
)

func TestRunLocalProcessStopsWhenContextEnds(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	script := filepath.Join(dir, "hold.sh")
	body := "#!/bin/sh\necho $$ > \"$1\"\nexec /bin/sleep 60\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := embed.RunLocalProcess(ctx, embed.KindEmbed, []string{script, pidFile}, nil)
		errCh <- err
	}()
	var raw []byte
	var readErr error
	deadline := time.Now().Add(3 * time.Second)
	for {
		raw, readErr = os.ReadFile(pidFile)
		if readErr == nil && strings.TrimSpace(string(raw)) != "" {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			waitForward(errCh)
			t.Fatalf("pid file missing: %v", readErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		cancel()
		waitForward(errCh)
		t.Fatal(convErr)
	}
	if !processAlive(pid) {
		cancel()
		waitForward(errCh)
		t.Fatalf("process %d exited before cancel", pid)
	}
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forward hung after cancel")
	}
	deadline = time.Now().Add(2 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunLocalProcessReadsStepFD(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "steps.sh")
	body := "#!/bin/sh\nprintf '%s\\n' load_embed_done query_vector_done >&\"$URAG_STEP_FD\"\nprintf '%s\\n' '{\"ok\":true}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	ctx := embed.WithStepHandler(context.Background(), func(token string) {
		mu.Lock()
		got = append(got, token)
		mu.Unlock()
	})
	out, err := embed.RunLocalProcess(ctx, embed.KindEmbed, []string{"/bin/sh", script}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"ok":true`) {
		t.Fatalf("stdout %s", out)
	}
	mu.Lock()
	embedSteps := append([]string(nil), got...)
	mu.Unlock()
	want := []string{"load_embed_done", "query_vector_done", "unload_embed", "unload_embed_done"}
	if strings.Join(embedSteps, ",") != strings.Join(want, ",") {
		t.Fatalf("embed steps %v", embedSteps)
	}

	mu.Lock()
	got = nil
	mu.Unlock()
	out, err = embed.RunLocalProcess(ctx, embed.KindRerank, []string{"/bin/sh", script}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"ok":true`) {
		t.Fatalf("rerank stdout %s", out)
	}
	mu.Lock()
	rerankSteps := append([]string(nil), got...)
	mu.Unlock()
	if strings.Join(rerankSteps, ",") != "load_embed_done,query_vector_done" {
		t.Fatalf("rerank steps %v", rerankSteps)
	}
}

func waitForward(errCh <-chan error) {
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
	}
}
