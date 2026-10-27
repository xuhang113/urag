package embed

import (
	"fmt"
	"sync"
)

// 进程里同时只驻留一种本地模型。嵌入是 embed，重排是 rerank。
const (
	localEmbedKind  = "embed"
	localRerankKind = "rerank"
)

var localModels struct {
	mu   sync.Mutex
	held string
}

// GuardLocalModel 在加载本地模型期间占住进程内的模型闸。
// kind 取 embed 或 rerank。另一种模型已经驻留时返回错误，避免两个模型同时在内存里。
// load 返回错误时不把 kind 留下；返回 nil 时 kind 保持驻留，另一种模型不能再加载。
func GuardLocalModel(kind string, load func() error) error {
	if kind != localEmbedKind && kind != localRerankKind {
		return fmt.Errorf("embed: unknown local model %q", kind)
	}
	if load == nil {
		return fmt.Errorf("embed: nil loader for %s", kind)
	}
	localModels.mu.Lock()
	defer localModels.mu.Unlock()
	if localModels.held != "" && localModels.held != kind {
		return fmt.Errorf("embed: local model %s is already loaded", localModels.held)
	}
	if err := load(); err != nil {
		return err
	}
	localModels.held = kind
	return nil
}
