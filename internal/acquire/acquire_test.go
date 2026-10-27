package acquire_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xuhang113/urag/internal/acquire"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/storage"
)

type lineageCtxKey struct{}

func TestLocalAndHTTPSameLineage(t *testing.T) {
	if _, err := acquire.NewDatabase("postgres://127.0.0.1/db"); !errors.Is(err, acquire.ErrNotImplemented) {
		t.Fatalf("数据库来源：%v", err)
	}
	sitemapSrc, err := acquire.NewSitemap("https://127.0.0.1/sitemap.xml")
	if err != nil {
		t.Fatalf("站点地图来源：%v", err)
	}
	if sitemapSrc.Kind() != acquire.KindSitemap {
		t.Fatalf("站点地图 Kind = %q", sitemapSrc.Kind())
	}
	if _, err := acquire.NewVendorAPI("acme"); !errors.Is(err, acquire.ErrNotImplemented) {
		t.Fatalf("厂商接口来源：%v", err)
	}
	if _, err := acquire.NewHTTP("ftp://127.0.0.1/a", nil); err == nil {
		t.Fatal("ftp 不应被接受")
	}
	if _, err := acquire.NewHTTP("file:///tmp/a", nil); err == nil {
		t.Fatal("file 不应被接受")
	}

	blocked := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Error("列出 HTTPS 条目时发生了请求")
		return nil, errors.New("acquire test: list must not dial")
	})}
	httpsSrc, err := acquire.NewHTTP("https://127.0.0.1/a.txt", blocked)
	if err != nil {
		t.Fatalf("https 来源：%v", err)
	}
	if httpsSrc.Kind() != acquire.KindHTTP {
		t.Fatalf("https Kind = %q", httpsSrc.Kind())
	}
	httpsItems, err := httpsSrc.List(context.Background())
	if err != nil {
		t.Fatalf("列出 https：%v", err)
	}
	if len(httpsItems) != 1 || httpsItems[0].Filename != "a.txt" || httpsItems[0].SourceURI != "https://127.0.0.1/a.txt" {
		t.Fatalf("https 条目 = %+v", httpsItems)
	}

	payload := []byte("same-lineage-body\n")
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "skip.txt"), []byte("skip"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	local, err := acquire.NewLocalDir(dir)
	if err != nil {
		t.Fatalf("本地目录：%v", err)
	}
	if local.Kind() != acquire.KindLocalDir {
		t.Fatalf("本地 Kind = %q", local.Kind())
	}

	items, err := local.List(context.Background())
	if err != nil {
		t.Fatalf("列出本地目录：%v", err)
	}
	if len(items) != 1 {
		t.Fatalf("本地条目数量 = %d，应跳过目录", len(items))
	}
	if items[0].SourceURI != path || items[0].Filename != "note.txt" {
		t.Fatalf("本地条目 = %+v，路径 %s", items[0], path)
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := items[0].Open(cancelCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后的本地 Open = %v", err)
	}

	var mu sync.Mutex
	dials := 0
	sawCtx := false
	method := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		method = r.Method
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	base := srv.Client().Transport
	client := srv.Client()
	client.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		dials++
		if r.Context().Value(lineageCtxKey{}) == "yes" {
			sawCtx = true
		}
		mu.Unlock()
		ip := net.ParseIP(r.URL.Hostname())
		if ip == nil || !ip.IsLoopback() {
			t.Errorf("请求了回环以外的地址 %s", r.URL.Host)
			return nil, errors.New("acquire test: non-loopback")
		}
		return base.RoundTrip(r)
	})

	rawURL := srv.URL + "/doc.txt"
	remote, err := acquire.NewHTTP(rawURL, client)
	if err != nil {
		t.Fatalf("http 来源：%v", err)
	}
	if remote.Kind() != acquire.KindHTTP {
		t.Fatalf("http Kind = %q", remote.Kind())
	}
	if local.Kind() == remote.Kind() {
		t.Fatal("本地与 HTTP 的 Kind 不应相同")
	}

	httpItems, err := remote.List(context.Background())
	if err != nil {
		t.Fatalf("列出 http：%v", err)
	}
	mu.Lock()
	listedDials := dials
	mu.Unlock()
	if listedDials != 0 {
		t.Fatalf("List 拨号 %d 次", listedDials)
	}
	if len(httpItems) != 1 || httpItems[0].SourceURI != rawURL || httpItems[0].Filename != "doc.txt" {
		t.Fatalf("http 条目 = %+v", httpItems)
	}
	httpCancel, cancelHTTP := context.WithCancel(context.Background())
	cancelHTTP()
	if _, err := httpItems[0].Open(httpCancel); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后的 HTTP Open = %v", err)
	}

	col := model.CollectionID("docs")
	prefix := "raw"
	store := newMemStore()
	ctx := context.WithValue(context.Background(), lineageCtxKey{}, "yes")
	localRes, err := acquire.IngestSource(ctx, store, col, local, prefix)
	if err != nil {
		t.Fatalf("收入本地目录：%v", err)
	}
	httpRes, err := acquire.IngestSource(ctx, store, col, remote, prefix)
	if err != nil {
		t.Fatalf("收入 http：%v", err)
	}
	if len(localRes) != 1 || len(httpRes) != 1 {
		t.Fatalf("结果数量 local=%d http=%d", len(localRes), len(httpRes))
	}

	mu.Lock()
	gotDials, gotMethod, gotCtx := dials, method, sawCtx
	mu.Unlock()
	if gotDials < 1 || gotMethod != http.MethodGet || !gotCtx {
		t.Fatalf("GET 未带上 context：dials=%d method=%s ctx=%v", gotDials, gotMethod, gotCtx)
	}

	wantSum := checksumOf(payload)
	if localRes[0].Meta.Checksum != httpRes[0].Meta.Checksum || localRes[0].Meta.Checksum != wantSum {
		t.Fatalf("校验和 local=%q http=%q want=%q", localRes[0].Meta.Checksum, httpRes[0].Meta.Checksum, wantSum)
	}
	if localRes[0].CollectionID != col || httpRes[0].CollectionID != col {
		t.Fatalf("集合 local=%q http=%q", localRes[0].CollectionID, httpRes[0].CollectionID)
	}
	assertLineage(t, localRes[0], col, prefix, path, int64(len(payload)))
	assertLineage(t, httpRes[0], col, prefix, rawURL, int64(len(payload)))
	if !strings.Contains(localRes[0].Meta.ContentType, "text/plain") || !strings.Contains(httpRes[0].Meta.ContentType, "text/plain") {
		t.Fatalf("内容类型 local=%q http=%q", localRes[0].Meta.ContentType, httpRes[0].Meta.ContentType)
	}
	assertStored(t, store, localRes[0].Meta.Key, payload)
	assertStored(t, store, httpRes[0].Meta.Key, payload)

	nilSrc, err := acquire.NewHTTP(rawURL, nil)
	if err != nil {
		t.Fatalf("nil client：%v", err)
	}
	nilItems, err := nilSrc.List(context.Background())
	if err != nil {
		t.Fatalf("nil client List：%v", err)
	}
	body, err := nilItems[0].Open(context.Background())
	if err != nil {
		t.Fatalf("nil client Open：%v", err)
	}
	defer body.Close()
	var got bytes.Buffer
	if _, err := io.CopyBuffer(&got, body, make([]byte, storage.CopyBufSize)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatal("nil client 读到的正文与原文不同")
	}
}

