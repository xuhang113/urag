// Package web 定义页面和 JSON API。
// 本包只把请求交给任务、检索和评价入口。
// 本包不计算向量，也不把上传正文读进内存。
package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/xuhang113/urag/internal/eval"
	"github.com/xuhang113/urag/internal/job"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
)

// ErrNotImplemented 表示 HTTP 服务尚未接通。
var ErrNotImplemented = errors.New("web: not implemented")

// Listen 在 addr 上提供 Handler。addr 为空时使用 :8080。
func Listen(addr string, p *job.Pipeline) error {
	if strings.TrimSpace(addr) == "" {
		addr = ":8080"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           Handler(p),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

type server struct {
	p *job.Pipeline
}

// Handler 注册页面和 JSON API。
//
//	GET / 跳转到 /upload。
//	GET /upload、/fetch、/clean、/ask、/try、/eval 返回对应页面。
//	POST /api/uploads 接受表单字段 collection、idempotency_key、file。
//	成功时返回 202，JSON 含 job_id、object_key、checksum、size、collection_id。
//	文件流式交给 job.Pipeline.AcceptUpload。
//	POST /api/fetch 接受 JSON 字段 collection_id、kind、uri、idempotency_prefix。
//	成功时返回 202，JSON 含 job_id。
//	GET /api/jobs/{id} 返回 job.Job。
//	GET /api/collections/{id}/documents 返回该集合的文档。
//	POST /api/retrieve 的 JSON 正文是 retrieve.Request，响应是 []retrieve.Hit。
//	POST /api/try 只接受 JSON 字段 query，只检索集合 public。
//	嵌入或重排在 trySearchTimeout 内没有返回时，结束这次请求。
//	POST /api/eval 与 GET /api/eval/report 同时返回正常和对照报告。
func Handler(p *job.Pipeline) http.Handler {
	s := &server{p: p}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /upload", s.pageUpload)
	mux.HandleFunc("POST /upload", s.pageUpload)
	mux.HandleFunc("GET /fetch", s.pageFetch)
	mux.HandleFunc("POST /fetch", s.pageFetch)
	mux.HandleFunc("GET /clean", s.pageClean)
	mux.HandleFunc("GET /ask", s.pageAsk)
	mux.HandleFunc("GET /try", s.pageTry)
	mux.HandleFunc("GET /eval", s.pageEval)
	mux.HandleFunc("POST /eval", s.pageEval)
	mux.HandleFunc("POST /api/uploads", s.apiUpload)
	mux.HandleFunc("POST /api/fetch", s.apiFetch)
	mux.HandleFunc("GET /api/jobs/{id}", s.apiJob)
	mux.HandleFunc("GET /api/collections/{id}/documents", s.apiDocuments)
	mux.HandleFunc("POST /api/retrieve", s.apiRetrieve)
	mux.HandleFunc("POST /api/try", s.apiTry)
	mux.HandleFunc("POST /api/eval", s.apiEval)
	mux.HandleFunc("GET /api/eval/report", s.apiEval)
	return mux
}

func (s *server) home(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/upload", http.StatusFound)
}

func (s *server) apiUpload(w http.ResponseWriter, r *http.Request) {
	acc, collection, err := s.acceptUpload(r)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id":        acc.Job.ID,
		"object_key":    acc.ObjectKey,
		"checksum":      acc.Checksum,
		"size":          acc.Size,
		"collection_id": collection,
	})
}

