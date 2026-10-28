package index

import (
	"context"
	"fmt"

	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/model"
)

const diskSyncBatch = 64

// DiskSyncStats 是一次把磁盘向量补进 Milvus 的计数。
// 已有的 chunk_id 整条跳过，不比较向量，也不重新嵌入。
type DiskSyncStats struct {
	DiskUnique   int
	MilvusBefore int
	Written      int
	Skipped      int
	SkippedLong  int
	MilvusAfter  int
}

// CopyMissingDiskRecords 把磁盘上 Milvus 还没有的子块写进同一个集合。
// 主键是 chunk_id。集合里已经有的 id 跳过，磁盘里同一个 id 以后出现的为准。
// 向量用磁盘上的那一份。正文超过 Milvus 字段上限的不写。
func CopyMissingDiskRecords(ctx context.Context, disk *Disk, dst *Milvus, id model.CollectionID, progress func(written int)) (DiskSyncStats, error) {
	var stats DiskSyncStats
	if disk == nil || dst == nil {
		return stats, fmt.Errorf("index: nil disk sync")
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	have, err := dst.ChunkVersions(ctx, id)
	if err != nil {
		return stats, err
	}
	stats.MilvusBefore = len(have)

	seen := make(map[string]struct{})
	long := make(map[string]struct{})
	flushed := make(map[string]struct{})
	batch := make([]VectorRecord, 0, diskSyncBatch)
	pos := make(map[string]int)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := dst.UpsertRecords(ctx, batch); err != nil {
			return err
		}
		for _, rec := range batch {
			flushed[rec.ChunkID] = struct{}{}
		}
		stats.Written += len(batch)
		batch = batch[:0]
		pos = make(map[string]int)
		if progress != nil {
			progress(stats.Written)
		}
		return nil
	}

	err = disk.walkRecords(ctx, id, func(rec VectorRecord, vec []float32) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if rec.ChunkID == "" {
			return fmt.Errorf("index: empty chunk id")
		}
		if rec.CollectionID != id {
			return fmt.Errorf("index: chunk %s belongs to %s", rec.ChunkID, rec.CollectionID)
		}
		if len(vec) != disk.meta.Dim {
			return fmt.Errorf("index: vector length %d, want %d", len(vec), disk.meta.Dim)
		}
		seen[rec.ChunkID] = struct{}{}
		if _, ok := have[rec.ChunkID]; ok {
			return nil
		}
		if len(rec.Body) > milvusTextMax || len(rec.HeadingPath) > milvusTextMax || len(rec.ChunkID) > 512 {
			if _, ok := pos[rec.ChunkID]; !ok {
				if _, ok := flushed[rec.ChunkID]; !ok {
					long[rec.ChunkID] = struct{}{}
				}
			}
			return nil
		}
		delete(long, rec.ChunkID)
		rec.Vector = append(embed.Vector(nil), vec...)
		if _, ok := flushed[rec.ChunkID]; ok {
			return dst.UpsertRecords(ctx, []VectorRecord{rec})
		}
		if i, ok := pos[rec.ChunkID]; ok {
			batch[i] = rec
			return nil
		}
		pos[rec.ChunkID] = len(batch)
		batch = append(batch, rec)
		if len(batch) >= diskSyncBatch {
			return flush()
		}
		return nil
	})
	if err != nil {
		return stats, err
	}
	if err := flush(); err != nil {
		return stats, err
	}
	if err := dst.FlushCollection(ctx, id); err != nil {
		return stats, err
	}
	stats.DiskUnique = len(seen)
	stats.SkippedLong = len(long)
	stats.Skipped = stats.DiskUnique - stats.Written - stats.SkippedLong
	if stats.Skipped < 0 {
		stats.Skipped = 0
	}
	after, err := dst.Count(ctx, id)
	if err != nil {
		return stats, err
	}
	stats.MilvusAfter = after
	return stats, nil
}

func (d *Disk) walkRecords(ctx context.Context, id model.CollectionID, fn func(VectorRecord, []float32) error) error {
	if d == nil {
		return fmt.Errorf("index: nil disk")
	}
	opened, err := d.Open(ctx, id)
	if err != nil {
		return err
	}
	idx, ok := opened.(*diskIndex)
	if !ok {
		return fmt.Errorf("index: disk collection %s", id)
	}
	return idx.scan(ctx, func(rec storedRecord, vec []float32) error {
		out := rec.rec
		out.StartByte = rec.startByte
		out.EndByte = rec.endByte
		return fn(out, vec)
	})
}
