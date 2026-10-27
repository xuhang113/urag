package retrieve_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/retrieve"
)

func TestNewQwenRerankerRejectsHiddenSize(t *testing.T) {
	dir := writeRerankLayout(t, 1024)
	marker := filepath.Join(t.TempDir(), "ran")
	_, err := retrieve.NewQwenRerankerCommand(dir, []string{pythonBin(t), writeTouchScript(t, marker)})
	if err == nil || !strings.Contains(err.Error(), "1024") {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("command ran for hidden size 1024")
	}
}

func TestNewQwenRerankerRejectsMissingWeight(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"hidden_size":2560}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := retrieve.NewQwenReranker(dir)
	if err == nil {
		t.Fatal("missing weight file should fail")
	}
}

func TestQwenRerankerCommandScoresBatch(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "pairs.jsonl")
	script := writeText(t, filepath.Join(dir, "fake.py"), fakeRerankScript)
	rr, err := retrieve.NewQwenRerankerCommand(writeRerankLayout(t, 2560), []string{pythonBin(t), script, logPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	empty, err := rr.Score(ctx, embed.QueryText("crimson"), nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty score: %v %v", empty, err)
	}
	if _, statErr := os.Stat(logPath); !os.IsNotExist(statErr) {
		t.Fatal("empty score started a process")
	}

	query := embed.QueryText("crimson lantern")
	scores, err := rr.Score(ctx, query, []string{"east", "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 2 || scores[0] != 4 || scores[1] != 6 {
		t.Fatalf("scores %v", scores)
	}
	raw, err := rr.Score(ctx, "lantern", []string{"stone"})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 || raw[0] != 5 {
		t.Fatalf("raw scores %v", raw)
	}

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("batches %q", body)
	}
	var first []string
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	want0 := "<Instruct>: Given a web search query, retrieve relevant passages that answer the query\n<Query>:crimson lantern\n<Document>:east"
	want1 := "<Instruct>: Given a web search query, retrieve relevant passages that answer the query\n<Query>:crimson lantern\n<Document>:bridge"
	if len(first) != 2 || first[0] != want0 || first[1] != want1 {
		t.Fatalf("pairs %q", first)
	}
	if strings.Contains(first[0], "<Query>: ") || strings.Contains(first[0], "Query: ") {
		t.Fatal("space inserted after Query:")
	}
	var second []string
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0] != "<Instruct>: Given a web search query, retrieve relevant passages that answer the query\n<Query>:lantern\n<Document>:stone" {
		t.Fatalf("raw pair %q", second)
	}
}

func TestEmbedProcessExitsBeforeRerank(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "order.log")
	hold := filepath.Join(dir, "hold")
	if err := os.WriteFile(hold, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	py := pythonBin(t)
	emb, err := embed.NewQwenCommand(writeRerankLayout(t, 2560), embed.ModelMeta{
		Name: embed.QwenEmbeddingName,
		Dim:  2560,
	}, []string{py, writeText(t, filepath.Join(dir, "embed.py"), fakeHoldEmbedScript), logPath, hold})
	if err != nil {
		t.Fatal(err)
	}
	rr, err := retrieve.NewQwenRerankerCommand(writeRerankLayout(t, 2560), []string{
		py, writeText(t, filepath.Join(dir, "rerank.py"), fakeHoldRerankScript), logPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	blocked := make(chan struct{})
	var once sync.Once
	embed.SetLocalWaitNotifier(func(waiting, held string) {
		if waiting == embed.KindRerank && held == embed.KindEmbed {
			once.Do(func() { close(blocked) })
		}
	})
	t.Cleanup(func() {
		cancel()
		embed.SetLocalWaitNotifier(nil)
	})

	errCh := make(chan error, 2)
	go func() {
		_, err := emb.Embed(ctx, []string{"alpha"})
		errCh <- err
	}()
	if !waitLog(t, logPath, "embed-start", 5*time.Second) {
		t.Fatal("embed process did not start")
	}
	go func() {
		_, err := rr.Score(ctx, embed.QueryText("question"), []string{"passage"})
		errCh <- err
	}()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("rerank was not blocked on the embed process")
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "rerank-start") {
		t.Fatalf("rerank started while embed was resident\n%s", body)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("forward did not finish")
		}
	}
	body, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(body))
	want := "embed-start\nembed-exit\nrerank-start\nrerank-exit"
	if got != want {
		t.Fatalf("order\n%s", got)
	}
}

func TestQwenRerankScriptIsLocal(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	script := filepath.Join(filepath.Dir(file), "qwen_rerank.py")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, frag := range []string{
		"Instruct: Given a web search query, retrieve relevant passages that answer the query",
		"local_files_only=True",
		"mps",
		"2560",
	} {
		if !strings.Contains(text, frag) {
			t.Fatalf("script missing %s", frag)
		}
	}
	for _, frag := range []string{"snapshot_download", "huggingface.co", ".generate(", `to("cpu")`, "to('cpu')", "NewQwen"} {
		if strings.Contains(text, frag) {
			t.Fatalf("script contains %s", frag)
		}
	}
	cmd := exec.Command(pythonBin(t), "-m", "py_compile", script)
	cmd.Env = append(os.Environ(), "PYTHONPYCACHEPREFIX="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("py_compile: %v\n%s", err, out)
	}
}

func writeRerankLayout(t *testing.T, hidden int) string {
	t.Helper()
	dir := t.TempDir()
	writeText(t, filepath.Join(dir, "config.json"), fmt.Sprintf("{\"hidden_size\":%d}\n", hidden))
	writeText(t, filepath.Join(dir, "model.safetensors"), "fake")
	return dir
}

func writeText(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTouchScript(t *testing.T, marker string) string {
	t.Helper()
	return writeText(t, filepath.Join(t.TempDir(), "touch.py"), fmt.Sprintf("import pathlib\npathlib.Path(%q).touch()\n", marker))
}

func pythonBin(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func waitLog(t *testing.T, path, needle string, limit time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		body, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(body), needle) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const fakeRerankScript = `
import json, sys
req = json.load(sys.stdin)
with open(sys.argv[1], "a", encoding="utf-8") as handle:
    handle.write(json.dumps(req["pairs"], ensure_ascii=False) + "\n")
scores = [float(len(passage)) for passage in req["passages"]]
json.dump({"scores": scores}, sys.stdout)
`

const fakeHoldEmbedScript = `
import json, os, sys, time
req = json.load(sys.stdin)
log, hold = sys.argv[1], sys.argv[2]
with open(log, "a", encoding="utf-8") as handle:
    handle.write("embed-start\n")
while os.path.exists(hold):
    time.sleep(0.02)
with open(log, "a", encoding="utf-8") as handle:
    handle.write("embed-exit\n")
dim = int(req["dim"])
row = [0.0] * dim
row[0] = 1.0
json.dump({"vectors": [row]}, sys.stdout)
`

const fakeHoldRerankScript = `
import json, sys
req = json.load(sys.stdin)
log = sys.argv[1]
with open(log, "a", encoding="utf-8") as handle:
    handle.write("rerank-start\n")
json.dump({"scores": [0.25] * len(req["passages"])}, sys.stdout)
with open(log, "a", encoding="utf-8") as handle:
    handle.write("rerank-exit\n")
`
