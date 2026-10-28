package index

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// elasticIndexBody 是一个集合的索引设置。
// 中文用 Elasticsearch 的 smartcn 分析器。本机没有 IK 插件，索引用 smartcn，不用 IK。
// smartcn 由 analysis-smartcn 提供。创建索引前，本机 Elasticsearch 要已经装上这个插件。
// body 和 heading_path 的 similarity 是 BM25。命中分数用 Elasticsearch 返回的 _score，不在进程内重算。
const elasticIndexBody = `{
  "settings": {
    "index": {
      "similarity": {
        "urag_bm25": {
          "type": "BM25",
          "k1": 1.2,
          "b": 0.75
        }
      }
    },
    "analysis": {
      "analyzer": {
        "urag_zh": {
          "type": "smartcn"
        }
      }
    }
  },
  "mappings": {
    "properties": {
      "chunk_id": {"type": "keyword"},
      "parent_id": {"type": "long"},
      "doc_id": {"type": "long"},
      "collection_id": {"type": "keyword"},
      "heading_path": {"type": "text", "analyzer": "urag_zh", "similarity": "urag_bm25"},
      "body": {"type": "text", "analyzer": "urag_zh", "similarity": "urag_bm25"}
    }
  }
}`

// dialElasticHTTP 只连接调用方给出的地址。不要在这里填默认主机或端口。
func dialElasticHTTP(ctx context.Context, addr string) (elasticSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkElasticAddr(addr); err != nil {
		return nil, err
	}
	u, err := url.Parse(addr)
	if err != nil {
		return nil, err
	}
	user := ""
	pass := ""
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
		u.User = nil
	}
	base := u.Scheme + "://" + u.Host
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &httpElastic{
		base:    base,
		host:    u.Host,
		user:    user,
		pass:    pass,
		rawAddr: addr,
		client:  client,
	}, nil
}

type httpElastic struct {
	base    string
	host    string
	user    string
	pass    string
	rawAddr string
	client  *http.Client
}

// ElasticReady 报告这个地址上有没有 Elasticsearch。
// 空地址返回 false，不拨号。地址格式不对时返回错误。
// 连不上时返回 false，不把地址写进错误。
func ElasticReady(ctx context.Context, addr string) (bool, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false, nil
	}
	if err := checkElasticAddr(addr); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, nil
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return true, nil
}

func (h *httpElastic) Close() error {
	if h == nil || h.client == nil {
		return nil
	}
	h.client.CloseIdleConnections()
	return nil
}

func (h *httpElastic) Exists(ctx context.Context, index string) (bool, error) {
	status, raw, err := h.do(ctx, http.MethodHead, "/"+url.PathEscape(index), nil, "")
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, h.fail(fmt.Errorf("status %d: %s", status, elasticReason(raw)))
	}
}

func (h *httpElastic) Create(ctx context.Context, index string) error {
	status, raw, err := h.do(ctx, http.MethodPut, "/"+url.PathEscape(index), strings.NewReader(elasticIndexBody), "application/json")
	if err != nil {
		return err
	}
	if status == http.StatusOK || status == http.StatusCreated {
		return nil
	}
	reason := elasticReason(raw)
	if strings.Contains(reason, "resource_already_exists_exception") {
		return nil
	}
	return h.fail(fmt.Errorf("status %d: %s", status, reason))
}