func (s *server) apiFetch(w http.ResponseWriter, r *http.Request) {
	if err := s.requirePipeline(); err != nil {
		writeError(w, err)
		return
	}
	var in job.FetchInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	got, err := s.p.SubmitFetch(r.Context(), in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": got.ID})
}

func (s *server) apiJob(w http.ResponseWriter, r *http.Request) {
	if err := s.requirePipeline(); err != nil {
		writeError(w, err)
		return
	}
	got, err := s.p.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (s *server) apiDocuments(w http.ResponseWriter, r *http.Request) {
	if err := s.requirePipeline(); err != nil {
		writeError(w, err)
		return
	}
	docs, err := s.p.Documents(r.Context(), model.CollectionID(r.PathValue("id")))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, docs)
}

// trySearchTimeout 是一次试查最多等待的时间。
// 嵌入或重排子进程到点还没返回，就结束这次请求，不再停在检索中。
var trySearchTimeout = 3 * time.Minute

// tryTimeoutText 是试查超时后页面和接口展示的失败说明。
const tryTimeoutText = "检索超时。收录正在占用嵌入模型，这次没有结果。"

func (s *server) apiTry(w http.ResponseWriter, r *http.Request) {
	if err := s.requirePipeline(); err != nil {
		writeError(w, err)
		return
	}
	var in struct {
		Query string `json:"query"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(&in); err != nil {
		http.Error(w, "web: bad request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(in.Query) == "" {
		http.Error(w, "web: query required", http.StatusBadRequest)
		return
	}
	if wantsTryStream(r) {
		s.streamTry(w, r, in.Query)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), trySearchTimeout)
	defer cancel()
	type tryOutcome struct {
		hits []retrieve.Hit
		err  error
	}
	done := make(chan tryOutcome, 1)
	go func() {
		hits, err := s.p.Try(ctx, in.Query)
		done <- tryOutcome{hits: hits, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			if tryTimedOut(ctx, got.err) {
				writeTryTimeout(w)
				return
			}
			writeError(w, got.err)
			return
		}
		hits := got.hits
		if hits == nil {
			hits = []retrieve.Hit{}
		}
		writeJSON(w, http.StatusOK, hits)
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			writeTryTimeout(w)
		}
	}
}

func tryTimedOut(ctx context.Context, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return errors.Is(ctx.Err(), context.DeadlineExceeded)
}

func writeTryTimeout(w http.ResponseWriter) {
	http.Error(w, tryTimeoutText, http.StatusGatewayTimeout)
}

func (s *server) apiRetrieve(w http.ResponseWriter, r *http.Request) {
	if err := s.requirePipeline(); err != nil {
		writeError(w, err)
		return
	}
	var req retrieve.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hits, err := s.p.Ask(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	if hits == nil {
		hits = []retrieve.Hit{}
	}
	writeJSON(w, http.StatusOK, hits)
}

func (s *server) apiEval(w http.ResponseWriter, r *http.Request) {
	payload, err := s.evaluate(r)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) evaluate(r *http.Request) (evalPayload, error) {
	if err := s.requirePipeline(); err != nil {
		return evalPayload{}, err
	}
	normal, err := s.p.Evaluate(r.Context())
	if err != nil {
		return evalPayload{}, err
	}
	contrast, err := s.p.EvaluateContrast(r.Context())
	if err != nil {
		return evalPayload{}, err
	}
	return evalPayload{Normal: normal, Contrast: contrast}, nil
}

type evalPayload struct {
	Normal   eval.Report `json:"normal"`
	Contrast eval.Report `json:"contrast"`
}

func (s *server) acceptUpload(r *http.Request) (job.Accepted, string, error) {
	if err := s.requirePipeline(); err != nil {
		return job.Accepted{}, "", err
	}
	reader, err := r.MultipartReader()
	if err != nil {
		return job.Accepted{}, "", err
	}
	var (
		collection  string
		idem        string
		filename    string
		contentType string
	)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return job.Accepted{}, "", err
		}
		switch part.FormName() {
		case "collection":
			collection, err = readField(part)
		case "idempotency_key":
			idem, err = readField(part)
		case "file":
			if strings.TrimSpace(collection) == "" {
				part.Close()
				return job.Accepted{}, "", errors.New("web: collection must precede file")
			}
			filename = part.FileName()
			contentType = part.Header.Get("Content-Type")
			acc, putErr := s.p.AcceptUpload(r.Context(), job.UploadInput{
				CollectionID:   model.CollectionID(strings.TrimSpace(collection)),
				IdempotencyKey: model.IdempotencyKey(strings.TrimSpace(idem)),
				Filename:       filename,
				ContentType:    contentType,
				Body:           part,
			})
			part.Close()
			if putErr != nil {
				return job.Accepted{}, collection, putErr
			}
			return acc, strings.TrimSpace(collection), nil
		default:
			_, err = readField(part)
		}
		part.Close()
		if err != nil {
			return job.Accepted{}, "", err
		}
	}
	return job.Accepted{}, "", errors.New("web: file required")
}

// readField 只读取集合名、幂等键这类短字段。上传文件不经过这里，而是流式交给任务。
func readField(part *multipart.Part) (string, error) {
	b, err := io.ReadAll(io.LimitReader(part, 8<<10+1))
	if err != nil {
		return "", err
	}
	if len(b) > 8<<10 {
		return "", errors.New("web: field too large")
	}
	return strings.TrimSpace(string(b)), nil
}

func (s *server) requirePipeline() error {
	if s == nil || s.p == nil {
		return errors.New("web: pipeline is nil")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, job.ErrNotFound) {
		status = http.StatusNotFound
	}
	http.Error(w, err.Error(), status)
}
