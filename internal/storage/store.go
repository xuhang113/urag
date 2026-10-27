// Package storage 定义原始对象的流式存放契约。
// 本包负责校验和、幂等键、分片和有限次退避。
// 本包不解析正文，不清洗，不建索引，也不导出云厂商客户端类型。
package storage

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/xuhang113/urag/internal/model"
)

// CopyBufSize 是单次复制缓冲的上限。
// 读写都不得使用更大的缓冲把整个对象载入内存。
const CopyBufSize = 32 << 10

var (
	// ErrNotFound 表示对象或分片会话不存在。
	ErrNotFound = errors.New("storage: not found")

	// ErrChecksumMismatch 表示内容与期望校验和不一致。
	// 遇到它不得重试，对象必须保持不可见。
	ErrChecksumMismatch = errors.New("storage: checksum mismatch")

	// ErrRetryable 表示这次失败可以按 RetryPolicy 退避后重试。
	ErrRetryable = errors.New("storage: retryable")

	// ErrNotImplemented 表示存储尚未接通。
	ErrNotImplemented = errors.New("storage: not implemented")
)

// RetryPolicy 限制可重试错误的次数和退避。
// MaxAttempts 包含第一次调用。BaseDelay 是首次退避，MaxDelay 是退避上限。
// ErrChecksumMismatch、context.Canceled、context.DeadlineExceeded 不重试。
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// PutOptions 控制一次写入。
// ExpectedChecksum 为空时只计算校验和。
// 非空且与正文不符时返回 ErrChecksumMismatch，不重试，对象不可见。
type PutOptions struct {
	ContentType      string
	CollectionID     model.CollectionID
	IdempotencyKey   model.IdempotencyKey
	ExpectedChecksum model.Checksum
}

// ObjectMeta 是已提交并且可见的对象元数据。
// 只有校验和与大小跟正文一致时，对象才会以这份元数据可见。
type ObjectMeta struct {
	Key            model.ObjectKey
	Size           int64
	Checksum       model.Checksum
	ContentType    string
	CollectionID   model.CollectionID
	IdempotencyKey model.IdempotencyKey
}

// MultipartSession 是一次尚未提交的分片上传。
// 提交成功前，对应对象不可见。
type MultipartSession struct {
	UploadID       string
	Key            model.ObjectKey
	CollectionID   model.CollectionID
	IdempotencyKey model.IdempotencyKey
}

// Part 是一个已经上传完成的分片。
type Part struct {
	PartNumber int
	Size       int64
	Checksum   model.Checksum
	ETag       string
}

// Hook 观察元数据提交。
// OnCommit 的 attempt 从 1 开始。返回错误则这次提交失败，对象保持不可见。
// hook 为空时照常提交。
type Hook interface {
	OnCommit(attempt int) error
}

