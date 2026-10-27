// Package catalog 记录来源、父级章节和子块。
// 保存的正文是清洗后的 UTF-8。本包不保存 PDF 或 DOCX 字节，不保存向量，也不把原文放进对象存储。
// doc_id 和 parent_id 在写入时生成。chunk_id 由文档编号和序号拼成。
// 测试使用 NewMemory，不连接 MySQL，不访问外网。
package catalog

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxCollectionID = 128
	maxSourceURI    = 512
	maxTitle        = 512
	maxMediaType    = 128
	maxLicense      = 128
	maxTextBytes    = 65535
	maxMediumBytes  = 16_777_215
	maxVersion      = math.MaxInt32
)

var errVersionOverflow = errors.New("catalog: version overflow")

var (
	// ErrNotFound 表示文档、父块或子块不存在。
	ErrNotFound = errors.New("catalog: not found")

	// ErrConflict 表示同一个文档里已经有这个序号的子块。
	ErrConflict = errors.New("catalog: already exists")
)

// Store 是来源台账。
// 内存实现和 MySQL 实现都遵守这些规则。
// 同一集合的同一个 source_uri 只有一行。
// CountIndexed 数的是 source_document 行，不是子块。
type Store interface {
	// PutDocument 写入一篇来源。doc_id 由存储生成，调用方传入的 DocID 被忽略。
	// 首次插入保存调用方传入的 Version。
	// 同一 collection_id 和 source_uri 再次写入时不产生第二行，返回已有 doc_id。
	// 校验和相同则整行保持不变。校验和不同则已保存的 version 加 1，仍是同一个 doc_id，
	// 并更新校验和、媒体类型、许可、标题和抓取时间。旧的父块和子块留到 ReplaceSections。
	PutDocument(ctx context.Context, in Document) (docID int64, err error)

	// PutParent 写入一节。parent_id 由存储生成。文档不存在时返回 ErrNotFound。
	PutParent(ctx context.Context, in Parent) (parentID int64, err error)

	// PutChild 写入一个子块。chunk_id 由存储按 ChunkID 生成，调用方传入的 ChunkID 被忽略。
	// 父块不存在，或父块不属于该文档时返回 ErrNotFound。序号已存在时返回 ErrConflict。
	PutChild(ctx context.Context, in Child) error

	// GetParent 取回一整节，包含正文。父块不存在时返回 ErrNotFound。
	GetParent(ctx context.Context, parentID int64) (Parent, error)

	// ListChildren 返回一篇文档的全部子块，按序号升序。
	// 文档不存在时返回 ErrNotFound。文档存在但没有子块时返回空切片。
	ListChildren(ctx context.Context, docID int64) ([]Child, error)

	// CountIndexed 返回该集合的 source_document 行数。
	// 表里没有重复标记。调用方只写入清洗后不是重复、且至少有一个子块的文档。
	// 本方法不检查子块，集合没有来源时返回 0。
	CountIndexed(ctx context.Context, collectionID string) (int, error)

	// ReplaceSections 在一个事务里换掉该文档的全部父块和子块。
	// 文档不存在时返回 ErrNotFound，并不写入新段落。
	// parents 为空时删掉旧段落，来源行仍在。
	// 新的 parent_id 由存储生成。子块的 chunk_id、parent_id、doc_id 由存储填写。
	ReplaceSections(ctx context.Context, docID int64, parents []ParentWithChildren) error
}

// Document 是一篇来源。一行对应一个集合里的一个 source_uri。
// Checksum 是清洗后 UTF-8 的小写十六进制 SHA-256。
// 这里没有对象键，也没有原始文件字节。
type Document struct {
	DocID        int64
	CollectionID string
	Version      int
	SourceURI    string
	Checksum     string
	MediaType    string
	License      string
	Title        string
	FetchedAt    time.Time
}

// Parent 是文档里的一节。Body 是这一节的 UTF-8。
type Parent struct {
	ParentID    int64
	DocID       int64
	HeadingPath string
	PageFrom    int
	PageTo      int
	Body        string
}

// Child 是父块里的一个子块。
// ChunkID 的格式是 strconv.FormatInt(docID, 10) + "#" + 序号的十进制文本。
// StartByte 与 EndByte 原样保存，本包不据此截取正文。
type Child struct {
	ChunkID     string
	ParentID    int64
	DocID       int64
	Ordinal     int
	StartByte   int
	EndByte     int
	HeadingPath string
	Page        int
	Body        string
}

// ParentWithChildren 是 ReplaceSections 一次换入的一节和它的子块。
type ParentWithChildren struct {
	Parent   Parent
	Children []Child
}

