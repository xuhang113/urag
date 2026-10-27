package embed

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveSnapshot 把目录收成含 config.json 的本地快照。
// 目录本身已有 config.json 时直接使用。
// Hugging Face 缓存根目录则只读 refs/main 指向的快照，不下载，也不创建目录。
func ResolveSnapshot(dir, label string) (string, error) {
	if fileReady(filepath.Join(dir, "config.json")) {
		return dir, nil
	}
	refPath := filepath.Join(dir, "refs", "main")
	raw, err := os.ReadFile(refPath)
	if err == nil {
		hash := strings.TrimSpace(string(raw))
		if !validSnapshotID(hash) {
			return "", fmt.Errorf("%s invalid snapshot ref in %q, refusing to download", label, dir)
		}
		snap := filepath.Join(dir, "snapshots", hash)
		info, statErr := os.Stat(snap)
		if statErr != nil || !info.IsDir() {
			return "", fmt.Errorf("%s snapshot %s missing in %q, refusing to download", label, hash, dir)
		}
		return snap, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	snaps := filepath.Join(dir, "snapshots")
	entries, readErr := os.ReadDir(snaps)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return dir, nil
		}
		return "", readErr
	}
	var found []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		cand := filepath.Join(snaps, entry.Name())
		if fileReady(filepath.Join(cand, "config.json")) {
			found = append(found, cand)
		}
	}
	switch len(found) {
	case 0:
		return dir, nil
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("%s multiple snapshots in %q, refusing to download", label, dir)
	}
}

// WeightsReady 确认目录里的权重文件都能在本机打开。
// 有分片索引时，每一片都必须存在且不是空文件。断掉的符号链接算缺失。
// 不读取张量，也不下载。
func WeightsReady(dir, label string) error {
	indexPath := filepath.Join(dir, "model.safetensors.index.json")
	raw, err := os.ReadFile(indexPath)
	if err == nil {
		var index struct {
			WeightMap map[string]string `json:"weight_map"`
		}
		if err := json.Unmarshal(raw, &index); err != nil {
			return fmt.Errorf("%s weight index in %q: %w", label, dir, err)
		}
		if len(index.WeightMap) == 0 {
			return fmt.Errorf("%s weight index in %q is empty, refusing to download", label, dir)
		}
		seen := make(map[string]struct{})
		for _, name := range index.WeightMap {
			if !validShardName(name) {
				return fmt.Errorf("%s invalid weight shard %q, refusing to download", label, name)
			}
			seen[name] = struct{}{}
		}
		for name := range seen {
			path := filepath.Join(dir, name)
			if !fileReady(path) {
				return fmt.Errorf("%s weight file missing in %q, refusing to download", label, path)
			}
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.ToLower(entry.Name())
		if !isWeightName(name) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if fileReady(path) {
			return nil
		}
	}
	return fmt.Errorf("%s weight file missing in %q, refusing to download", label, dir)
}

// TokenizerReady 确认本机有分词器文件。没有就返回错误，不下载。
func TokenizerReady(dir, label string) error {
	for _, name := range []string{"tokenizer.json", "vocab.json"} {
		if fileReady(filepath.Join(dir, name)) {
			return nil
		}
	}
	return fmt.Errorf("%s tokenizer missing in %q, refusing to download", label, dir)
}

func fileReady(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
}

func isWeightName(name string) bool {
	return strings.HasSuffix(name, ".safetensors") || strings.HasSuffix(name, ".gguf") || name == "pytorch_model.bin"
}

func validShardName(name string) bool {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return false
	}
	return isWeightName(strings.ToLower(name))
}

func validSnapshotID(hash string) bool {
	if hash == "" || len(hash) > 128 {
		return false
	}
	for _, r := range hash {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}
