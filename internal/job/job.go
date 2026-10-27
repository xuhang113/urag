// Package job 编排上传、抓取、清洗、嵌入、检索和评价任务。
// 接受上传时只把正文流式写入存储并入队。清洗和嵌入在任务执行时进行。
// 本包不提供 HTTP 页面。
//
// 校验和不符不重试。任务取消后不再重试。其他失败最多尝试 3 次。
// Checkpoint 记下已完成的对象键，恢复时跳过这些键。
package job

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/xuhang113/urag/internal/chunk"
	"github.com/xuhang113/urag/internal/clean"
	"github.com/xuhang113/urag/internal/embed"
	"github.com/xuhang113/urag/internal/eval"
	"github.com/xuhang113/urag/internal/index"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
	"github.com/xuhang113/urag/internal/storage"
)

const (
	// StateQueued 表示任务已入队，尚未运行。
	StateQueued = "queued"
	// StateRunning 表示任务正在运行。
	StateRunning = "running"
	// StateSucceeded 表示任务已经成功。
	StateSucceeded = "succeeded"
	// StateFailed 表示任务失败，且不会再自动重试。
	StateFailed = "failed"
	// StateCanceled 表示任务已取消，之后不再重试。
	StateCanceled = "canceled"

	// KindUpload 是上传任务。
	KindUpload = "upload"
	// KindFetch 是抓取任务。
	KindFetch = "fetch"
	// KindEval 是评价任务。
	KindEval = "eval"
)

// ErrNotImplemented 表示任务管道尚未接通。
var ErrNotImplemented = errors.New("job: not implemented")

// Progress 是任务进度。Done 不超过 Total。Stage 是当前阶段名。
type Progress struct {
	Done  int    `json:"done"`
	Total int    `json:"total"`
	Stage string `json:"stage"`
}

// Job 是一次异步任务的公开状态。
// State 只取 queued、running、succeeded、failed、canceled。
// Kind 只取 upload、fetch、eval。
// 校验和不符不增加后续重试。取消后 State 为 canceled，且不再重试。
// Checkpoint 记下已完成的对象键，恢复时跳过这些键。
type Job struct {
	ID           string          `json:"job_id"`
	Kind         string          `json:"kind"`
	State        string          `json:"state"`
	CollectionID string          `json:"collection_id"`
	Progress     Progress        `json:"progress"`
	ObjectKey    string          `json:"object_key,omitempty"`
	Checksum     string          `json:"checksum,omitempty"`
	Size         int64           `json:"size,omitempty"`
	Error        string          `json:"error,omitempty"`
	Attempts     int             `json:"attempts"`
	Checkpoint   json.RawMessage `json:"checkpoint,omitempty"`
}

// UploadInput 是一次上传。Body 只能流式读取，禁止先读进内存。
type UploadInput struct {
	CollectionID   model.CollectionID
	IdempotencyKey model.IdempotencyKey
	Filename       string
	ContentType    string
	Body           io.Reader
}

// Accepted 是上传入队后立刻返回给调用方的结果。
// 成功时带有任务、对象键、校验和和大小。清洗与嵌入尚未在这次调用里执行。
type Accepted struct {
	Job       Job
	ObjectKey model.ObjectKey
	Checksum  model.Checksum
	Size      int64
}

// FetchInput 是一次抓取请求。Kind 取 acquire 的来源种类。
type FetchInput struct {
	CollectionID      model.CollectionID `json:"collection_id"`
	Kind              string             `json:"kind"`
	URI               string             `json:"uri"`
	IdempotencyPrefix string             `json:"idempotency_prefix"`
}

// Limits 是各阶段上限。
// Upload 是单次上传的字节上限。Clean 是清洗并发上限。
// EmbedBatch 与 EmbedConcurrency 是嵌入的批大小和并发。
type Limits struct {
	Upload           int64
	Clean            int
	EmbedBatch       int
	EmbedConcurrency int
}

// Config 是进程组装参数。
// Storage 只取 local 或 s3。FixturePath 是召回夹具路径。
type Config struct {
	Addr        string
	DataDir     string
	Storage     string
	Retry       storage.RetryPolicy
	Weights     retrieve.Weights
	Chunk       chunk.Options
	Limits      Limits
	FixturePath string
}

// Deps 是管道使用的协作件。尚未接通的依赖可以为空。
type Deps struct {
	Store    storage.Store
	Embedder embed.Embedder
	Registry index.Registry
	Reranker retrieve.Reranker
	Splitter chunk.Splitter
	Cleaner  clean.Cleaner
}

// Pipeline 是上传、抓取、检索和评价的任务入口。
// 当前各方法返回 ErrNotImplemented，不读取正文，也不访问依赖。
type Pipeline struct{}

// New 组装管道。当前不拨号、不打开存储、不读取夹具。
// 成功时返回非空管道。方法调用返回 ErrNotImplemented。
func New(cfg Config, deps Deps) (*Pipeline, error) {
	return &Pipeline{}, nil
}

// AcceptUpload 只把 Body 流式写入存储并入队。
// 调用方立刻得到任务编号、对象键、校验和和大小。清洗和嵌入在任务里做。
// 单次读取不得超过 storage.CopyBufSize。禁止把 Body 读进内存。
// 校验和不符时返回 storage.ErrChecksumMismatch，并且不重试。
// 当前返回 ErrNotImplemented，不读取 Body。
func (p *Pipeline) AcceptUpload(ctx context.Context, in UploadInput) (Accepted, error) {
	return Accepted{}, ErrNotImplemented
}

// SubmitFetch 按 Kind 把 URI 的抓取任务入队。
// 成功时返回已入队的 Job。当前返回 ErrNotImplemented。
func (p *Pipeline) SubmitFetch(ctx context.Context, in FetchInput) (Job, error) {
	return Job{}, ErrNotImplemented
}

// GetJob 返回编号为 id 的任务。任务不存在时返回错误。
// 当前返回 ErrNotImplemented。
func (p *Pipeline) GetJob(ctx context.Context, id string) (Job, error) {
	return Job{}, ErrNotImplemented
}

// Cancel 取消编号为 id 的任务。取消后不再重试。
// 任务不存在时返回错误。当前返回 ErrNotImplemented。
func (p *Pipeline) Cancel(ctx context.Context, id string) error {
	return ErrNotImplemented
}

// Documents 返回一个集合内的文档，不包含其他集合。
// 当前返回 ErrNotImplemented。
func (p *Pipeline) Documents(ctx context.Context, col model.CollectionID) ([]model.Document, error) {
	return nil, ErrNotImplemented
}

// Ask 只检索 req 点名的集合，规则与 retrieve.Service 相同。
// 当前返回 ErrNotImplemented。
func (p *Pipeline) Ask(ctx context.Context, req retrieve.Request) ([]retrieve.Hit, error) {
	return nil, ErrNotImplemented
}

// Evaluate 读取夹具并生成报告。
// 没有基线时 Direction 为 baseline，RecallDelta 与 MRRDelta 为 0。
// 当前返回 ErrNotImplemented。
func (p *Pipeline) Evaluate(ctx context.Context) (eval.Report, error) {
	return eval.Report{}, ErrNotImplemented
}
