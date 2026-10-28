package retrieve_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/retrieve"
)

func TestCrossEncoderScoresRawQuestion(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "query.txt")
	script := writeText(t, filepath.Join(dir, "fake.py"), fakeCrossScript)
	rr, err := retrieve.NewCrossEncoderRerankerCommand(writeCrossLayout(t, "BAAI/bge-reranker-base"), []string{pythonBin(t), script, logPath})
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	ctx := retrieve.WithProgress(context.Background(), func(name string, done bool) {
		if done {
			steps = append(steps, name)
		}
	})
	scores, err := rr.Score(ctx, embed.QueryText("crimson"), []string{"east"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 1 || scores[0] != 0.8 {
		t.Fatalf("scores %v", scores)
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if got != "crimson\nBAAI/bge-reranker-base\n" {
		t.Fatalf("payload %q", got)
	}
	if !strings.Contains(strings.Join(steps, "\n"), retrieve.RerankModelStep("BAAI/bge-reranker-base")) {
		t.Fatalf("steps %v", steps)
	}
}

func TestCrossEncoderRejectsQwenLayout(t *testing.T) {
	_, err := retrieve.NewCrossEncoderReranker(writeRerankLayout(t, 2560))
	if err == nil || !strings.Contains(err.Error(), "cross-encoder") {
		t.Fatal(err)
	}
}

func TestQwenRerankerReportsModelStep(t *testing.T) {
	dir := t.TempDir()
	script := writeText(t, filepath.Join(dir, "fake.py"), fakeRerankScript)
	rr, err := retrieve.NewQwenRerankerCommand(writeRerankLayout(t, 2560), []string{pythonBin(t), script, filepath.Join(dir, "pairs.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	ctx := retrieve.WithProgress(context.Background(), func(name string, done bool) {
		if done {
			steps = append(steps, name)
		}
	})
	if _, err := rr.Score(ctx, embed.QueryText("crimson"), []string{"east"}); err != nil {
		t.Fatal(err)
	}
	want := retrieve.RerankModelStep("Qwen/Qwen3-Reranker-4B")
	if !strings.Contains(strings.Join(steps, "\n"), want) {
		t.Fatalf("steps %v", steps)
	}
}

func TestCrossRerankScriptIsLocal(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	script := filepath.Join(filepath.Dir(file), "cross_rerank.py")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, frag := range []string{
		"local_files_only=True",
		"AutoModelForSequenceClassification",
		"sigmoid",
		"load_rerank_done",
		"rerank_done",
		"rerank_model ",
		"rerank_model_done ",
		"URAG_STEP_FD",
		"max_length=512",
	} {
		if !strings.Contains(text, frag) {
			t.Fatalf("script missing %s", frag)
		}
	}
	for _, frag := range []string{"snapshot_download", "huggingface.co", ".generate(", `to("cpu")`, "to('cpu')"} {
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

func writeCrossLayout(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	writeText(t, filepath.Join(dir, "config.json"), fmt.Sprintf("{\"architectures\":[\"XLMRobertaForSequenceClassification\"],\"model_type\":\"xlm-roberta\",\"hidden_size\":768,\"num_labels\":1,\"_name_or_path\":%q}\n", name))
	writeText(t, filepath.Join(dir, "model.safetensors"), "fake")
	writeText(t, filepath.Join(dir, "tokenizer.json"), "{}")
	return dir
}

const fakeCrossScript = `
import json, sys
req = json.load(sys.stdin)
with open(sys.argv[1], "w", encoding="utf-8") as handle:
    handle.write(req["query"] + "\n" + req["model"] + "\n")
json.dump({"scores": [0.8] * len(req["passages"])}, sys.stdout)
`
