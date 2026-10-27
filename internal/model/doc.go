// Package model 定义集合、文档、片段和血缘这些共享类型。
// 本包不负责存储、清洗、切分、嵌入、索引、检索或 HTTP。
// 一份文档只属于一个集合。
package model

// CollectionID 标识一个集合。集合是隔离边界。
type CollectionID string

// DocumentID 标识一篇文档。文档只属于一个集合。
type DocumentID string

// ChunkID 标识一个片段。格式见 Chunk。
type ChunkID string

// ObjectKey 是原始对象在存储里的键。
type ObjectKey string

// IdempotencyKey 标识一次写入。相同的非空键对应同一份已提交对象。
type IdempotencyKey string

// Checksum 是小写十六进制 SHA-256。
type Checksum string

// CleanStep 是清洗血缘里的一步。
// Name 取 detect、strip、dedupe。Detail 记录这一步的结果。
type CleanStep struct {
	Name   string
	Detail string
}

// Lineage 记录文档怎样从原始对象而来。
// DuplicateOf 为空表示原文；非空表示这篇是重复文档，不进入索引。
// RawObjectKey 是清洗前的对象键。Steps 按发生顺序保留。
// MediaType 取 txt、md、html、pdf。Encoding 是识别到的原始编码。
type Lineage struct {
	RawObjectKey ObjectKey
	DocumentID   DocumentID
	CollectionID CollectionID
	ContentHash  string
	ChunkIDs     []ChunkID
	Steps        []CleanStep
	DuplicateOf  DocumentID
	Encoding     string
	MediaType    string
	SourceURI    string
}

// Document 是一个集合里的一篇正文。
// Text 是 UTF-8。CollectionID 与 Lineage.CollectionID 相同。
// Duplicate 为 true 时这篇不进入索引，Lineage.DuplicateOf 指向已有文档。
type Document struct {
	ID           DocumentID
	CollectionID CollectionID
	Text         string
	Lineage      Lineage
	Duplicate    bool
}

// Chunk 是文档正文上的一个字节区间。
// ChunkID 的格式固定为 documentID + "#" + ordinal。
// StartByte 与 EndByte 是 Document.Text 的字节下标，片段正文是 Text[StartByte:EndByte]。
// 下标从 0 起，EndByte 不包含在区间内。CollectionID 与所属文档的集合相同。
type Chunk struct {
	ID           ChunkID
	DocumentID   DocumentID
	CollectionID CollectionID
	Ordinal      int
	Text         string
	StartByte    int
	EndByte      int
}
