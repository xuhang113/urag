package embed_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func waitForward(errCh <-chan error) {
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
	}
}
