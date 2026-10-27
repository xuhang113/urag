package web

import (
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/xuhang113/urag/internal/eval"
	"github.com/xuhang113/urag/internal/job"
	"github.com/xuhang113/urag/internal/model"
	"github.com/xuhang113/urag/internal/retrieve"
)

type pageData struct {
	Page   string
	Title  string
	Error  string
	Upload *uploadView
	Fetch  *fetchView
	Clean  *cleanView
	Ask    *askView
	Try    *tryView
	Eval   *evalView
}

type uploadView struct {
	Collection string
	JobID      string
	ObjectKey  string
	Checksum   string
	Size       int64
}

type fetchView struct {
	Collection string
	Kind       string
	URI        string
	Prefix     string
	JobID      string
	State      string
}

type cleanView struct {
	Collection string
	Documents  []model.Document
}

type askView struct {
	Collections string
	Query       string
	TopK        int
	Hybrid      bool
	Hits        []retrieve.Hit
}

type tryView struct {
	Collection string
}

type evalView struct {
	Normal   eval.Report
	Contrast eval.Report
	Ready    bool
}

func (s *server) pageUpload(w http.ResponseWriter, r *http.Request) {
	data := pageData{Page: "upload", Title: "上传"}
	if r.Method == http.MethodPost {
		acc, collection, err := s.acceptUpload(r)
		if err != nil {
			data.Error = err.Error()
		} else {
			data.Upload = &uploadView{
				Collection: collection,
				JobID:      acc.Job.ID,
				ObjectKey:  string(acc.ObjectKey),
				Checksum:   string(acc.Checksum),
				Size:       acc.Size,
			}
		}
	}
	s.render(w, data)
}

func (s *server) pageFetch(w http.ResponseWriter, r *http.Request) {
	data := pageData{Page: "fetch", Title: "获取", Fetch: &fetchView{Kind: "local_dir"}}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			data.Error = err.Error()
			s.render(w, data)
			return
		}
		in := job.FetchInput{
			CollectionID:      model.CollectionID(strings.TrimSpace(r.FormValue("collection_id"))),
			Kind:              strings.TrimSpace(r.FormValue("kind")),
			URI:               strings.TrimSpace(r.FormValue("uri")),
			IdempotencyPrefix: strings.TrimSpace(r.FormValue("idempotency_prefix")),
		}
		data.Fetch = &fetchView{
			Collection: string(in.CollectionID),
			Kind:       in.Kind,
			URI:        in.URI,
			Prefix:     in.IdempotencyPrefix,
		}
		if err := s.requirePipeline(); err != nil {
			data.Error = err.Error()
		} else if got, err := s.p.SubmitFetch(r.Context(), in); err != nil {
			data.Error = err.Error()
			if got.ID != "" {
				data.Fetch.JobID = got.ID
				data.Fetch.State = got.State
			}
		} else {
			data.Fetch.JobID = got.ID
			data.Fetch.State = got.State
		}
	}
	s.render(w, data)
}

func (s *server) pageClean(w http.ResponseWriter, r *http.Request) {
	col := strings.TrimSpace(r.URL.Query().Get("collection"))
	data := pageData{Page: "clean", Title: "清洗", Clean: &cleanView{Collection: col}}
	if col != "" {
		if err := s.requirePipeline(); err != nil {
			data.Error = err.Error()
		} else {
			docs, err := s.p.Documents(r.Context(), model.CollectionID(col))
			if err != nil {
				data.Error = err.Error()
			} else {
				data.Clean.Documents = docs
			}
		}
	}
	s.render(w, data)
}

func (s *server) pageAsk(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	topK := 5
	if raw := q.Get("top_k"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			topK = n
		}
	}
	view := &askView{
		Collections: q.Get("collections"),
		Query:       q.Get("query"),
		TopK:        topK,
		Hybrid:      q.Get("hybrid") == "1" || q.Get("hybrid") == "on",
	}
	data := pageData{Page: "ask", Title: "提问", Ask: view}
	if strings.TrimSpace(view.Query) != "" && strings.TrimSpace(view.Collections) != "" {
		if err := s.requirePipeline(); err != nil {
			data.Error = err.Error()
		} else {
			hits, err := s.p.Ask(r.Context(), retrieve.Request{
				Collections: splitCollections(view.Collections),
				Query:       view.Query,
				TopK:        view.TopK,
				Hybrid:      view.Hybrid,
			})
			if err != nil {
				data.Error = err.Error()
			} else {
				view.Hits = hits
			}
		}
	}
	s.render(w, data)
}

