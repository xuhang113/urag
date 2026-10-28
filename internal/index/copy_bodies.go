package index

import (
	"context"
	"errors"

	"github.com/xuhang113/urag/internal/model"
)

// iterMilvusChunks 读出一个集合的子块正文。测试替换它。默认实现不读向量。
var iterMilvusChunks = func(ctx context.Context, session milvusSession, name string, fn func(milvusRow) error) error {
	sdk, ok := session.(*sdkSession)
	if !ok {
		return errors.New("index: milvus body copy is unavailable")
	}
	return sdk.ForEachChunk(ctx, name, fn)
}

// CopyBodies 把点名集合里已经写在 Milvus 的子块正文写入 Elasticsearch。
// 索引名与集合名相同。不读向量，不嵌入，也不把 PDF 或 DOCX 原文写入对象存储。
// 集合不存在时返回错误，不创建。
func (m *Milvus) CopyBodies(ctx context.Context, id model.CollectionID, dst *Elastic) (int, error) {
	if m == nil || dst == nil {
		return 0, errors.New("index: nil body copy")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	session, name, err := m.ensure(ctx, id, false)
	if err != nil {
		return 0, err
	}
	const batchN = 128
	batch := make([]VectorRecord, 0, batchN)
	written := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := dst.UpsertRecords(ctx, batch); err != nil {
			return err
		}
		written += len(batch)
		batch = batch[:0]
		return nil
	}
	err = iterMilvusChunks(ctx, session, name, func(row milvusRow) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if row.ChunkID == "" {
			return nil
		}
		if row.CollectionID != "" && row.CollectionID != string(id) && row.CollectionID != name {
			return nil
		}
		rec := VectorRecord{
			ChunkID:      row.ChunkID,
			ParentID:     row.ParentID,
			DocID:        row.DocID,
			Version:      int(row.Version),
			CollectionID: id,
			HeadingPath:  row.HeadingPath,
			Page:         int(row.Page),
			Body:         row.Body,
		}
		batch = append(batch, rec)
		if len(batch) >= batchN {
			return flush()
		}
		return nil
	})
	if err != nil {
		return written, err
	}
	if err := flush(); err != nil {
		return written, err
	}
	return written, nil
}
