package job

import (
	"context"
	"errors"
	"fmt"

	"github.com/xuhang113/urag/internal/catalog"
	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
)

// WriteChildVectors 只嵌入子块。嵌入文本是标题路径加正文，不加查询指令。
// 写入的正文是子块原文，不是 PDF 或 DOCX，也不加载重排模型。
// 不读种子，不改断点。
func WriteChildVectors(ctx context.Context, emb embed.Embedder, dst VectorWriter, col model.CollectionID, kids []catalog.CollectionChild, batch int) error {
	if emb == nil || dst == nil {
		return errMissingDeps
	}
	if col == "" {
		return errors.New("job: collection required")
	}
	batch = forwardBatch(batch)
	for start := 0; start < len(kids); start += batch {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := start + batch
		if end > len(kids) {
			end = len(kids)
		}
		part := kids[start:end]
		texts := make([]string, len(part))
		for i, child := range part {
			if child.ChunkID == "" || child.DocID <= 0 {
				return errors.New("job: child chunk required")
			}
			texts[i] = chunk.Passage(child.HeadingPath, child.Body)
		}
		vecs, err := emb.Embed(ctx, texts)
		if err != nil {
			return err
		}
		if len(vecs) != len(texts) {
			return fmt.Errorf("job: embedder returned %d vectors for %d texts", len(vecs), len(texts))
		}
		recs := make([]index.VectorRecord, len(part))
		for i, child := range part {
			recs[i] = index.VectorRecord{
				ChunkID:      child.ChunkID,
				ParentID:     child.ParentID,
				DocID:        child.DocID,
				Version:      child.Version,
				CollectionID: col,
				HeadingPath:  child.HeadingPath,
				Page:         child.Page,
				Body:         child.Body,
				Vector:       vecs[i],
				StartByte:    child.StartByte,
				EndByte:      child.EndByte,
			}
		}
		if err := dst.UpsertRecords(ctx, recs); err != nil {
			return err
		}
	}
	return nil
}