func TestUploadBoundedReader(t *testing.T) {
	// 三倍 CopyBufSize 长于单次上限。当前 io.ReadAll 读到这个长度时会申请更大的缓冲。
	payload := bytes.Repeat([]byte{'q'}, 3*storage.CopyBufSize)
	if len(payload) <= storage.CopyBufSize {
		t.Fatalf("正文长度 %d 没有超过 CopyBufSize", len(payload))
	}
	probe := &boundedReader{r: bytes.NewReader(payload), limit: storage.CopyBufSize}
	if _, err := io.ReadAll(probe); err == nil {
		t.Fatal("这组正文没有让 io.ReadAll 触发超限读取，无法证明 Ingest 没有先读全")
	}

	body := &boundedReader{r: bytes.NewReader(payload), limit: storage.CopyBufSize}
	store := newMemStore()
	col := model.CollectionID("docs")
	idem := model.IdempotencyKey("wide-1")
	res, err := acquire.Ingest(context.Background(), store, col, idem, "wide.bin", "application/octet-stream", body)
	if err != nil {
		t.Fatalf("Ingest：%v", err)
	}
	if body.max == 0 || body.max > storage.CopyBufSize {
		t.Fatalf("单次 Read 最大值 = %d", body.max)
	}
	if store.lastReader != body {
		t.Fatal("Ingest 没有把原来的 reader 交给 Store")
	}
	if res.SourceURI != "wide.bin" || res.CollectionID != col || res.IdempotencyKey != idem {
		t.Fatalf("Result = %+v", res)
	}
	wantKey := model.ObjectKey(string(col) + "/" + string(idem))
	if res.Meta.Key != wantKey || res.Meta.CollectionID != col || res.Meta.IdempotencyKey != idem {
		t.Fatalf("Meta = %+v", res.Meta)
	}
	if res.Meta.Size != int64(len(payload)) || res.Meta.Checksum != checksumOf(payload) {
		t.Fatalf("大小或校验和：size=%d sum=%s", res.Meta.Size, res.Meta.Checksum)
	}
	if res.Meta.ContentType != "application/octet-stream" {
		t.Fatalf("内容类型 = %q", res.Meta.ContentType)
	}
	assertStored(t, store, wantKey, payload)

	poison := newMemStore()
	_, err = acquire.Ingest(context.Background(), poison, "../docs", "ok", "f.txt", "text/plain", strings.NewReader("x"))
	if !errors.Is(err, acquire.ErrInvalidKey) {
		t.Fatalf("集合里的 .. ：%v", err)
	}
	_, err = acquire.Ingest(context.Background(), poison, "docs", "a/../../secret", "f.txt", "text/plain", strings.NewReader("x"))
	if !errors.Is(err, acquire.ErrInvalidKey) {
		t.Fatalf("幂等键里的 .. ：%v", err)
	}
	if poison.puts != 0 {
		t.Fatalf("非法键调用了 Put %d 次", poison.puts)
	}
}

