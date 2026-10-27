package embed_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xuhang113/urag/internal/embed"
)

func TestQueryText(t *testing.T) {
	const question = "crimson lantern"
	got := embed.QueryText(question)
	want := "Instruct: Given a web search query, retrieve relevant passages that answer the query\nQuery:" + question
	if got != want || got != embed.QueryInstruct+question {
		t.Fatalf("QueryText = %q", got)
	}
	if strings.Contains(embed.QueryInstruct, "Query: ") || !strings.HasSuffix(embed.QueryInstruct, "Query:") {
		t.Fatalf("prefix = %q", embed.QueryInstruct)
	}
	if got[len(embed.QueryInstruct)] != question[0] {
		t.Fatal("space inserted after Query:")
	}
}

func TestNewQwenMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "no-such-weights")
	_, err := embed.NewQwen(dir, embed.ModelMeta{
		Name: "Qwen/Qwen3-Embedding-4B",
		Dim:  2560,
	})
	if err == nil {
		t.Fatal("missing weights directory should fail")
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatal("missing directory was created")
	}
}

func TestNewQwenRejectsDimension(t *testing.T) {
	_, err := embed.NewQwen(t.TempDir(), embed.ModelMeta{
		Name: "Qwen/Qwen3-Embedding-4B",
		Dim:  768,
	})
	if err == nil {
		t.Fatal("dimension other than 2560 should fail")
	}
}

func TestNewQwenRejectsHiddenSize(t *testing.T) {
	dir := writeQwenLayout(t, 1024)
	marker := filepath.Join(t.TempDir(), "ran")
	_, err := embed.NewQwenCommand(dir, qwenMeta(), []string{pythonBin(t), writeFakeTouch(t, marker)})
	if err == nil || !strings.Contains(err.Error(), "1024") {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("command ran for hidden size 1024")
	}
}

func TestNewQwenRejectsMissingWeight(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"hidden_size":2560}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := embed.NewQwen(dir, qwenMeta())
	if err == nil {
		t.Fatal("missing weight file should fail")
	}
}

func TestNewQwenReadyDoesNotStartProcess(t *testing.T) {
	emb, err := embed.NewQwen(writeQwenLayout(t, 2560), qwenMeta())
	if err != nil {
		t.Fatal(err)
	}
	if emb.Dim() != 2560 {
		t.Fatalf("dim %d", emb.Dim())
	}
}

func TestQwenCommandReturnsBatchVectors(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "batches.jsonl")
	script := writeFile(t, filepath.Join(dir, "fake.py"), fakeEmbedScript)
	emb, err := embed.NewQwenCommand(writeQwenLayout(t, 2560), qwenMeta(), []string{pythonBin(t), script, logPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	empty, err := emb.Embed(ctx, nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty embed: %v %v", empty, err)
	}
	if _, statErr := os.Stat(logPath); !os.IsNotExist(statErr) {
		t.Fatal("empty embed started a process")
	}
	large := make([]string, embed.MaxForwardBatch+1)
	if _, err := emb.Embed(ctx, large); err == nil {
		t.Fatal("oversized batch should fail")
	}
	if _, statErr := os.Stat(logPath); !os.IsNotExist(statErr) {
		t.Fatal("oversized batch started a process")
	}

	first, err := emb.Embed(ctx, []string{"alpha", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	assertBatchVector(t, first[0], 2560, 5, 1)
	assertBatchVector(t, first[1], 2560, 4, 2)
	query := embed.QueryText("lantern")
	second, err := emb.Embed(ctx, []string{query})
	if err != nil {
		t.Fatal(err)
	}
	assertBatchVector(t, second[0], 2560, float32(len(query)), 1)

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("batches %q", body)
	}
	var gotFirst []string
	if err := json.Unmarshal([]byte(lines[0]), &gotFirst); err != nil {
		t.Fatal(err)
	}
	if len(gotFirst) != 2 || gotFirst[0] != "alpha" || gotFirst[1] != "beta" {
		t.Fatalf("first batch %v", gotFirst)
	}
	if strings.Contains(gotFirst[0], "Instruct:") {
		t.Fatal("document text was prefixed")
	}
	var gotSecond []string
	if err := json.Unmarshal([]byte(lines[1]), &gotSecond); err != nil {
		t.Fatal(err)
	}
	if len(gotSecond) != 1 || gotSecond[0] != query || strings.Count(gotSecond[0], "Instruct:") != 1 {
		t.Fatalf("query batch %q", gotSecond)
	}
}

func TestQwenForwardScriptIsLocal(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), "qwen_forward.py"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, frag := range []string{"local_files_only=True", "hidden_size", "mps", "2560"} {
		if !strings.Contains(text, frag) {
			t.Fatalf("script missing %s", frag)
		}
	}
	for _, frag := range []string{"snapshot_download", "huggingface.co", "Qwen3-Embedding-0.6B", `to("cpu")`, "to('cpu')", ".generate("} {
		if strings.Contains(text, frag) {
			t.Fatalf("script contains %s", frag)
		}
	}
	cmd := exec.Command(pythonBin(t), "-m", "py_compile", filepath.Join(filepath.Dir(file), "qwen_forward.py"))
	cmd.Env = append(os.Environ(), "PYTHONPYCACHEPREFIX="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("py_compile: %v\n%s", err, out)
	}
}

func TestLocalEmbedding06BIsNotSuccess(t *testing.T) {
	dir := embedding06BDir()
	if dir == "" {
		t.Skip("local Qwen3-Embedding-0.6B snapshot is absent")
	}
	_, err := embed.NewQwen(dir, qwenMeta())
	if err == nil || !strings.Contains(err.Error(), "1024") {
		t.Fatal(err)
	}
}

func qwenMeta() embed.ModelMeta {
	return embed.ModelMeta{Name: embed.QwenEmbeddingName, Dim: 2560}
}

func writeQwenLayout(t *testing.T, hidden int) string {
	t.Helper()
	dir := t.TempDir()
	writeQwenFiles(t, dir, hidden)
	return dir
}

func writeQwenFiles(t *testing.T, dir string, hidden int) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "config.json"), fmt.Sprintf("{\"hidden_size\":%d}\n", hidden))
	writeFile(t, filepath.Join(dir, "model.safetensors"), "fake")
	writeFile(t, filepath.Join(dir, "tokenizer.json"), "{}")
}

