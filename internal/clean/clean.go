// Package clean 定义从原始对象得到 UTF-8 正文和血缘的契约。
// 本包负责识别类型、转码、去掉样板，以及在集合内去重。
// 本包不切分、不嵌入、不建索引。
package clean

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/xuhang113/urag/internal/model"
)

var (
	// ErrUnsupportedType 表示媒体类型不是 txt、md、html 或 pdf。
	ErrUnsupportedType = errors.New("clean: unsupported type")

	// ErrNotImplemented 保留给旧调用方识别未接通的桩。接通后的 Clean 不再返回它。
	ErrNotImplemented = errors.New("clean: not implemented")

	errBindingExists = errors.New("clean: dedupe binding exists")
)

// Input 是一篇待清洗的原始对象。
// Body 提供原始字节。调用方不必预先把正文转成 UTF-8。
type Input struct {
	CollectionID model.CollectionID
	ObjectKey    model.ObjectKey
	SourceURI    string
	Filename     string
	Body         io.Reader
}

// Deduper 在一个集合内按正文哈希记住文档。
// Find 未命中时返回空编号、false 和 nil。不得用其他集合的文档回答这次查找。
// Put 把 contentHash 绑定到该集合的 id。写入失败时返回错误，不改已有绑定。
type Deduper interface {
	Find(ctx context.Context, col model.CollectionID, contentHash string) (model.DocumentID, bool, error)
	Put(ctx context.Context, col model.CollectionID, contentHash string, id model.DocumentID) error
}

// Cleaner 把原始对象变成一篇文档。
type Cleaner interface {
	// Clean 支持 txt、md、html、pdf。识别编码并输出 UTF-8，Document.Text 即这份正文。
	// 去掉 nav、header、footer 和重复模板。
	// 按正文哈希在集合内去重。重复时 Document.Duplicate 为 true，
	// Lineage.DuplicateOf 指向已有文档，这篇不进入索引。
	// Lineage.Steps 按顺序保留每一步，Name 取 detect、strip、dedupe。
	// Lineage.RawObjectKey 等于输入的对象键。
	// 媒体类型不能识别时返回 ErrUnsupportedType。
	Clean(ctx context.Context, in Input) (model.Document, error)
}

type cleaner struct {
	dedup Deduper
}

// New 返回使用 dedup 做集合内去重的清洗器。dedup 可以为空，空时不去重。
func New(dedup Deduper) Cleaner {
	return cleaner{dedup: dedup}
}

func (c cleaner) Clean(ctx context.Context, in Input) (model.Document, error) {
	if err := ctx.Err(); err != nil {
		return model.Document{}, err
	}
	if in.Body == nil {
		return model.Document{}, errors.New("clean: nil body")
	}
	raw, err := io.ReadAll(in.Body)
	if err != nil {
		return model.Document{}, fmt.Errorf("clean: read body: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return model.Document{}, err
	}

	media, err := detectMedia(in.Filename, raw)
	if err != nil {
		return model.Document{}, err
	}
	var text, enc string
	if media == "pdf" {
		text, enc, err = extractPDF(raw)
	} else {
		text, enc, err = decodeText(raw)
	}
	if err != nil {
		return model.Document{}, err
	}

	stripDetail := fmt.Sprintf("kept bytes=%d", len(text))
	if media == "html" {
		text, stripDetail, err = stripHTML(text)
		if err != nil {
			return model.Document{}, err
		}
	}

	sum := sha256.Sum256([]byte(text))
	hash := hex.EncodeToString(sum[:])
	id := model.DocumentID(string(in.CollectionID) + ":" + hash[:16])

	duplicate := false
	var dupOf model.DocumentID
	if c.dedup != nil {
		found, ok, findErr := c.dedup.Find(ctx, in.CollectionID, hash)
		if findErr != nil {
			return model.Document{}, findErr
		}
		if ok {
			duplicate = true
			dupOf = found
		} else if putErr := c.dedup.Put(ctx, in.CollectionID, hash, id); putErr != nil {
			return model.Document{}, putErr
		}
	}
	dedupeDetail := fmt.Sprintf("hash=%s duplicate=false", hash)
	if duplicate {
		dedupeDetail = fmt.Sprintf("hash=%s duplicate=%s", hash, dupOf)
	}

	return model.Document{
		ID:           id,
		CollectionID: in.CollectionID,
		Text:         text,
		Duplicate:    duplicate,
		Lineage: model.Lineage{
			RawObjectKey: in.ObjectKey,
			DocumentID:   id,
			CollectionID: in.CollectionID,
			ContentHash:  hash,
			Steps: []model.CleanStep{
				{Name: "detect", Detail: fmt.Sprintf("media=%s encoding=%s bytes=%d", media, enc, len(raw))},
				{Name: "strip", Detail: stripDetail},
				{Name: "dedupe", Detail: dedupeDetail},
			},
			DuplicateOf: dupOf,
			Encoding:    enc,
			MediaType:   media,
			SourceURI:   in.SourceURI,
		},
	}, nil
}