func assertLineage(t *testing.T, res acquire.Result, col model.CollectionID, prefix, sourceURI string, size int64) {
	t.Helper()
	if res.Meta.Key == "" || res.Meta.Checksum == "" || res.SourceURI == "" || res.CollectionID == "" || res.IdempotencyKey == "" {
		t.Fatalf("字段有空值：%+v", res)
	}
	if res.Meta.Size != size || res.Meta.CollectionID != col || res.CollectionID != col {
		t.Fatalf("元数据与集合不符：%+v", res)
	}
	wantIdem := model.IdempotencyKey(prefix + ":" + sourceURI)
	if res.IdempotencyKey != wantIdem || res.Meta.IdempotencyKey != wantIdem || res.SourceURI != sourceURI {
		t.Fatalf("血缘字段 = %+v，来源 %s", res, sourceURI)
	}
	wantKey := model.ObjectKey(string(col) + "/" + string(wantIdem))
	if res.Meta.Key != wantKey {
		t.Fatalf("对象键 = %q，应为 %q", res.Meta.Key, wantKey)
	}
}

func assertStored(t *testing.T, store *memStore, key model.ObjectKey, payload []byte) {
	t.Helper()
	got, ok := store.data(key)
	if !ok {
		t.Fatalf("存储里没有 %s", key)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("存储字节与原文不同，长度 %d / %d", len(got), len(payload))
	}
}

func checksumOf(b []byte) model.Checksum {
	sum := sha256.Sum256(b)
	return model.Checksum(hex.EncodeToString(sum[:]))
}