func TestNewQwenResolvesCacheSnapshot(t *testing.T) {
	root := t.TempDir()
	const hash = "abc123snapshot"
	snap := filepath.Join(root, "snapshots", hash)
	if err := os.MkdirAll(filepath.Join(root, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(root, "blobs", "weight")
	writeFile(t, blob, "fake")
	writeFile(t, filepath.Join(snap, "config.json"), "{\"hidden_size\":2560}\n")
	writeFile(t, filepath.Join(snap, "tokenizer.json"), "{}")
	if err := os.Symlink(blob, filepath.Join(snap, "model.safetensors")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "refs", "main"), hash+"\n")

	dir := t.TempDir()
	logPath := filepath.Join(dir, "model_dir.txt")
	script := writeFile(t, filepath.Join(dir, "fake.py"), fakeModelDirScript)
	emb, err := embed.NewQwenCommand(root, qwenMeta(), []string{pythonBin(t), script, logPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := emb.Embed(context.Background(), []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(snap)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("model dir %q, want snapshot %q", got, want)
	}
}

func TestNewQwenRejectsDanglingWeight(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.json"), "{\"hidden_size\":2560}\n")
	writeFile(t, filepath.Join(dir, "tokenizer.json"), "{}")
	if err := os.Symlink(filepath.Join(dir, "missing-blob"), filepath.Join(dir, "model.safetensors")); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	_, err := embed.NewQwenCommand(dir, qwenMeta(), []string{pythonBin(t), writeFakeTouch(t, marker)})
	if err == nil {
		t.Fatal("dangling weight symlink should fail")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("command ran for a dangling weight")
	}
}

func TestNewQwenRejectsIncompleteShards(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.json"), "{\"hidden_size\":2560}\n")
	writeFile(t, filepath.Join(dir, "tokenizer.json"), "{}")
	writeFile(t, filepath.Join(dir, "model-00001-of-00002.safetensors"), "fake")
	writeFile(t, filepath.Join(dir, "model.safetensors.index.json"), `{
		"weight_map": {
			"a": "model-00001-of-00002.safetensors",
			"b": "model-00002-of-00002.safetensors"
		}
	}`)
	_, err := embed.NewQwen(dir, qwenMeta())
	if err == nil || !strings.Contains(err.Error(), "model-00002-of-00002.safetensors") {
		t.Fatal(err)
	}
}

func TestNewQwenRejects06BCacheRoot(t *testing.T) {
	root := filepath.Join(os.Getenv("HOME"), ".cache", "huggingface", "hub", "models--Qwen--Qwen3-Embedding-0.6B")
	if _, err := os.Stat(filepath.Join(root, "refs", "main")); err != nil {
		t.Skip("local Qwen3-Embedding-0.6B cache is absent")
	}
	_, err := embed.NewQwen(root, qwenMeta())
	if err == nil || !strings.Contains(err.Error(), "1024") {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFakeTouch(t *testing.T, marker string) string {
	t.Helper()
	return writeFile(t, filepath.Join(t.TempDir(), "touch.py"), fmt.Sprintf("import pathlib\npathlib.Path(%q).touch()\n", marker))
}

func pythonBin(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func assertBatchVector(t *testing.T, vec embed.Vector, dim int, first, second float32) {
	t.Helper()
	if len(vec) != dim {
		t.Fatalf("len %d", len(vec))
	}
	if vec[0] != first || vec[1] != second {
		t.Fatalf("vector prefix %v %v", vec[0], vec[1])
	}
	for i := 2; i < len(vec); i++ {
		if vec[i] != 0 {
			t.Fatalf("vector[%d]=%v", i, vec[i])
		}
	}
}

func embedding06BDir() string {
	root := filepath.Join(os.Getenv("HOME"), ".cache", "huggingface", "hub", "models--Qwen--Qwen3-Embedding-0.6B", "snapshots")
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
			return dir
		}
	}
	return ""
}

const fakeModelDirScript = `
import json, sys
req = json.load(sys.stdin)
with open(sys.argv[1], "w", encoding="utf-8") as handle:
    handle.write(req["model_dir"])
dim = int(req["dim"])
json.dump({"vectors": [[0.0] * dim]}, sys.stdout)
`

const fakeEmbedScript = `
import json, sys
req = json.load(sys.stdin)
with open(sys.argv[1], "a", encoding="utf-8") as handle:
    handle.write(json.dumps(req["texts"], ensure_ascii=False) + "\n")
dim = int(req["dim"])
vectors = []
for index, text in enumerate(req["texts"]):
    row = [0.0] * dim
    row[0] = float(len(text))
    row[1] = float(index + 1)
    vectors.append(row)
json.dump({"vectors": vectors}, sys.stdout)
`