func (h *httpElastic) Bulk(ctx context.Context, index string, docs []elasticDoc) error {
	if len(docs) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, doc := range docs {
		meta, err := json.Marshal(elasticBulkMeta{Index: elasticBulkOp{Index: index, ID: doc.ChunkID}})
		if err != nil {
			return err
		}
		line, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		buf.Write(meta)
		buf.WriteByte('\n')
		buf.Write(line)
		buf.WriteByte('\n')
	}
	status, raw, err := h.do(ctx, http.MethodPost, "/_bulk", &buf, "application/x-ndjson")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return h.fail(fmt.Errorf("status %d: %s", status, elasticReason(raw)))
	}
	var parsed struct {
		Errors bool `json:"errors"`
		Items  []struct {
			Index struct {
				Status int `json:"status"`
				Error  *struct {
					Type   string `json:"type"`
					Reason string `json:"reason"`
				} `json:"error"`
			} `json:"index"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return h.fail(fmt.Errorf("bulk response: %w", err))
	}
	if !parsed.Errors {
		return nil
	}
	for _, item := range parsed.Items {
		if item.Index.Error == nil {
			continue
		}
		return h.fail(fmt.Errorf("status %d: %s: %s", item.Index.Status, item.Index.Error.Type, item.Index.Error.Reason))
	}
	return h.fail(errors.New("bulk item failed"))
}

func (h *httpElastic) Refresh(ctx context.Context, index string) error {
	status, raw, err := h.do(ctx, http.MethodPost, "/"+url.PathEscape(index)+"/_refresh", nil, "")
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return h.fail(fmt.Errorf("status %d: %s", status, elasticReason(raw)))
	}
	return nil
}

func (h *httpElastic) Search(ctx context.Context, index, collectionID, query string, topK int) ([]elasticHit, error) {
	if topK <= 0 || strings.TrimSpace(query) == "" {
		return []elasticHit{}, nil
	}
	payload := map[string]any{
		"size":    topK,
		"_source": []string{"chunk_id", "parent_id", "doc_id", "collection_id", "heading_path", "body"},
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []any{
					map[string]any{"term": map[string]any{"collection_id": collectionID}},
				},
				"must": map[string]any{
					"multi_match": map[string]any{
						"query":  query,
						"fields": []string{"body", "heading_path"},
						"type":   "most_fields",
					},
				},
			},
		},
	}
	rawBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	status, raw, err := h.do(ctx, http.MethodPost, "/"+url.PathEscape(index)+"/_search", bytes.NewReader(rawBody), "application/json")
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, h.fail(fmt.Errorf("status %d: %s", status, elasticReason(raw)))
	}
	var parsed struct {
		Hits struct {
			Hits []struct {
				Score  *float64   `json:"_score"`
				Source elasticDoc `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, h.fail(fmt.Errorf("search response: %w", err))
	}
	out := make([]elasticHit, 0, len(parsed.Hits.Hits))
	for _, hit := range parsed.Hits.Hits {
		if hit.Score == nil || hit.Source.CollectionID != collectionID {
			continue
		}
		out = append(out, elasticHit{Doc: hit.Source, Score: *hit.Score})
	}
	return out, nil
}

func (h *httpElastic) do(ctx context.Context, method, path string, body io.Reader, contentType string) (int, []byte, error) {
	if h == nil || h.client == nil {
		return 0, nil, errors.New("nil elasticsearch client")
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, body)
	if err != nil {
		return 0, nil, h.fail(err)
	}
	if h.user != "" || h.pass != "" {
		req.SetBasicAuth(h.user, h.pass)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	res, err := h.client.Do(req)
	if err != nil {
		return 0, nil, h.fail(err)
	}
	defer res.Body.Close()
	if res.Header.Get("X-Elastic-Product") != "Elasticsearch" {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return 0, nil, h.fail(errors.New("response is not Elasticsearch"))
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return 0, nil, h.fail(err)
	}
	return res.StatusCode, raw, nil
}

func (h *httpElastic) fail(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(h.redact(err.Error()))
}

func (h *httpElastic) redact(msg string) string {
	if h == nil {
		return msg
	}
	if h.pass != "" {
		msg = strings.ReplaceAll(msg, h.pass, "redacted")
	}
	if h.user != "" {
		msg = strings.ReplaceAll(msg, h.user, "elastic-user")
	}
	if h.rawAddr != "" {
		msg = strings.ReplaceAll(msg, h.rawAddr, "elasticsearch")
	}
	if h.base != "" {
		msg = strings.ReplaceAll(msg, h.base, "elasticsearch")
	}
	if h.host != "" {
		msg = strings.ReplaceAll(msg, h.host, "elasticsearch")
	}
	return msg
}

func elasticReason(raw []byte) string {
	var parsed struct {
		Error struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil && (parsed.Error.Type != "" || parsed.Error.Reason != "") {
		if parsed.Error.Type == "" {
			return parsed.Error.Reason
		}
		if parsed.Error.Reason == "" {
			return parsed.Error.Type
		}
		return parsed.Error.Type + ": " + parsed.Error.Reason
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "empty error"
	}
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}

type elasticBulkMeta struct {
	Index elasticBulkOp `json:"index"`
}

type elasticBulkOp struct {
	Index string `json:"_index"`
	ID    string `json:"_id"`
}
