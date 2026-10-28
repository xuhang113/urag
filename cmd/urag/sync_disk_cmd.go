package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
)

// syncDisk 把磁盘向量里 Milvus 还没有的子块补进集合 public。
// 已有 chunk_id 跳过。向量直接用磁盘上的，不重新嵌入，也不启动收录。
func syncDisk(args []string) error {
	if len(args) != 0 {
		return errors.New("urag: sync-disk 不接收参数")
	}
	addr, err := milvusAddr()
	if err != nil {
		return err
	}
	dataDir := strings.TrimSpace(os.Getenv("URAG_DATA_DIR"))
	if dataDir == "" {
		return errors.New("urag: 缺少 URAG_DATA_DIR，不会新建磁盘向量")
	}
	root := filepath.Join(dataDir, "vectors")
	reg, err := index.NewDisk(root, qwenMeta)
	if err != nil {
		return err
	}
	disk, ok := reg.(*index.Disk)
	if !ok {
		return errors.New("urag: 磁盘向量打不开")
	}
	writer, err := index.NewMilvusWriter(addr, qwenMeta)
	if err != nil {
		return err
	}
	defer writer.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stats, err := index.CopyMissingDiskRecords(ctx, disk, writer, model.CollectionID("public"), func(written int) {
		fmt.Printf("已写入 %d\n", written)
	})
	if err != nil {
		return err
	}
	fmt.Printf("磁盘唯一 %d，Milvus 原来 %d，写入 %d，按 chunk_id 跳过 %d，过长跳过 %d，Milvus 现在 %d\n",
		stats.DiskUnique, stats.MilvusBefore, stats.Written, stats.Skipped, stats.SkippedLong, stats.MilvusAfter)
	return nil
}