// ChunkID 返回子块主键。序号使用十进制，不补零。
func ChunkID(docID int64, ordinal int) string {
	return strconv.FormatInt(docID, 10) + "#" + strconv.Itoa(ordinal)
}

func validateDocument(in Document) error {
	if err := checkText("collection_id", in.CollectionID, maxCollectionID, 0); err != nil {
		return err
	}
	if strings.TrimSpace(in.CollectionID) == "" {
		return errors.New("catalog: empty collection_id")
	}
	if strings.ContainsRune(in.CollectionID, 0) {
		return errors.New("catalog: collection_id contains nul")
	}
	if err := checkText("source_uri", in.SourceURI, maxSourceURI, 0); err != nil {
		return err
	}
	if strings.TrimSpace(in.SourceURI) == "" {
		return errors.New("catalog: empty source_uri")
	}
	if strings.ContainsRune(in.SourceURI, 0) {
		return errors.New("catalog: source_uri contains nul")
	}
	if !validChecksum(in.Checksum) {
		return errors.New("catalog: checksum must be 64 lowercase hex characters")
	}
	if err := checkText("media_type", in.MediaType, maxMediaType, 0); err != nil {
		return err
	}
	if err := checkText("license", in.License, maxLicense, 0); err != nil {
		return err
	}
	if err := checkText("title", in.Title, maxTitle, 0); err != nil {
		return err
	}
	if !fitsInt32(in.Version) {
		return errors.New("catalog: version out of range")
	}
	return nil
}

func validateParent(in Parent) error {
	if in.DocID <= 0 {
		return errors.New("catalog: doc_id required")
	}
	return validateParentText(in)
}

func validateParentText(in Parent) error {
	if !fitsInt32(in.PageFrom) || !fitsInt32(in.PageTo) {
		return errors.New("catalog: page out of range")
	}
	if err := checkText("heading_path", in.HeadingPath, 0, maxTextBytes); err != nil {
		return err
	}
	if err := checkText("body", in.Body, 0, maxMediumBytes); err != nil {
		return err
	}
	return nil
}

func validateChild(in Child) error {
	if in.DocID <= 0 {
		return errors.New("catalog: doc_id required")
	}
	if in.ParentID <= 0 {
		return errors.New("catalog: parent_id required")
	}
	return validateChildText(in)
}

func validateChildText(in Child) error {
	if in.Ordinal < 0 {
		return errors.New("catalog: negative ordinal")
	}
	if in.StartByte < 0 || in.EndByte < 0 {
		return errors.New("catalog: negative byte offset")
	}
	if !fitsInt32(in.Ordinal) || !fitsInt32(in.StartByte) || !fitsInt32(in.EndByte) || !fitsInt32(in.Page) {
		return errors.New("catalog: integer out of range")
	}
	if err := checkText("heading_path", in.HeadingPath, 0, maxTextBytes); err != nil {
		return err
	}
	if err := checkText("body", in.Body, 0, maxMediumBytes); err != nil {
		return err
	}
	return nil
}

func validateBatch(docID int64, groups []ParentWithChildren) error {
	if docID <= 0 {
		return errors.New("catalog: doc_id required")
	}
	seen := make(map[int]struct{}, len(groups))
	for _, group := range groups {
		if group.Parent.DocID != 0 && group.Parent.DocID != docID {
			return errors.New("catalog: parent doc_id does not match")
		}
		if err := validateParentText(group.Parent); err != nil {
			return err
		}
		for _, child := range group.Children {
			if child.DocID != 0 && child.DocID != docID {
				return errors.New("catalog: child doc_id does not match")
			}
			if err := validateChildText(child); err != nil {
				return err
			}
			if _, ok := seen[child.Ordinal]; ok {
				return fmt.Errorf("catalog: duplicate ordinal %d", child.Ordinal)
			}
			seen[child.Ordinal] = struct{}{}
		}
	}
	return nil
}

func checkText(name, value string, maxRunes, maxBytes int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("catalog: %s is not utf-8", name)
	}
	if maxRunes > 0 && utf8.RuneCountInString(value) > maxRunes {
		return fmt.Errorf("catalog: %s longer than %d characters", name, maxRunes)
	}
	if maxBytes > 0 && len(value) > maxBytes {
		return fmt.Errorf("catalog: %s longer than %d bytes", name, maxBytes)
	}
	return nil
}

func validChecksum(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || (c > '9' && c < 'a') || c > 'f' {
			return false
		}
	}
	return true
}

func fitsInt32(n int) bool {
	return n >= math.MinInt32 && n <= math.MaxInt32
}

func normalizeFetched(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC().Truncate(time.Microsecond)
}

func sourceKey(collectionID, sourceURI string) string {
	return collectionID + "\x00" + sourceURI
}
