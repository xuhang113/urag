package index

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

const (
	fieldChunkID      = "chunk_id"
	fieldParentID     = "parent_id"
	fieldDocID        = "doc_id"
	fieldVersion      = "version"
	fieldCollectionID = "collection_id"
	fieldHeading      = "heading_path"
	fieldPage         = "page"
	fieldBody         = "body"
	fieldVector       = "vector"
)

// dialMilvusSDK 只连接调用方给出的地址。不要在这里填默认主机或端口。
func dialMilvusSDK(ctx context.Context, addr string) (milvusSession, error) {
	cli, err := client.NewClient(ctx, client.Config{Address: addr})
	if err != nil {
		return nil, err
	}
	return &sdkSession{cli: cli}, nil
}

type sdkSession struct {
	cli client.Client
}

func (s *sdkSession) Close() error {
	if s == nil || s.cli == nil {
		return nil
	}
	return s.cli.Close()
}

func (s *sdkSession) Has(ctx context.Context, name string) (bool, error) {
	return s.cli.HasCollection(ctx, name)
}

func (s *sdkSession) Create(ctx context.Context, name string, dim int) error {
	schema := entity.NewSchema().WithName(name).WithDescription("urag child chunks").WithAutoID(false).
		WithField(entity.NewField().WithName(fieldChunkID).WithDataType(entity.FieldTypeVarChar).WithIsPrimaryKey(true).WithIsAutoID(false).WithMaxLength(512)).
		WithField(entity.NewField().WithName(fieldParentID).WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().WithName(fieldDocID).WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().WithName(fieldVersion).WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().WithName(fieldCollectionID).WithDataType(entity.FieldTypeVarChar).WithMaxLength(128)).
		WithField(entity.NewField().WithName(fieldHeading).WithDataType(entity.FieldTypeVarChar).WithMaxLength(milvusTextMax)).
		WithField(entity.NewField().WithName(fieldPage).WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().WithName(fieldBody).WithDataType(entity.FieldTypeVarChar).WithMaxLength(milvusTextMax)).
		WithField(entity.NewField().WithName(fieldVector).WithDataType(entity.FieldTypeFloatVector).WithDim(int64(dim)))
	if err := s.cli.CreateCollection(ctx, schema, entity.DefaultShardNumber); err != nil {
		return err
	}
	idx, err := entity.NewIndexHNSW(entity.COSINE, 16, 200)
	if err != nil {
		return err
	}
	if err := s.cli.CreateIndex(ctx, name, fieldVector, idx, false); err != nil {
		return err
	}
	return s.cli.LoadCollection(ctx, name, false)
}

func (s *sdkSession) Dim(ctx context.Context, name string) (int, error) {
	coll, err := s.cli.DescribeCollection(ctx, name)
	if err != nil {
		return 0, err
	}
	if coll.Schema == nil {
		return 0, fmt.Errorf("index: milvus collection %s has no schema", name)
	}
	for _, field := range coll.Schema.Fields {
		if field.Name != fieldVector {
			continue
		}
		raw, ok := field.TypeParams[entity.TypeParamDim]
		if !ok {
			return 0, fmt.Errorf("index: milvus collection %s has no vector dim", name)
		}
		dim, err := strconv.Atoi(raw)
		if err != nil || dim <= 0 {
			return 0, fmt.Errorf("index: milvus collection %s dim %q", name, raw)
		}
		return dim, nil
	}
	return 0, fmt.Errorf("index: milvus collection %s has no vector field", name)
}

func (s *sdkSession) Upsert(ctx context.Context, name string, rows []milvusRow) error {
	if len(rows) == 0 {
		return nil
	}
	dim := len(rows[0].Vector)
	ids := make([]string, len(rows))
	parents := make([]int64, len(rows))
	docs := make([]int64, len(rows))
	versions := make([]int64, len(rows))
	collections := make([]string, len(rows))
	headings := make([]string, len(rows))
	pages := make([]int64, len(rows))
	bodies := make([]string, len(rows))
	vecs := make([][]float32, len(rows))
	for i, row := range rows {
		if len(row.Vector) != dim {
			return fmt.Errorf("index: vector length %d, want %d", len(row.Vector), dim)
		}
		ids[i] = row.ChunkID
		parents[i] = row.ParentID
		docs[i] = row.DocID
		versions[i] = row.Version
		collections[i] = row.CollectionID
		headings[i] = row.HeadingPath
		pages[i] = row.Page
		bodies[i] = row.Body
		vecs[i] = row.Vector
	}
	_, err := s.cli.Upsert(ctx, name, "",
		entity.NewColumnVarChar(fieldChunkID, ids),
		entity.NewColumnInt64(fieldParentID, parents),
		entity.NewColumnInt64(fieldDocID, docs),
		entity.NewColumnInt64(fieldVersion, versions),
		entity.NewColumnVarChar(fieldCollectionID, collections),
		entity.NewColumnVarChar(fieldHeading, headings),
		entity.NewColumnInt64(fieldPage, pages),
		entity.NewColumnVarChar(fieldBody, bodies),
		entity.NewColumnFloatVector(fieldVector, dim, vecs),
	)
	return err
}

