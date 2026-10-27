package acquire

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type localDir struct {
	root string
}

// NewLocalDir 准备根目录 root 下的本地来源。
// Kind 为 KindLocalDir。List 只返回 root 这一层的文件，跳过目录，不递归。
// 每条 SourceURI 是该文件路径，Filename 是基名。Open 返回文件流，调用方负责关闭。
// root 不存在或不是目录时返回错误，不列出条目。
func NewLocalDir(root string) (Source, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("acquire: %s is not a directory", root)
	}
	return &localDir{root: root}, nil
}

func (s *localDir) Kind() string { return KindLocalDir }

func (s *localDir) List(ctx context.Context) ([]Item, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := filepath.Join(s.root, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			continue
		}
		items = append(items, Item{
			SourceURI:   path,
			Filename:    filepath.Base(path),
			ContentType: contentTypeOf(entry.Name()),
			Open:        openLocal(path),
		})
	}
	return items, nil
}

func openLocal(path string) func(context.Context) (io.ReadCloser, error) {
	return func(ctx context.Context) (io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return os.Open(path)
	}
}
