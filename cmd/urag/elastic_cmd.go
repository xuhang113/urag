package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/xuhang113/urag/internal/catalog"
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

// loadBM25 把台账里的子块正文写入该集合的 Elasticsearch 索引。
// 索引名与集合名相同。集合 public 写入索引 public。
// 不嵌入，不写向量，不读种子，不改断点，也不把原文写入对象存储。
func loadBM25(args []string) error {
	if len(args) != 0 {
		return errors.New("urag: load-bm25 不接收参数")
	}
	esAddr, err := elasticAddr()
	if err != nil {
		return err
	}
	dsn := strings.TrimSpace(os.Getenv("URAG_MYSQL_DSN"))
	if dsn == "" {
		return errors.New("urag: 缺少 URAG_MYSQL_DSN，无法读取台账")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := catalog.NewMySQL(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	col := model.CollectionID(envOr("URAG_COLLECTION", "public"))
	kids, err := db.ListCollectionChildren(ctx, string(col))
	if err != nil {
		return err
	}
	keywords, err := index.NewElastic(esAddr)
	if err != nil {
		return err
	}
	defer keywords.Close()
	const batch = 2048
	for start := 0; start < len(kids); start += batch {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := start + batch
		if end > len(kids) {
			end = len(kids)
		}
		if err := keywords.UpsertRecords(ctx, keywordRecords(col, kids[start:end])); err != nil {
			return err
		}
		fmt.Printf("关键词写入 %d/%d\n", end, len(kids))
	}
	fmt.Printf("关键词索引完成，集合 %s 子块 %d\n", col, len(kids))
	return nil
}

func keywordRecords(col model.CollectionID, kids []catalog.CollectionChild) []index.VectorRecord {
	recs := make([]index.VectorRecord, len(kids))
	for i, child := range kids {
		recs[i] = index.VectorRecord{
			ChunkID:      child.ChunkID,
			ParentID:     child.ParentID,
			DocID:        child.DocID,
			Version:      child.Version,
			CollectionID: col,
			HeadingPath:  child.HeadingPath,
			Page:         child.Page,
			Body:         child.Body,
			StartByte:    child.StartByte,
			EndByte:      child.EndByte,
		}
	}
	return recs
}