// boundedReader 在单次 Read 的缓冲大于 limit 时失败。
type boundedReader struct {
	r     io.Reader
	limit int
	max   int
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if len(p) > b.max {
		b.max = len(p)
	}
	if len(p) > b.limit {
		return 0, errors.New("acquire test: read exceeds limit")
	}
	return b.r.Read(p)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// limitedWriter 只提供 Write，避免 io.CopyBuffer 改走 ReaderFrom。
type limitedWriter struct {
	buf bytes.Buffer
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	return w.buf.Write(p)
}

// memStore 是测试里的内存 storage.Store。Put 只用不超过 CopyBufSize 的缓冲复制。
type memStore struct {
	mu         sync.Mutex
	objs       map[model.ObjectKey]memObject
	byIdem     map[model.IdempotencyKey]model.ObjectKey
	puts       int
	lastReader io.Reader
}

type memObject struct {
	meta storage.ObjectMeta
	data []byte
}

func newMemStore() *memStore {
	return &memStore{
		objs:   map[model.ObjectKey]memObject{},
		byIdem: map[model.IdempotencyKey]model.ObjectKey{},
	}
}

func (m *memStore) Put(ctx context.Context, key model.ObjectKey, r io.Reader, opts storage.PutOptions) (storage.ObjectMeta, error) {
	m.mu.Lock()
	m.puts++
	m.lastReader = r
	m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return storage.ObjectMeta{}, err
	}
	if r == nil {
		return storage.ObjectMeta{}, errors.New("acquire test: nil reader")
	}
	buf := make([]byte, storage.CopyBufSize)
	// bytes.Buffer 实现了 ReaderFrom，io.CopyBuffer 会忽略传入缓冲。
	// 只实现 Write 的写入器才会按 CopyBufSize 逐次 Read。
	var dst limitedWriter
	if _, err := io.CopyBuffer(&dst, r, buf); err != nil {
		return storage.ObjectMeta{}, err
	}
	data := append([]byte(nil), dst.buf.Bytes()...)
	sum := sha256.Sum256(data)
	checksum := model.Checksum(hex.EncodeToString(sum[:]))
	if opts.ExpectedChecksum != "" && opts.ExpectedChecksum != checksum {
		return storage.ObjectMeta{}, storage.ErrChecksumMismatch
	}
	meta := storage.ObjectMeta{
		Key:            key,
		Size:           int64(len(data)),
		Checksum:       checksum,
		ContentType:    opts.ContentType,
		CollectionID:   opts.CollectionID,
		IdempotencyKey: opts.IdempotencyKey,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if opts.IdempotencyKey != "" {
		if prev, ok := m.byIdem[opts.IdempotencyKey]; ok {
			return m.objs[prev].meta, nil
		}
		m.byIdem[opts.IdempotencyKey] = key
	}
	m.objs[key] = memObject{meta: meta, data: data}
	return meta, nil
}

func (m *memStore) data(key model.ObjectKey) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objs[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), obj.data...), true
}

func (m *memStore) Get(ctx context.Context, key model.ObjectKey) (io.ReadCloser, storage.ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, storage.ObjectMeta{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objs[key]
	if !ok {
		return nil, storage.ObjectMeta{}, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), obj.data...))), obj.meta, nil
}

func (m *memStore) Delete(ctx context.Context, key model.ObjectKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[key]; !ok {
		return storage.ErrNotFound
	}
	delete(m.objs, key)
	return nil
}

func (m *memStore) Stat(ctx context.Context, key model.ObjectKey) (storage.ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return storage.ObjectMeta{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objs[key]
	if !ok {
		return storage.ObjectMeta{}, storage.ErrNotFound
	}
	return obj.meta, nil
}

func (m *memStore) ExistsByChecksum(ctx context.Context, checksum model.Checksum) (storage.ObjectMeta, bool, error) {
	if err := ctx.Err(); err != nil {
		return storage.ObjectMeta{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, obj := range m.objs {
		if obj.meta.Checksum == checksum {
			return obj.meta, true, nil
		}
	}
	return storage.ObjectMeta{}, false, nil
}

func (m *memStore) CreateMultipart(context.Context, model.ObjectKey, storage.PutOptions) (storage.MultipartSession, error) {
	return storage.MultipartSession{}, storage.ErrNotImplemented
}

func (m *memStore) UploadPart(context.Context, storage.MultipartSession, int, io.Reader) (storage.Part, error) {
	return storage.Part{}, storage.ErrNotImplemented
}

func (m *memStore) ListParts(context.Context, storage.MultipartSession) ([]storage.Part, error) {
	return nil, storage.ErrNotImplemented
}

func (m *memStore) CompleteMultipart(context.Context, storage.MultipartSession, []storage.Part) (storage.ObjectMeta, error) {
	return storage.ObjectMeta{}, storage.ErrNotImplemented
}

func (m *memStore) AbortMultipart(context.Context, storage.MultipartSession) error {
	return storage.ErrNotImplemented
}

var _ storage.Store = (*memStore)(nil)
