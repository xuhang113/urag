package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
)

// elasticAddr 读取 URAG_ELASTIC_ADDR。假值只写在 .env.example。
// 空字符串直接返回，不拨号，也不猜一个地址。
func elasticAddr() (string, error) {
	addr := strings.TrimSpace(os.Getenv("URAG_ELASTIC_ADDR"))
	if addr == "" {
		return "", errors.New("urag: 缺少 URAG_ELASTIC_ADDR，BM25 不在进程内计算，不会连接默认的 Elasticsearch")
	}
	return addr, nil
}

// loadBM25 把 Milvus 里已有子块的正文写入该集合的 Elasticsearch 索引。
// 索引名与集合名相同。集合 public 写入索引 public，中文分析器是 smartcn。
// 不嵌入，不读向量，不读种子，不改断点，也不把原文写入对象存储。
func loadBM25(args []string) error {
	if len(args) != 0 {
		return errors.New("urag: load-bm25 不接收参数")
	}
	addr, err := milvusAddr()
	if err != nil {
		return err
	}
	esAddr, err := elasticAddr()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	writer, err := index.NewMilvusWriter(addr, qwenMeta)
	if err != nil {
		return err
	}
	defer writer.Close()
	keywords, err := index.NewElastic(esAddr)
	if err != nil {
		return err
	}
	defer keywords.Close()
	col := model.CollectionID(envOr("URAG_COLLECTION", "public"))
	n, err := writer.CopyBodies(ctx, col, keywords)
	if err != nil {
		return err
	}
	fmt.Printf("关键词索引完成，集合 %s 写入 %d 条\n", col, n)
	return nil
}
