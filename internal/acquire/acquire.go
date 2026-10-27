// Package acquire 定义把外部来源收成原始对象的契约。
// 本包负责列出条目，并把正文流式交给存储。
// 本包不清洗、不切分、不嵌入。数据库、站点地图和厂商接口尚未接通。
package acquire

import (
	"context"
	"errors"
	"io"
	"net/http"

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
	// KindSitemap 是站点地图来源，尚未接通。
	KindSitemap = "sitemap"
	// KindVendorAPI 是厂商接口来源，尚未接通。
	KindVendorAPI = "vendor_api"
)

// ErrNotImplemented 表示来源或收入尚未接通。
var ErrNotImplemented = errors.New("acquire: not implemented")

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

// Result 是一次收入后的原始对象位置，形状与 Ingest 相同。
// Meta 是存储提交成功后的可见对象。
type Result struct {
	Meta           storage.ObjectMeta
	SourceURI      string
	CollectionID   model.CollectionID
	IdempotencyKey model.IdempotencyKey
}

// NewLocalDir 准备根目录 root 下的本地来源。
// 当前返回 ErrNotImplemented，不读取目录。
func NewLocalDir(root string) (Source, error) {
	return nil, ErrNotImplemented
}

// NewHTTP 准备 rawURL 上的 HTTP 来源。client 留给接通后的请求。
// 当前返回 ErrNotImplemented，不拨号。
func NewHTTP(rawURL string, client *http.Client) (Source, error) {
	return nil, ErrNotImplemented
}

// NewDatabase 尚未接通，不是可用实现。
// 调用返回 ErrNotImplemented，不解析 dsn，也不连接数据库。
func NewDatabase(dsn string) (Source, error) {
	return nil, ErrNotImplemented
}

// NewSitemap 尚未接通，不是可用实现。
// 调用返回 ErrNotImplemented，不抓取 rawURL。
func NewSitemap(rawURL string) (Source, error) {
	return nil, ErrNotImplemented
}

// NewVendorAPI 尚未接通，不是可用实现。
// 调用返回 ErrNotImplemented，不访问名为 name 的外部接口。
func NewVendorAPI(name string) (Source, error) {
	return nil, ErrNotImplemented
}

// Ingest 把 body 流式交给 storage.Store。
// 禁止 io.ReadAll。单次 Read 不得超过 storage.CopyBufSize。
// 成功时 Result 的集合与幂等键等于入参，Meta 是存储返回的可见对象。
// 校验和不符时返回 storage.ErrChecksumMismatch，且不产生可见对象。
// 当前返回 ErrNotImplemented，不读取 body。
func Ingest(ctx context.Context, store storage.Store, col model.CollectionID, idem model.IdempotencyKey, filename, contentType string, body io.Reader) (Result, error) {
	return Result{}, ErrNotImplemented
}

// IngestSource 列出 src 的每条条目，并走与 Ingest 相同的结果形状。
// 每条的幂等键是 idemPrefix + ":" + sourceURI。
// 任一条失败时返回该错误。当前返回 ErrNotImplemented，不调用 src。
func IngestSource(ctx context.Context, store storage.Store, col model.CollectionID, src Source, idemPrefix string) ([]Result, error) {
	return nil, ErrNotImplemented
}
