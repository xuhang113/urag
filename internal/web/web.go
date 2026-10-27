// Package web 定义页面和 JSON API 的路由契约。
// 本包只把请求交给任务、检索和评价入口。
// 本包不计算向量，也不把上传正文读进内存。
package web

import (
	"errors"
	"net/http"

	"github.com/xuhang113/urag/internal/job"
)

// ErrNotImplemented 表示 HTTP 服务尚未接通。
var ErrNotImplemented = errors.New("web: not implemented")

// Listen 在 addr 上提供 Handler。
// 当前返回 ErrNotImplemented，不打开端口。
func Listen(addr string, p *job.Pipeline) error {
	return ErrNotImplemented
}

// Handler 的路由契约如下。
//
//	GET / 跳转到 /upload。
//	GET /upload、/fetch、/clean、/ask、/eval 返回对应页面。
//	POST /api/uploads 接受表单字段 collection、idempotency_key、file。
//	成功时返回 202，JSON 含 job_id、object_key、checksum、size、collection_id。
//	文件流式交给 job.Pipeline.AcceptUpload。
//	POST /api/fetch 接受 JSON 字段 collection_id、kind、uri、idempotency_prefix。
//	成功时返回 202，JSON 含 job_id。
//	GET /api/jobs/{id} 返回 job.Job。
//	GET /api/collections/{id}/documents 返回该集合的文档。
//	POST /api/retrieve 的 JSON 正文是 retrieve.Request，响应是 []retrieve.Hit。
//	POST /api/eval 与 GET /api/eval/report 返回 eval.Report。
//
// 本包不计算向量，也不把上传文件读进内存。
// 当前返回 http.NotFoundHandler，不注册上述路由。
func Handler(p *job.Pipeline) http.Handler {
	return http.NotFoundHandler()
}