// Store 按对象键存放原文。
//
// 读写都是流式的，不把整个对象读进内存，复制缓冲不超过 CopyBufSize。
// 写入时计算 SHA-256。元数据在校验和与大小一致后才提交。
// 先写临时键，提交成功后对象才可见。
// 同一个非空幂等键的第二次调用返回第一次的 ObjectMeta，不产生第二份对象。
// 可重试错误按 RetryPolicy 做有限次退避。
// ErrChecksumMismatch、context.Canceled、context.DeadlineExceeded 不重试。
// 分片中断后 ListParts 仍能看到已完成分片，调用方只补缺失分片。
type Store interface {
	// Put 流式写入一个对象。对 r 的单次读取不超过 CopyBufSize。
	// 成功返回已可见对象的元数据。失败时对象不可见。
	Put(ctx context.Context, key model.ObjectKey, r io.Reader, opts PutOptions) (ObjectMeta, error)

	// Get 流式打开已可见对象。调用方必须关闭 ReadCloser。
	// 对象不存在时返回 ErrNotFound。
	Get(ctx context.Context, key model.ObjectKey) (io.ReadCloser, ObjectMeta, error)

	// Delete 删除已可见对象。对象不存在时返回 ErrNotFound。
	Delete(ctx context.Context, key model.ObjectKey) error

	// Stat 返回已可见对象的元数据。对象不存在时返回 ErrNotFound。
	Stat(ctx context.Context, key model.ObjectKey) (ObjectMeta, error)

	// ExistsByChecksum 在已可见对象中按校验和查找。
	// 未找到时返回零值、false 和 nil。存储失败时返回错误。
	ExistsByChecksum(ctx context.Context, checksum model.Checksum) (ObjectMeta, bool, error)

	// CreateMultipart 开始一次分片上传。会话提交前对象不可见。
	CreateMultipart(ctx context.Context, key model.ObjectKey, opts PutOptions) (MultipartSession, error)

	// UploadPart 上传一个分片。对 r 的单次读取不超过 CopyBufSize。
	// 成功后该分片可被 ListParts 看到。
	UploadPart(ctx context.Context, session MultipartSession, partNumber int, r io.Reader) (Part, error)

	// ListParts 返回会话里已经完成的分片。
	// 中断之后已完成的分片仍然可见，调用方据此只补缺失分片。
	ListParts(ctx context.Context, session MultipartSession) ([]Part, error)

	// CompleteMultipart 在分片校验和与大小一致后提交对象。
	// 成功后对象可见。失败时对象保持不可见，已完成分片仍可列出。
	CompleteMultipart(ctx context.Context, session MultipartSession, parts []Part) (ObjectMeta, error)

	// AbortMultipart 丢弃会话。之后对象不可见。
	// 会话不存在时返回 ErrNotFound。
	AbortMultipart(ctx context.Context, session MultipartSession) error
}

// Local 是本地目录存储的具体类型。
// 写入尚未接通。
type Local struct{}

// CommitAttempts 返回已经提交成功的次数。桩固定返回 0。
func (Local) CommitAttempts() int { return 0 }

// NewLocal 准备目录 dir 上的本地存储。
// policy 控制可重试错误的退避。hook 观察提交，可以为空。
// 当前返回 ErrNotImplemented，不创建目录，也不写入文件。
func NewLocal(dir string, policy RetryPolicy, hook Hook) (Store, error) {
	return nil, ErrNotImplemented
}

// S3Config 是通用对象存储的连接参数。
// AccessKey 与 SecretKey 只来自调用方或环境变量，禁止写入日志和仓库。
type S3Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	PathStyle bool
	PartSize  int64
	Retry     RetryPolicy
}

// ConfigFromEnv 用 getenv 组装 S3Config，不创建客户端。
// 环境变量名为 URAG_S3_ENDPOINT、URAG_S3_REGION、URAG_S3_BUCKET、
// URAG_S3_ACCESS_KEY、URAG_S3_SECRET_KEY、URAG_S3_PATH_STYLE。
// PATH_STYLE 为 1、true、TRUE 或 True 时 PathStyle 为 true，其他值为 false。
// PartSize 与 Retry 不从环境变量读取，保持零值。
// getenv 为 nil 时返回错误。缺字段时仍返回已读到的值。
func ConfigFromEnv(getenv func(string) string) (S3Config, error) {
	if getenv == nil {
		return S3Config{}, errors.New("storage: getenv is nil")
	}
	return S3Config{
		Endpoint:  getenv("URAG_S3_ENDPOINT"),
		Region:    getenv("URAG_S3_REGION"),
		Bucket:    getenv("URAG_S3_BUCKET"),
		AccessKey: getenv("URAG_S3_ACCESS_KEY"),
		SecretKey: getenv("URAG_S3_SECRET_KEY"),
		PathStyle: s3PathStyle(getenv("URAG_S3_PATH_STYLE")),
	}, nil
}

func s3PathStyle(v string) bool {
	switch v {
	case "1", "true", "TRUE", "True":
		return true
	default:
		return false
	}
}

// NewS3 按 cfg 准备对象存储。
// 当前返回 ErrNotImplemented，不连接端点，也不伪造可用客户端。
func NewS3(cfg S3Config) (Store, error) {
	return nil, ErrNotImplemented
}
