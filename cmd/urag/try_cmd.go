package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/xuhang113/urag/internal/catalog"
	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/clean"
	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/job"
	"github.com/xuhang113/urag/internal/retrieve"
	"github.com/xuhang113/urag/internal/storage"
	"github.com/xuhang113/urag/internal/web"
)

// tryPage 在本机打开磁盘向量试查页。
// 只打开集合 public。不连接 Milvus，不新建向量文件，也不把连接串写进输出。
// 嵌入模型按次启动，返回查询向量后退出，然后才加载重排模型。两个权重不会一起留在内存里。
func tryPage(args []string) error {
	if len(args) != 0 {
		return errors.New("urag: try 不接收参数")
	}
	dataDir := strings.TrimSpace(os.Getenv("URAG_DATA_DIR"))
	if dataDir == "" {
		return errors.New("urag: 缺少 URAG_DATA_DIR，不会新建磁盘向量")
	}
	root := filepath.Join(dataDir, "vectors")
	if _, err := os.Stat(filepath.Join(root, "meta.json")); err != nil {
		return errors.New("urag: 磁盘向量元数据不存在，不会新建")
	}
	if _, err := os.Stat(filepath.Join(root, "collections", "public", "records.bin")); err != nil {
		return errors.New("urag: 集合 public 的磁盘向量不存在，不会新建")
	}
	modelDir := strings.TrimSpace(os.Getenv("URAG_EMBED_MODEL_DIR"))
	if modelDir == "" {
		return errors.New("urag: 缺少 URAG_EMBED_MODEL_DIR，不会下载嵌入模型")
	}
	info, err := os.Stat(modelDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("urag: 嵌入权重目录 %s 不存在，不会下载", modelDir)
	}
	rdir, err := tryRerankDir(modelDir)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("python3"); err != nil {
		return errors.New("urag: 找不到 python3，不会下载运行时")
	}
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	reg, err := index.NewDisk(root, qwenMeta)
	if err != nil {
		return err
	}
	emb, err := embed.NewQwen(modelDir, qwenMeta)
	if err != nil {
		return err
	}
	rr, err := retrieve.NewQwenReranker(rdir)
	if err != nil {
		return err
	}
	parents, closeParents, err := openTryParents()
	if err != nil {
		return err
	}
	defer closeParents()
	// 对象目录只满足管道构造。不使用收录目录，避免碰正在写入的原文旁路文件。
	store, err := storage.NewLocal(filepath.Join(os.TempDir(), "urag-try-objects"), cfg.Retry, nil)
	if err != nil {
		return err
	}
	pipeline, err := job.New(cfg, job.Deps{
		Store:    store,
		Embedder: emb,
		Registry: reg,
		Reranker: retrieve.NewOverlapReranker(),
		Splitter: chunk.NewParagraph(),
		Cleaner:  clean.New(clean.NewMemoryDeduper()),
		Passages: rr,
		Parents:  parents,
	})
	if err != nil {
		return err
	}
	ln, addr, err := listenTry()
	if err != nil {
		return err
	}
	fmt.Printf("试查页面 http://%s/try\n", addr)
	srv := &http.Server{
		Handler:           web.Handler(pipeline),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func listenTry() (net.Listener, string, error) {
	var last error
	for _, addr := range []string{"127.0.0.1:8080", "127.0.0.1:8088"} {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, addr, nil
		}
		last = err
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("urag: 127.0.0.1:8080 和 127.0.0.1:8088 都无法监听: %w", last)
}

// tryRerankDir 优先使用已经设好的 URAG_RERANK_MODEL_DIR。
// 没有该变量时，找嵌入目录旁边的 Qwen3-Reranker-4B，再退回缓存目录名。
// 找不到就返回错误，不下载。
func tryRerankDir(embedDir string) (string, error) {
	if dir := strings.TrimSpace(os.Getenv("URAG_RERANK_MODEL_DIR")); dir != "" {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			return "", fmt.Errorf("urag: 重排权重目录 %s 不存在，不会下载", dir)
		}
		return dir, nil
	}
	sibling := filepath.Join(filepath.Dir(embedDir), "Qwen3-Reranker-4B")
	if info, err := os.Stat(sibling); err == nil && info.IsDir() {
		return sibling, nil
	}
	return rerankDir(embedDir)
}

// openTryParents 在环境里已有台账连接时读取父块。
// 没有该变量时不连接。错误文本不带连接串。
func openTryParents() (retrieve.ParentLookup, func(), error) {
	if strings.TrimSpace(os.Getenv("URAG_MYSQL_DSN")) == "" {
		return nil, func() {}, nil
	}
	db, err := catalog.NewMySQL(os.Getenv("URAG_MYSQL_DSN"))
	if err != nil {
		return nil, nil, errors.New("urag: 台账连接无法准备")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.CountIndexed(ctx, "public"); err != nil {
		_ = db.Close()
		return nil, nil, errors.New("urag: 台账无法读取父块")
	}
	return storeParents{store: db}, func() { _ = db.Close() }, nil
}