func (s *server) pageTry(w http.ResponseWriter, r *http.Request) {
	data := pageData{Page: "try", Title: "试查", Try: &tryView{Collection: "public"}}
	s.render(w, data)
}

func (s *server) pageEval(w http.ResponseWriter, r *http.Request) {
	data := pageData{Page: "eval", Title: "评价", Eval: &evalView{}}
	if r.Method == http.MethodPost {
		payload, err := s.evaluate(r)
		if err != nil {
			data.Error = err.Error()
		} else {
			data.Eval.Normal = payload.Normal
			data.Eval.Contrast = payload.Contrast
			data.Eval.Ready = true
		}
	}
	s.render(w, data)
}

func splitCollections(raw string) []model.CollectionID {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n'
	})
	out := make([]model.CollectionID, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, model.CollectionID(part))
		}
	}
	return out
}

func (s *server) render(w http.ResponseWriter, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

var pageTmpl = template.Must(template.New("layout").Parse(layoutHTML + uploadHTML + fetchHTML + cleanHTML + askHTML + tryHTML + evalHTML))

const layoutHTML = `{{define "layout"}}<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>{{.Title}}</title>
<style>
body { margin: 2rem auto; max-width: 52rem; font: 16px/1.5 sans-serif; color: #1c1c1c; }
nav a { margin-right: 1rem; }
main { margin-top: 1.5rem; }
label { display: block; margin: 0.8rem 0 0.2rem; }
input, select, button, textarea { font: inherit; }
table { border-collapse: collapse; width: 100%; margin-top: 1rem; }
th, td { border: 1px solid #ccc; padding: 0.35rem 0.5rem; text-align: left; vertical-align: top; word-break: break-word; }
#try-log { margin: 0.8rem 0; padding-left: 1.2rem; }
#try-log li.pending { font-weight: 600; }
.err { color: #8a1f1f; }
.meta dt { font-weight: 600; }
.meta dd { margin: 0 0 0.4rem; word-break: break-all; }
</style>
</head>
<body>
<nav>
<a href="/upload">上传</a>
<a href="/fetch">获取</a>
<a href="/clean">清洗</a>
<a href="/ask">提问</a>
<a href="/try">试查</a>
<a href="/eval">评价</a>
</nav>
<main id="page-{{.Page}}">
<h1>{{.Title}}</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if eq .Page "upload"}}{{template "upload" .}}{{end}}
{{if eq .Page "fetch"}}{{template "fetch" .}}{{end}}
{{if eq .Page "clean"}}{{template "clean" .}}{{end}}
{{if eq .Page "ask"}}{{template "ask" .}}{{end}}
{{if eq .Page "try"}}{{template "try" .}}{{end}}
{{if eq .Page "eval"}}{{template "eval" .}}{{end}}
</main>
</body>
</html>{{end}}`

const uploadHTML = `{{define "upload"}}
<form method="post" action="/upload" enctype="multipart/form-data">
<label for="collection">集合</label>
<input id="collection" name="collection" required>
<label for="idempotency_key">幂等键</label>
<input id="idempotency_key" name="idempotency_key">
<label for="file">文件</label>
<input id="file" name="file" type="file" required>
<button type="submit">上传</button>
</form>
{{with .Upload}}
<dl class="meta">
<dt>任务编号</dt><dd>{{.JobID}}</dd>
<dt>对象键</dt><dd>{{.ObjectKey}}</dd>
<dt>校验和</dt><dd>{{.Checksum}}</dd>
<dt>大小</dt><dd>{{.Size}}</dd>
<dt>集合</dt><dd>{{.Collection}}</dd>
</dl>
{{end}}
{{end}}`

const fetchHTML = `{{define "fetch"}}
<form method="post" action="/fetch">
<label for="collection_id">集合</label>
<input id="collection_id" name="collection_id" value="{{.Fetch.Collection}}" required>
<label for="kind">来源</label>
<select id="kind" name="kind">
<option value="local_dir" {{if eq .Fetch.Kind "local_dir"}}selected{{end}}>本地目录</option>
<option value="http" {{if eq .Fetch.Kind "http"}}selected{{end}}>URL</option>
</select>
<label for="uri">目录或 URL</label>
<input id="uri" name="uri" value="{{.Fetch.URI}}" required>
<label for="idempotency_prefix">幂等前缀</label>
<input id="idempotency_prefix" name="idempotency_prefix" value="{{.Fetch.Prefix}}">
<button type="submit">获取</button>
</form>
{{with .Fetch}}{{if .JobID}}
<dl class="meta">
<dt>任务编号</dt><dd>{{.JobID}}</dd>
<dt>状态</dt><dd>{{.State}}</dd>
</dl>
{{end}}{{end}}
{{end}}`

const cleanHTML = `{{define "clean"}}
<form method="get" action="/clean">
<label for="collection">集合</label>
<input id="collection" name="collection" value="{{.Clean.Collection}}" required>
<button type="submit">查看清洗结果</button>
</form>
{{with .Clean}}{{if .Documents}}
<table>
<thead><tr><th>文档</th><th>重复</th><th>指向</th><th>正文</th></tr></thead>
<tbody>
{{range .Documents}}
<tr>
<td>{{.ID}}</td>
<td>{{.Duplicate}}</td>
<td>{{.Lineage.DuplicateOf}}</td>
<td>{{.Text}}</td>
</tr>
{{end}}
</tbody>
</table>
{{end}}{{end}}
{{end}}`

const askHTML = `{{define "ask"}}
<form method="get" action="/ask">
<label for="collections">集合</label>
<input id="collections" name="collections" value="{{.Ask.Collections}}" placeholder="alpha,beta" required>
<label for="query">问题</label>
<input id="query" name="query" value="{{.Ask.Query}}" required>
<label for="top_k">TopK</label>
<input id="top_k" name="top_k" value="{{.Ask.TopK}}">
<label><input type="checkbox" name="hybrid" value="1" {{if .Ask.Hybrid}}checked{{end}}>混合</label>
<button type="submit">提问</button>
</form>
{{with .Ask}}{{if .Hits}}
<table>
<thead><tr><th>集合</th><th>文档</th><th>片段</th><th>分数</th><th>Start</th><th>End</th><th>正文</th></tr></thead>
<tbody>
{{range .Hits}}
<tr>
<td>{{.CollectionID}}</td>
<td>{{.DocumentID}}</td>
<td>{{.ChunkID}}</td>
<td>{{.Score}}</td>
<td>{{.StartByte}}</td>
<td>{{.EndByte}}</td>
<td>{{.Text}}</td>
</tr>
{{end}}
</tbody>
</table>
{{end}}{{end}}
{{end}}`

const tryHTML = `{{define "try"}}
<p>只查集合 {{.Try.Collection}}。磁盘向量和这个集合的 BM25 合并后留下前 50 条，再重排。分数高于门槛才显示，最多 8 条。嵌入模型退出后才加载重排模型。下面按这次查询的实际顺序显示步骤，进行中的一步标成未完成。</p>
<form id="try-form">
<label for="query">问题</label>
<input id="query" name="query" required>
<button type="submit">试查</button>
</form>
<ol id="try-log"></ol>
<p id="try-status"></p>
<p id="try-error" class="err"></p>
<div id="try-result"></div>
<script>
(function () {
  var form = document.getElementById("try-form");
  var log = document.getElementById("try-log");
  var status = document.getElementById("try-status");
  var err = document.getElementById("try-error");
  var result = document.getElementById("try-result");
  var items = {};
  function upsert(step) {
    var li = items[step.name];
    if (!li) {
      li = document.createElement("li");
      items[step.name] = li;
      log.appendChild(li);
    }
    li.className = step.done ? "done" : "pending";
    li.textContent = step.name + (step.done ? " 完成" : " 未完成");
  }
  function showHits(hits) {
    result.textContent = "";
    if (!hits || !hits.length) {
      status.textContent = "没有足够相关的片段";
      return;
    }
    status.textContent = "命中 " + hits.length + " 条";
    var table = document.createElement("table");
    var head = document.createElement("tr");
    ["collection_id", "heading_path", "原文位置", "分数", "子块", "父块"].forEach(function (name) {
      var th = document.createElement("th");
      th.textContent = name;
      head.appendChild(th);
    });
    var thead = document.createElement("thead");
    thead.appendChild(head);
    table.appendChild(thead);
    var body = document.createElement("tbody");
    hits.forEach(function (hit) {
      var tr = document.createElement("tr");
      var pos = String(hit.start_byte) + "–" + String(hit.end_byte);
      [hit.collection_id, hit.heading_path || "", pos, String(hit.score), hit.chunk || "", hit.parent || ""].forEach(function (value) {
        var td = document.createElement("td");
        td.textContent = value == null ? "" : String(value);
        tr.appendChild(td);
      });
      body.appendChild(tr);
    });
    table.appendChild(body);
    result.appendChild(table);
  }
  function applyBlock(block) {
    var eventName = "message";
    var data = "";
    block.split("\n").forEach(function (line) {
      if (line.indexOf("event:") === 0) {
        eventName = line.slice(6).replace(/^\s+/, "");
      } else if (line.indexOf("data:") === 0) {
        data += line.slice(5).replace(/^\s+/, "");
      }
    });
    if (!data || data.charAt(0) === ":") {
      return;
    }
    var payload = JSON.parse(data);
    if (eventName === "step") {
      upsert(payload);
      return;
    }
    if (eventName === "result") {
      showHits(payload.hits || []);
      return;
    }
    if (eventName === "error") {
      status.textContent = "";
      err.textContent = payload.message || "检索失败";
    }
  }
  form.addEventListener("submit", function (ev) {
    ev.preventDefault();
    var query = document.getElementById("query").value;
    err.textContent = "";
    result.textContent = "";
    status.textContent = "";
    log.textContent = "";
    items = {};
    var timeoutText = "检索超时。收录正在占用嵌入模型，这次没有结果。";
    var controller = new AbortController();
    var timer = window.setTimeout(function () {
      controller.abort();
    }, 180000);
    var pending = "";
    function consume(text) {
      pending += text;
      var parts = pending.split("\n\n");
      pending = parts.pop();
      parts.forEach(function (block) {
        if (block.replace(/\s/g, "") !== "") {
          applyBlock(block);
        }
      });
    }
    fetch("/api/try", {
      method: "POST",
      headers: {"Content-Type": "application/json", "Accept": "text/event-stream"},
      body: JSON.stringify({query: query}),
      signal: controller.signal
    }).then(function (res) {
      var type = res.headers.get("content-type") || "";
      if (type.indexOf("text/event-stream") === -1) {
        return res.text().then(function (text) {
          if (!res.ok) {
            throw new Error(text || ("HTTP " + res.status));
          }
          showHits(JSON.parse(text));
        });
      }
      if (!res.ok) {
        return res.text().then(function (text) {
          throw new Error(text || ("HTTP " + res.status));
        });
      }
      var reader = res.body.getReader();
      var decoder = new TextDecoder();
      function pump() {
        return reader.read().then(function (chunk) {
          if (chunk.done) {
            if (pending.replace(/\s/g, "") !== "") {
              applyBlock(pending);
            }
            return;
          }
          consume(decoder.decode(chunk.value, {stream: true}));
          return pump();
        });
      }
      return pump();
    }).catch(function (e) {
      if (err.textContent) {
        return;
      }
      status.textContent = "";
      if (e && e.name === "AbortError") {
        err.textContent = timeoutText;
        return;
      }
      var message = e && e.message ? String(e.message).trim() : "";
      err.textContent = message || "检索失败";
    }).finally(function () {
      window.clearTimeout(timer);
    });
  });
})();
</script>
{{end}}`

const evalHTML = `{{define "eval"}}
<form method="post" action="/eval">
<button type="submit">运行评价</button>
</form>
{{with .Eval}}{{if .Ready}}
<h2>正常切分</h2>
<p>方向 {{.Normal.Direction}}，Recall 差 {{.Normal.RecallDelta}}，MRR 差 {{.Normal.MRRDelta}}</p>
{{template "scores" .Normal.Scores}}
<h2>对照切分</h2>
<p>方向 {{.Contrast.Direction}}，Recall 差 {{.Contrast.RecallDelta}}，MRR 差 {{.Contrast.MRRDelta}}</p>
{{template "scores" .Contrast.Scores}}
{{end}}{{end}}
{{end}}

{{define "scores"}}
<table>
<thead><tr><th>集合</th><th>Recall@K</th><th>MRR</th><th>K</th></tr></thead>
<tbody>
{{range .}}
<tr>
<td>{{.CollectionID}}</td>
<td>{{.RecallAtK}}</td>
<td>{{.MRR}}</td>
<td>{{.K}}</td>
</tr>
{{end}}
</tbody>
</table>
{{end}}`