func (s *sdkSession) Flush(ctx context.Context, name string) error {
	return s.cli.Flush(ctx, name, false)
}

func (s *sdkSession) Search(ctx context.Context, name string, vec []float32, topK int) ([]milvusHit, error) {
	if err := s.cli.LoadCollection(ctx, name, false); err != nil {
		return nil, err
	}
	sp, err := entity.NewIndexHNSWSearchParam(64)
	if err != nil {
		return nil, err
	}
	// COSINE 分数越大越近。小于等于 0 的由调用方丢掉。
	results, err := s.cli.Search(ctx, name, nil, "", milvusOutputFields(),
		[]entity.Vector{entity.FloatVector(vec)}, fieldVector, entity.COSINE, topK, sp)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	res := results[0]
	if res.Err != nil {
		return nil, res.Err
	}
	out := make([]milvusHit, 0, res.ResultCount)
	for i := 0; i < res.ResultCount; i++ {
		if i >= len(res.Scores) {
			return nil, fmt.Errorf("index: milvus score %d missing", i)
		}
		score := float64(res.Scores[i])
		if score <= 0 || math.IsNaN(score) {
			continue
		}
		row, err := rowFromResult(res, i)
		if err != nil {
			return nil, err
		}
		if row.CollectionID != name {
			continue
		}
		out = append(out, milvusHit{Row: row, Score: score})
	}
	return out, nil
}

func milvusOutputFields() []string {
	return []string{
		fieldChunkID,
		fieldParentID,
		fieldDocID,
		fieldVersion,
		fieldCollectionID,
		fieldHeading,
		fieldPage,
		fieldBody,
	}
}

func rowFromResult(res client.SearchResult, i int) (milvusRow, error) {
	chunkID, err := resultString(res, fieldChunkID, i)
	if err != nil {
		return milvusRow{}, err
	}
	parentID, err := resultInt64(res, fieldParentID, i)
	if err != nil {
		return milvusRow{}, err
	}
	docID, err := resultInt64(res, fieldDocID, i)
	if err != nil {
		return milvusRow{}, err
	}
	version, err := resultInt64(res, fieldVersion, i)
	if err != nil {
		return milvusRow{}, err
	}
	if version < 0 {
		return milvusRow{}, fmt.Errorf("index: negative version")
	}
	collectionID, err := resultString(res, fieldCollectionID, i)
	if err != nil {
		return milvusRow{}, err
	}
	heading, err := resultString(res, fieldHeading, i)
	if err != nil {
		return milvusRow{}, err
	}
	page, err := resultInt64(res, fieldPage, i)
	if err != nil {
		return milvusRow{}, err
	}
	body, err := resultString(res, fieldBody, i)
	if err != nil {
		return milvusRow{}, err
	}
	return milvusRow{
		ChunkID:      chunkID,
		ParentID:     parentID,
		DocID:        docID,
		Version:      version,
		CollectionID: collectionID,
		HeadingPath:  heading,
		Page:         page,
		Body:         body,
	}, nil
}

func resultString(res client.SearchResult, name string, i int) (string, error) {
	col := res.Fields.GetColumn(name)
	if col == nil && name == fieldChunkID {
		col = res.IDs
	}
	if col == nil {
		return "", fmt.Errorf("index: milvus result missing %s", name)
	}
	switch c := col.(type) {
	case *entity.ColumnVarChar:
		return c.ValueByIdx(i)
	default:
		return "", fmt.Errorf("index: milvus field %s has type %T", name, col)
	}
}

func resultInt64(res client.SearchResult, name string, i int) (int64, error) {
	col := res.Fields.GetColumn(name)
	if col == nil {
		return 0, fmt.Errorf("index: milvus result missing %s", name)
	}
	switch c := col.(type) {
	case *entity.ColumnInt64:
		return c.ValueByIdx(i)
	default:
		return 0, fmt.Errorf("index: milvus field %s has type %T", name, col)
	}
}
