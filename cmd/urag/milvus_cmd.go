package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/xuhang113/urag/internal/catalog"
	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/job"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
)

// milvusAddr 读取 URAG_MILVUS_ADDR。假值只写在 .env.example。
// 空字符串直接返回，不拨号，也不猜一个地址。
func milvusAddr() (string, error) {
	addr := strings.TrimSpace(os.Getenv("URAG_MILVUS_ADDR"))
	if addr == "" {
		return "", errors.New("urag: 缺少 URAG_MILVUS_ADDR，不会连接默认的 Milvus")
	}
	return addr, nil
}

// indexCollection 只嵌入台账里的子块并写入该集合的 Milvus collection。
// 只加载嵌入模型。不加载重排模型，不读种子，不改断点，不把原文写入对象存储。
func indexCollection(args []string) error {
	if len(args) != 0 {
		return errors.New("urag: index 不接收参数")
	}
	addr, err := milvusAddr()
	if err != nil {
		return err
	}
	dsn := strings.TrimSpace(os.Getenv("URAG_MYSQL_DSN"))
	if dsn == "" {
		return errors.New("urag: 缺少 URAG_MYSQL_DSN，无法读取台账")
	}
	modelDir := strings.TrimSpace(os.Getenv("URAG_EMBED_MODEL_DIR"))
	if modelDir == "" {
		return errors.New("urag: 缺少 URAG_EMBED_MODEL_DIR，不会下载嵌入模型")
	}
	info, err := os.Stat(modelDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("urag: 嵌入权重目录 %s 不存在，不会下载", modelDir)
	}
	cfg, err := configFromEnv()
	if err != nil {
		return err
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
	emb, err := embed.NewQwen(modelDir, qwenMeta)
	if err != nil {
		return err
	}
	starter, ok := emb.(interface {
		Start(context.Context) error
		Close() error
	})
	if !ok {
		return errors.New("urag: 嵌入进程无法常驻")
	}
	if err := starter.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = starter.Close() }()
	writer, err := index.NewMilvusWriter(addr, qwenMeta)
	if err != nil {
		return err
	}
	defer writer.Close()
	if err := job.WriteChildVectors(ctx, emb, writer, col, kids, cfg.Limits.EmbedBatch); err != nil {
		return err
	}
	fmt.Printf("索引完成，集合 %s 子块 %d\n", col, len(kids))
	return nil
}

// searchCollection 在一个点名集合里做向量检索和 BM25，再重排前 50 个子块。
// 嵌入进程关掉之后才加载重排模型。重排只出分数，不生成答案。
// 分数必须高于 URAG_RERANK_MIN_SCORE 才写入输出，可以一条都没有。
// 夹具评价不走这个命令。
func searchCollection(args []string) error {
	question := strings.TrimSpace(strings.Join(args, " "))
	if question == "" {
		return errors.New("urag: search 需要问题")
	}
	addr, err := milvusAddr()
	if err != nil {
		return err
	}
	dsn := strings.TrimSpace(os.Getenv("URAG_MYSQL_DSN"))
	if dsn == "" {
		return errors.New("urag: 缺少 URAG_MYSQL_DSN，无法读取台账")
	}
	modelDir := strings.TrimSpace(os.Getenv("URAG_EMBED_MODEL_DIR"))
	if modelDir == "" {
		return errors.New("urag: 缺少 URAG_EMBED_MODEL_DIR，不会下载嵌入模型")
	}
	info, err := os.Stat(modelDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("urag: 嵌入权重目录 %s 不存在，不会下载", modelDir)
	}
	rdir, err := rerankDir(modelDir)
	if err != nil {
		return err
	}
	cfg, err := configFromEnv()
	if err != nil {
		return err
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
	keywords := index.NewMemory()
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
	if err := index.WriteKeywords(ctx, keywords, recs); err != nil {
		return err
	}
	reg, closer, err := index.NewMilvus(addr, qwenMeta, keywords)
	if err != nil {
		return err
	}
	defer closer.Close()
	rr, err := retrieve.NewQwenReranker(rdir)
	if err != nil {
		return err
	}
	emb, err := embed.NewQwen(modelDir, qwenMeta)
	if err != nil {
		return err
	}
	starter, ok := emb.(interface {
		embed.Embedder
		Start(context.Context) error
		Close() error
	})
	if !ok {
		return errors.New("urag: 嵌入进程无法常驻")
	}
	if err := starter.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = starter.Close() }()
	// Embed 返回前关掉嵌入进程，RetrieveChildren 随后才调用重排。
	wrapped := &releaseEmbedder{inner: starter, release: starter.Close}
	svc := retrieve.New(reg, wrapped, cfg.Weights, nil)
	hits, err := svc.RetrieveChildren(ctx, retrieve.Request{
		Collections: []model.CollectionID{col},
		Query:       question,
	}, rr, storeParents{store: db})
	if err != nil {
		return err
	}
	hits = retrieve.KeepAboveRerankScore(hits, cfg.RerankMinScore)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(hits)
}

// rerankDir 从嵌入权重目录的同级缓存找到 Qwen3-Reranker-4B。
// 找不到就返回错误，不下载，也不另读一个环境变量。
func rerankDir(embedDir string) (string, error) {
	const embedName = "models--Qwen--Qwen3-Embedding-4B"
	const rerankName = "models--Qwen--Qwen3-Reranker-4B"
	dir := embedDir
	for i := 0; i < 6; i++ {
		if filepath.Base(dir) == embedName {
			cand := filepath.Join(filepath.Dir(dir), rerankName)
			info, err := os.Stat(cand)
			if err != nil || !info.IsDir() {
				return "", fmt.Errorf("urag: 重排权重目录 %s 不存在，不会下载", cand)
			}
			return cand, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("urag: 找不到 Qwen3-Reranker-4B 的本地目录，不会下载")
}

// releaseEmbedder 在一次 Embed 返回前调用 release，把嵌入进程让给重排。
type releaseEmbedder struct {
	inner   embed.Embedder
	release func() error
}

func (r *releaseEmbedder) Dim() int {
	if r == nil || r.inner == nil {
		return 0
	}
	return r.inner.Dim()
}

func (r *releaseEmbedder) Embed(ctx context.Context, texts []string) ([]embed.Vector, error) {
	defer func() {
		if r.release != nil {
			_ = r.release()
			r.release = nil
		}
	}()
	if r == nil || r.inner == nil {
		return nil, errors.New("urag: nil embedder")
	}
	return r.inner.Embed(ctx, texts)
}

type storeParents struct {
	store catalog.Store
}

func (p storeParents) GetParent(ctx context.Context, id int64) (string, error) {
	if p.store == nil {
		return "", errors.New("urag: nil catalog")
	}
	row, err := p.store.GetParent(ctx, id)
	if err != nil {
		return "", err
	}
	return row.Body, nil
}
