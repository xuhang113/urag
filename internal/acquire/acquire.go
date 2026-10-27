// Package acquire 定义把外部来源收成原始对象的契约。
// 本地目录和 HTTP 地址会列出条目，并把正文流式交给存储。
// 公开收录只列出不登录就能下载的文档，不把正文写入对象存储。
// 本包不清洗、不切分、不嵌入。数据库和厂商接口尚未接通。
package acquire

import (
	"context"
	"errors"
	"io"
	"mime"
	"path/filepath"
	"strings"

	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/storage"
)

const (
	// KindLocalDir 是本地目录来源。
	KindLocalDir = "local_dir"
	// KindHTTP 是 HTTP 地址来源。
	KindHTTP = "http"
	// KindDatabase 是数据库来源，尚未接通。
	KindDatabase = "database"
	// KindSitemap 是站点地图来源。
	// List 解析 urlset 和 sitemapindex，只列出允许的文档 URL。
	KindSitemap = "sitemap"
	// KindVendorAPI 是厂商接口来源，尚未接通。
	KindVendorAPI = "vendor_api"
)

var (
	// ErrNotImplemented 表示来源或收入尚未接通。
	ErrNotImplemented = errors.New("acquire: not implemented")

	// ErrInvalidKey 表示对象键为空，或键里出现 ".."。
	// Ingest 遇到它时不调用 Store.Put。
	ErrInvalidKey = errors.New("acquire: invalid object key")
)

// Item 是一条待收入的原始对象。
// Open 返回正文流，调用方负责关闭。Open 不得把正文一次性读入内存。
type Item struct {
	SourceURI   string
	Filename    string
	ContentType string
	Open        func(ctx context.Context) (io.ReadCloser, error)
}

// Source 是一种原始来源。
// Kind 返回本包的 Kind 常量。List 只列出条目，不清洗正文。
type Source interface {
	Kind() string
	List(ctx context.Context) ([]Item, error)
}

// Result 是一次收入后的原始对象位置。
// Meta 是存储提交成功后的可见对象。
// 成功时 SourceURI、CollectionID、IdempotencyKey 都有值。
// Ingest 写入的 SourceURI 等于 filename。IngestSource 写入的 SourceURI 等于条目的来源地址。
type Result struct {
	Meta           storage.ObjectMeta
	SourceURI      string
	CollectionID   model.CollectionID
	IdempotencyKey model.IdempotencyKey
}

// NewDatabase 尚未接通，不是可用实现。
// 调用返回 ErrNotImplemented，不解析 dsn，也不连接数据库。
func NewDatabase(dsn string) (Source, error) {
	return nil, ErrNotImplemented
}

// NewVendorAPI 尚未接通，不是可用实现。
// 调用返回 ErrNotImplemented，不访问名为 name 的外部接口。
func NewVendorAPI(name string) (Source, error) {
	return nil, ErrNotImplemented
}

// Ingest 把 body 原样交给 store.Put，不调用 io.ReadAll，也不在本地复制全文。
// 本函数自身不读取 body，因此不会以大于 storage.CopyBufSize 的缓冲调用 Read。
// 对象键是集合编号 + "/" + 幂等键。集合或幂等键为空，或键里出现 ".." 时返回 ErrInvalidKey，且不调用 Put。
// PutOptions 带上 contentType、集合和幂等键。
// 成功时 Result.Meta 是存储返回的可见对象，CollectionID 与 IdempotencyKey 等于入参，SourceURI 等于 filename。
// store 或 body 为 nil 时返回错误。store 的错误原样返回。
// storage.ErrChecksumMismatch 表示校验和不符，本函数不另存正文。
func Ingest(ctx context.Context, store storage.Store, col model.CollectionID, idem model.IdempotencyKey, filename, contentType string, body io.Reader) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if store == nil {
		return Result{}, errors.New("acquire: nil store")
	}
	if body == nil {
		return Result{}, errors.New("acquire: nil body")
	}
	key, err := objectKey(col, idem)
	if err != nil {
		return Result{}, err
	}
	meta, err := store.Put(ctx, key, body, storage.PutOptions{
		ContentType:    contentType,
		CollectionID:   col,
		IdempotencyKey: idem,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{
		Meta:           meta,
		SourceURI:      filename,
		CollectionID:   col,
		IdempotencyKey: idem,
	}, nil
}

// IngestSource 列出 src 的每条条目，并对每条走与 Ingest 相同的收入路径。
// 幂等键是 idemPrefix + ":" + 该条 SourceURI。成功时 Result.SourceURI 是条目的来源地址。
// 任一条列出、打开、收入或关闭失败时返回该错误，不返回已经收入的部分结果。
// src 或 store 为 nil 时返回错误。来源返回的错误原样返回。
func IngestSource(ctx context.Context, store storage.Store, col model.CollectionID, src Source, idemPrefix string) ([]Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("acquire: nil store")
	}
	if src == nil {
		return nil, errors.New("acquire: nil source")
	}
	items, err := src.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(items))
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		idem := model.IdempotencyKey(idemPrefix + ":" + item.SourceURI)
		res, err := ingestItem(ctx, store, col, idem, item)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

func ingestItem(ctx context.Context, store storage.Store, col model.CollectionID, idem model.IdempotencyKey, item Item) (res Result, err error) {
	if item.Open == nil {
		return Result{}, errors.New("acquire: nil opener")
	}
	body, err := item.Open(ctx)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		cerr := body.Close()
		if err == nil && cerr != nil {
			err = cerr
		}
	}()
	res, err = Ingest(ctx, store, col, idem, item.Filename, item.ContentType, body)
	if err != nil {
		return Result{}, err
	}
	res.SourceURI = item.SourceURI
	return res, nil
}

func objectKey(col model.CollectionID, idem model.IdempotencyKey) (model.ObjectKey, error) {
	if col == "" || idem == "" {
		return "", ErrInvalidKey
	}
	raw := string(col) + "/" + string(idem)
	if strings.Contains(raw, "..") {
		return "", ErrInvalidKey
	}
	return model.ObjectKey(raw), nil
}

func contentTypeOf(name string) string {
	ext := filepath.Ext(name)
	if ext == "" {
		return "application/octet-stream"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}
