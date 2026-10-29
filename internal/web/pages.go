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
	Collection  string
	RerankModel string
	RerankDir   string
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
	data := pageData{Page: "try", Title: "试查", Try: &tryView{
		Collection:  "public",
		RerankModel: s.tryInfo.RerankModel,
		RerankDir:   s.tryInfo.RerankDir,
	}}
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
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light">
<title>{{.Title}}</title>
<style>
:root {
  --ink: #1d1915;
  --ink-soft: #534e46;
  --muted: #6e675e;
  --line: #ddd3c4;
  --line-strong: #cbbfae;
  --paper: #e7dfd2;
  --card: #fffdf9;
  --accent: #9d3b2e;
  --accent-deep: #7c2e23;
  --ok: #246046;
  --warn: #7a5412;
  --err: #7a2c22;
  --spine: #1b1815;
  --serif: "Songti SC", "Noto Serif SC", "Source Han Serif SC", "STSong", serif;
  --sans: "PingFang SC", "Hiragino Sans GB", "Noto Sans SC", "Source Han Sans SC", sans-serif;
  --shadow: 0 16px 40px rgba(55, 40, 24, 0.06);
}
* { box-sizing: border-box; }
html { min-height: 100%; }
body {
  margin: 0;
  min-height: 100%;
  color: var(--ink);
  background:
    radial-gradient(ellipse at 0 0, rgba(255, 252, 247, 0.72), transparent 42%),
    var(--paper);
  font: 15.5px/1.65 var(--sans);
  -webkit-font-smoothing: antialiased;
}
::selection { background: #f0d2cb; color: var(--ink); }
.skip {
  position: absolute;
  left: 12px;
  top: -48px;
  z-index: 8;
  padding: 8px 12px;
  background: #fff;
  color: var(--ink);
  border-radius: 8px;
}
.skip:focus { top: 12px; }
.app {
  display: grid;
  grid-template-columns: 228px minmax(0, 1fr);
  min-height: 100vh;
}
.spine {
  position: sticky;
  top: 0;
  align-self: start;
  height: 100vh;
  display: flex;
  flex-direction: column;
  gap: 16px;
  padding: 26px 16px 18px;
  background: var(--spine);
  color: #e8e1d6;
}
.brand {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 0 8px;
  color: inherit;
  text-decoration: none;
}
.seal {
  width: 40px;
  height: 40px;
  display: grid;
  place-items: center;
  border: 1.5px solid #c4513a;
  border-radius: 3px;
  color: #f4d4cd;
  font-family: var(--serif);
  font-size: 22px;
  line-height: 1;
  transform: rotate(-8deg);
}
.brand-text strong {
  display: block;
  font-family: var(--serif);
  font-size: 22px;
  font-weight: 600;
  letter-spacing: 0.12em;
  line-height: 1;
}
.brand-text small {
  display: block;
  margin-top: 5px;
  color: #b7aea2;
  font-size: 12px;
  letter-spacing: 0.08em;
}
.spine-label {
  margin: 4px 12px 0;
  color: #8f877d;
  font-size: 11px;
  letter-spacing: 0.18em;
}
.spine nav { display: flex; flex-direction: column; gap: 2px; }
.spine nav a {
  display: block;
  padding: 9px 12px;
  border-radius: 10px;
  color: #ddd4c8;
  text-decoration: none;
}
.spine nav a:hover { background: rgba(255, 248, 242, 0.06); color: #fff; }
.spine nav a.active {
  background: rgba(255, 248, 242, 0.09);
  color: #fffaf6;
  box-shadow: inset 3px 0 0 #c4513a;
}
.spine-foot {
  margin: auto 0 0;
  padding: 0 12px;
  color: #8f877d;
  font-size: 12px;
  line-height: 1.6;
}
main { min-width: 0; }
.page { max-width: 900px; padding: 42px 48px 88px; }
.kicker {
  margin: 0 0 8px;
  color: var(--muted);
  font-size: 12px;
  letter-spacing: 0.22em;
}
h1 {
  margin: 0;
  font-family: var(--serif);
  font-size: 2.55rem;
  font-weight: 600;
  line-height: 1.12;
  letter-spacing: 0.08em;
}
h1::after {
  content: "";
  display: block;
  width: 36px;
  height: 2px;
  margin-top: 14px;
  background: var(--accent);
}
h2 {
  margin: 0;
  font-family: var(--serif);
  font-size: 1.35rem;
  font-weight: 600;
  letter-spacing: 0.04em;
}
.lead {
  margin: 16px 0 0;
  max-width: 66ch;
  color: var(--ink-soft);
}
.form, .composer {
  margin-top: 18px;
  padding: 18px;
  background: var(--card);
  border: 1px solid var(--line);
  border-radius: 18px;
  box-shadow: var(--shadow);
}
.form { display: grid; max-width: 38rem; }
label {
  margin: 14px 0 6px;
  color: var(--ink-soft);
  font-size: 13px;
  font-weight: 650;
}
.form > label:first-child, .composer > label:first-child { margin-top: 0; }
input, select, textarea, button { font: inherit; color: inherit; }
input, select, textarea {
  width: 100%;
  padding: 11px 12px;
  background: #fff;
  border: 1px solid var(--line-strong);
  border-radius: 11px;
}
input:hover, select:hover, textarea:hover { border-color: #b7aa98; }
input:focus, select:focus, textarea:focus {
  outline: none;
  border-color: var(--accent);
  box-shadow: 0 0 0 3px rgba(157, 59, 46, 0.16);
}
button[type="submit"] {
  justify-self: start;
  margin-top: 16px;
  padding: 11px 22px;
  border: 0;
  border-radius: 11px;
  background: var(--accent);
  color: #fffaf6;
  font-weight: 650;
  letter-spacing: 0.08em;
  cursor: pointer;
}
button[type="submit"]:hover { background: var(--accent-deep); }
button[type="submit"]:active { transform: translateY(1px); }
button[type="submit"]:focus-visible { outline: 2px solid var(--accent-deep); outline-offset: 3px; }
button[type="submit"]:disabled { cursor: progress; opacity: 0.6; }
.choice {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 14px;
  font-weight: 650;
}
.choice input { width: 1.05rem; height: 1.05rem; accent-color: var(--accent); }
.composer { max-width: none; }
.composer-row { display: flex; gap: 10px; align-items: stretch; }
.composer-row input {
  flex: 1;
  min-width: 0;
  height: 54px;
  padding: 0 16px;
  font-size: 17px;
}
.composer-row button {
  margin-top: 0;
  height: 54px;
  padding: 0 28px;
  font-size: 16px;
  white-space: nowrap;
}
dl.meta {
  display: grid;
  grid-template-columns: 8.5rem minmax(0, 1fr);
  gap: 8px 16px;
  margin: 14px 0 0;
  padding: 14px 16px;
  background: var(--card);
  border: 1px solid var(--line);
  border-radius: 14px;
}
.meta dt { margin: 0; color: var(--muted); font-size: 13px; font-weight: 650; }
.meta dd { margin: 0; overflow-wrap: anywhere; }
.panel {
  margin-top: 18px;
  padding: 16px 16px 8px;
  background: rgba(255, 253, 249, 0.62);
  border: 1px solid var(--line);
  border-radius: 18px;
}
.panel-head {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
  gap: 12px;
  margin-bottom: 6px;
}
#try-status:empty { display: none; }
#try-status { margin: 0; color: var(--ink-soft); font-size: 13px; font-variant-numeric: tabular-nums; }
.log-empty { margin: 2px 0 12px; color: var(--muted); font-size: 13.5px; }
#try-log {
  list-style: none;
  margin: 0;
  padding: 6px 2px 8px;
}
#try-log:empty { display: none; }
#try-log li {
  position: relative;
  display: grid;
  grid-template-columns: 18px minmax(0, 1fr);
  column-gap: 12px;
  padding-bottom: 16px;
}
#try-log li::before {
  content: "";
  position: absolute;
  left: 8px;
  top: 16px;
  bottom: 0;
  width: 1px;
  background: #ddd3c4;
}
#try-log li:last-child { padding-bottom: 8px; }
#try-log li:last-child::before { display: none; }
.mark {
  width: 11px;
  height: 11px;
  margin: 5px 0 0 3px;
  border: 2px solid #b7ad9f;
  border-radius: 50%;
  background: #fff;
  position: relative;
  z-index: 1;
}
#try-log li.done .mark { background: var(--ok); border-color: var(--ok); }
#try-log li.pending .mark {
  background: #fff;
  border-color: var(--accent);
  box-shadow: 0 0 0 4px rgba(157, 59, 46, 0.12);
}
.step-head { display: flex; justify-content: space-between; gap: 12px; align-items: baseline; }
.step-name { font-weight: 650; }
.step-state { color: var(--muted); font-size: 12px; letter-spacing: 0.06em; }
#try-log li.pending .step-state { color: var(--accent); font-weight: 650; }
#try-log li.done .step-state { color: var(--ok); }
.step-times {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 8px;
  margin-top: 8px;
}
.step-times span {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
  padding: 8px 10px;
  background: #f4efe6;
  border-radius: 9px;
}
.step-times em { font-style: normal; color: var(--muted); font-size: 11px; letter-spacing: 0.06em; }
.step-times b { font-weight: 650; font-size: 13px; font-variant-numeric: tabular-nums; overflow-wrap: anywhere; }
#try-note:empty { display: none; }
#try-note {
  margin: 14px 0 0;
  padding: 12px 14px;
  color: var(--warn);
  background: #f8f1e2;
  border: 1px solid #ead7b0;
  border-radius: 12px;
}
.err, #try-error {
  margin: 14px 0 0;
  padding: 12px 14px 12px 16px;
  color: var(--err);
  background: #f8ece8;
  border: 1px solid #e7c9c2;
  border-left: 3px solid var(--accent);
  border-radius: 12px;
}
#try-error:empty { display: none; }
.hit-list { display: grid; gap: 12px; margin-top: 14px; }
.hit {
  padding: 16px 16px 14px;
  background: var(--card);
  border: 1px solid var(--line);
  border-radius: 16px;
}
.hit-top { display: flex; gap: 14px; align-items: flex-start; }
.rank {
  flex: none;
  width: 1.8rem;
  color: var(--muted);
  font-family: var(--serif);
  font-size: 1.15rem;
  font-variant-numeric: tabular-nums;
}
.hit h3 {
  flex: 1;
  min-width: 0;
  margin: 0;
  font-family: var(--serif);
  font-size: 1.32rem;
  font-weight: 600;
  line-height: 1.35;
}
.score { flex: none; min-width: 4.6rem; text-align: right; }
.score span, .field {
  display: block;
  margin-bottom: 2px;
  color: var(--muted);
  font-size: 11px;
  letter-spacing: 0.1em;
}
.score strong {
  color: var(--accent);
  font-family: var(--serif);
  font-size: 1.5rem;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
  line-height: 1.1;
  white-space: nowrap;
}
.passage { margin: 12px 0 0; max-height: 8.4rem; overflow: auto; line-height: 1.75; overflow-wrap: anywhere; }
.hit blockquote {
  margin: 12px 0 0;
  max-height: 8.4rem;
  overflow: auto;
  padding: 10px 12px;
  background: #f4efe7;
  border-left: 3px solid #c9bba8;
  border-radius: 0 12px 12px 0;
  overflow-wrap: anywhere;
}
.hit footer { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 12px; }
.chip {
  display: inline-flex;
  align-items: center;
  padding: 3px 8px;
  border-radius: 999px;
  background: #f3eee6;
  color: var(--ink-soft);
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}
.empty-state {
  margin-top: 16px;
  padding: 28px 18px;
  text-align: center;
  color: var(--ink-soft);
  background: rgba(255, 253, 249, 0.7);
  border: 1px dashed var(--line-strong);
  border-radius: 16px;
}
.empty-state strong {
  display: block;
  margin-bottom: 4px;
  color: var(--ink);
  font-family: var(--serif);
  font-size: 1.28rem;
  font-weight: 600;
}
.empty-state p { margin: 0; }
.brief { margin-top: 32px; }
.brief h2 { margin-bottom: 12px; }
.facts {
  list-style: none;
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 10px;
  margin: 0;
  padding: 0;
}
.facts li {
  margin: 0;
  padding: 14px;
  color: var(--ink-soft);
  background: var(--card);
  border: 1px solid var(--line);
  border-radius: 14px;
  font-size: 13.5px;
  line-height: 1.7;
}
.facts li.span { grid-column: 1 / -1; }
.facts b {
  display: block;
  margin-bottom: 4px;
  color: var(--ink);
  font-size: 13px;
  letter-spacing: 0.06em;
}
.table-wrap {
  margin-top: 18px;
  overflow-x: auto;
  background: var(--card);
  border: 1px solid var(--line);
  border-radius: 14px;
}
table { width: 100%; min-width: 36rem; margin: 0; border-collapse: collapse; }
th, td {
  padding: 10px 12px;
  text-align: left;
  vertical-align: top;
  border-bottom: 1px solid var(--line);
  overflow-wrap: anywhere;
}
th {
  color: var(--muted);
  background: #f3eee6;
  font-size: 12px;
  font-weight: 650;
  letter-spacing: 0.04em;
}
tr:last-child td { border-bottom: 0; }
tbody tr:hover td { background: #fbf8f3; }
td.prose { white-space: pre-wrap; }
.report { margin-top: 26px; }
.report + .report { margin-top: 22px; }
.report p { margin: 8px 0 0; color: var(--ink-soft); }
.report .table-wrap { margin-top: 12px; }
@media (prefers-reduced-motion: no-preference) {
  #try-log li.pending .mark { animation: urag-pulse 1.6s ease-out infinite; }
}
@keyframes urag-pulse {
  0% { box-shadow: 0 0 0 0 rgba(157, 59, 46, 0.28); }
  100% { box-shadow: 0 0 0 8px rgba(157, 59, 46, 0); }
}
@media (max-width: 860px) {
  .app { grid-template-columns: 1fr; }
  .spine {
    position: sticky;
    top: 0;
    z-index: 4;
    height: auto;
    flex-direction: row;
    align-items: center;
    gap: 8px;
    padding: 10px 12px;
    overflow-x: auto;
  }
  .brand { flex: none; padding: 0; }
  .brand-text small, .spine-label, .spine-foot { display: none; }
  .seal { width: 32px; height: 32px; font-size: 17px; }
  .spine nav { flex-direction: row; gap: 4px; }
  .spine nav a { white-space: nowrap; padding: 7px 10px; }
  .spine nav a.active { box-shadow: inset 0 -2px 0 #c4513a; }
  .page { padding: 22px 16px 64px; }
  h1 { font-size: 2rem; letter-spacing: 0.06em; }
  .facts, .step-times, dl.meta { grid-template-columns: 1fr; }
  .meta dd { margin-bottom: 8px; }
  .composer-row { flex-direction: column; }
  .composer-row button, button[type="submit"] { width: 100%; }
  .hit-top { flex-wrap: wrap; }
  .score { text-align: left; }
}
@media (max-width: 420px) {
  h1 { font-size: 1.75rem; }
  .composer-row input { font-size: 16px; }
}
@media (prefers-reduced-motion: reduce) {
  *, *::before, *::after { animation: none !important; transition: none !important; }
}
</style>
</head>
<body>
<a class="skip" href="#page-{{.Page}}">跳到内容</a>
<div class="app">
<aside class="spine">
<a class="brand" href="/try">
<span class="seal" aria-hidden="true">查</span>
<span class="brand-text"><strong>urag</strong><small>检索试查</small></span>
</a>
<p class="spine-label">页面</p>
<nav aria-label="页面">
<a href="/upload"{{if eq .Page "upload"}} class="active" aria-current="page"{{end}}>上传</a>
<a href="/fetch"{{if eq .Page "fetch"}} class="active" aria-current="page"{{end}}>获取</a>
<a href="/clean"{{if eq .Page "clean"}} class="active" aria-current="page"{{end}}>清洗</a>
<a href="/ask"{{if eq .Page "ask"}} class="active" aria-current="page"{{end}}>提问</a>
<a href="/try"{{if eq .Page "try"}} class="active" aria-current="page"{{end}}>试查</a>
<a href="/eval"{{if eq .Page "eval"}} class="active" aria-current="page"{{end}}>评价</a>
</nav>
<p class="spine-foot">只返回片段<br>不生成回答</p>
</aside>
<main id="page-{{.Page}}">
<div class="page">
<p class="kicker">{{if eq .Page "try"}}公开语料{{else}}验收{{end}}</p>
<h1>{{.Title}}</h1>
{{if .Error}}<p class="err" role="alert">{{.Error}}</p>{{end}}
{{if eq .Page "upload"}}{{template "upload" .}}{{end}}
{{if eq .Page "fetch"}}{{template "fetch" .}}{{end}}
{{if eq .Page "clean"}}{{template "clean" .}}{{end}}
{{if eq .Page "ask"}}{{template "ask" .}}{{end}}
{{if eq .Page "try"}}{{template "try" .}}{{end}}
{{if eq .Page "eval"}}{{template "eval" .}}{{end}}
</div>
</main>
</div>
</body>
</html>{{end}}`

const uploadHTML = `{{define "upload"}}
<p class="lead">把一份文件送进指定集合。幂等键相同的再次提交不会另存一份。</p>
<form class="form" method="post" action="/upload" enctype="multipart/form-data">
<label for="collection">集合</label>
<input id="collection" name="collection" required autocomplete="off">
<label for="idempotency_key">幂等键</label>
<input id="idempotency_key" name="idempotency_key" autocomplete="off">
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
<p class="lead">从本地目录或 URL 建立一次获取任务，并在下面看到任务编号和状态。</p>
<form class="form" method="post" action="/fetch">
<label for="collection_id">集合</label>
<input id="collection_id" name="collection_id" value="{{.Fetch.Collection}}" required autocomplete="off">
<label for="kind">来源</label>
<select id="kind" name="kind">
<option value="local_dir" {{if eq .Fetch.Kind "local_dir"}}selected{{end}}>本地目录</option>
<option value="http" {{if eq .Fetch.Kind "http"}}selected{{end}}>URL</option>
</select>
<label for="uri">目录或 URL</label>
<input id="uri" name="uri" value="{{.Fetch.URI}}" required autocomplete="off">
<label for="idempotency_prefix">幂等前缀</label>
<input id="idempotency_prefix" name="idempotency_prefix" value="{{.Fetch.Prefix}}" autocomplete="off">
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
<p class="lead">按集合查看清洗后的文档、重复标记和正文。</p>
<form class="form" method="get" action="/clean">
<label for="collection">集合</label>
<input id="collection" name="collection" value="{{.Clean.Collection}}" required autocomplete="off">
<button type="submit">查看清洗结果</button>
</form>
{{with .Clean}}{{if .Documents}}
<div class="table-wrap">
<table>
<thead><tr><th>文档</th><th>重复</th><th>指向</th><th>正文</th></tr></thead>
<tbody>
{{range .Documents}}
<tr>
<td>{{.ID}}</td>
<td>{{.Duplicate}}</td>
<td>{{.Lineage.DuplicateOf}}</td>
<td class="prose">{{.Text}}</td>
</tr>
{{end}}
</tbody>
</table>
</div>
{{else if and .Collection (not $.Error)}}
<div class="empty-state">
<strong>没有文档</strong>
<p>这个集合还没有清洗结果。</p>
</div>
{{end}}{{end}}
{{end}}`

const askHTML = `{{define "ask"}}
<p class="lead">在点名的集合里提问。勾选混合后，同时使用向量和关键词。</p>
<form class="form" method="get" action="/ask">
<label for="collections">集合</label>
<input id="collections" name="collections" value="{{.Ask.Collections}}" placeholder="多个集合用逗号分开" required autocomplete="off">
<label for="query">问题</label>
<input id="query" name="query" value="{{.Ask.Query}}" required autocomplete="off">
<label for="top_k">返回条数</label>
<input id="top_k" name="top_k" value="{{.Ask.TopK}}" inputmode="numeric">
<label class="choice"><input type="checkbox" name="hybrid" value="1" {{if .Ask.Hybrid}}checked{{end}}>混合</label>
<button type="submit">提问</button>
</form>
{{with .Ask}}{{if .Hits}}
<div class="table-wrap">
<table>
<thead><tr><th>集合</th><th>文档</th><th>片段</th><th>分数</th><th>起点</th><th>终点</th><th>正文</th></tr></thead>
<tbody>
{{range .Hits}}
<tr>
<td>{{.CollectionID}}</td>
<td>{{.DocumentID}}</td>
<td>{{.ChunkID}}</td>
<td>{{.Score}}</td>
<td>{{.StartByte}}</td>
<td>{{.EndByte}}</td>
<td class="prose">{{.Text}}</td>
</tr>
{{end}}
</tbody>
</table>
</div>
{{else if and .Query (not $.Error)}}
<div class="empty-state">
<strong>没有命中</strong>
<p>这次没有返回片段。</p>
</div>
{{end}}{{end}}
{{end}}`

const tryHTML = `{{define "try"}}
<p class="lead">只查集合 {{.Try.Collection}}。向量检索读 Milvus 里的这个集合。BM25 走 Elasticsearch 索引 public。两边合并后留下前 50 条，再重排。</p>
<dl class="meta" id="try-rerank"{{if and (not .Try.RerankModel) (not .Try.RerankDir)}} hidden{{end}}>
{{if .Try.RerankModel}}<dt>这次重排模型</dt><dd>{{.Try.RerankModel}}</dd>{{end}}
{{if .Try.RerankDir}}<dt>这次重排目录</dt><dd>{{.Try.RerankDir}}</dd>{{end}}
</dl>
<form id="try-form" class="composer">
<label for="query">问题</label>
<div class="composer-row">
<input id="query" name="query" required autocomplete="off" placeholder="输入要查的句子" enterkeyhint="search">
<button type="submit" id="try-submit">试查</button>
</div>
</form>
<section class="panel" aria-labelledby="try-log-title">
<div class="panel-head">
<h2 id="try-log-title">过程</h2>
<p id="try-status" aria-live="polite"></p>
</div>
<p class="log-empty" id="try-log-empty">提交后，这里按实际顺序写出每一步。进行中的一步标成未完成，并写上开始时间；完成后补上结束时间和耗时。</p>
<ol id="try-log" aria-live="polite"></ol>
</section>
<p id="try-note" aria-live="polite"></p>
<p id="try-error" class="err" role="alert"></p>
<div id="try-result" aria-live="polite"></div>
<section class="brief" aria-label="检索说明">
<h2>这次怎么查</h2>
<ul class="facts">
<li><b>模型</b>嵌入模型在试查进程里只加载一次。第二次提问日志写「嵌入模型已在内存」，不再重新加载。重排仍每次加载，用完就退出。页面日志写出嵌入设备。</li>
<li><b>分数</b>拿到重排分后按分数从高到低返回最多 8 条，每条显示原始分。最高分低于 0.5 时仍显示这些命中，并写「最高分低于 0.5，仍显示前几条供查看」。没有重排分时按混合分返回最多 8 条，每条显示分数。</li>
<li><b>时间</b>下面按这次查询的实际顺序显示步骤。每一行有开始时间、结束时间和耗时（秒），时间是本机本地时间，精确到秒。进行中的一步有开始时间，没有结束时间，并标成未完成。</li>
<li id="try-rerank-config"><b>重排</b>重排目录由环境变量 URAG_RERANK_MODEL_DIR 指定。未设置时默认是嵌入目录旁边的 Qwen3-Reranker-4B。页面日志写出重排模型名，以及这一步的开始时间、结束时间和耗时。</li>
<li class="span"><b>超时</b>嵌入或重排超过三分钟还没有返回，就结束这次请求：检索超时。收录正在占用嵌入模型，这次没有结果。</li>
</ul>
</section>
<script>
(function () {
  var form = document.getElementById("try-form");
  var log = document.getElementById("try-log");
  var status = document.getElementById("try-status");
  var err = document.getElementById("try-error");
  var result = document.getElementById("try-result");
  var submit = document.getElementById("try-submit");
  var logEmpty = document.getElementById("try-log-empty");
  var items = {};
  function formatScore(value) {
    var raw = value == null ? "" : String(value);
    var num = Number(raw);
    if (raw === "" || !isFinite(num)) return raw;
    return (Math.round(num * 10000) / 10000).toFixed(4);
  }
  function cell(label, value) {
    var span = document.createElement("span");
    var name = document.createElement("em");
    name.textContent = label;
    var num = document.createElement("b");
    num.textContent = value;
    span.appendChild(name);
    span.appendChild(num);
    return span;
  }
  function upsert(step) {
    var li = items[step.name];
    if (!li) {
      li = document.createElement("li");
      items[step.name] = li;
      log.appendChild(li);
    }
    li.className = step.done ? "done" : "pending";
    li.textContent = "";
    var mark = document.createElement("span");
    mark.className = "mark";
    mark.setAttribute("aria-hidden", "true");
    var body = document.createElement("div");
    body.className = "step-body";
    var head = document.createElement("div");
    head.className = "step-head";
    var name = document.createElement("span");
    name.className = "step-name";
    name.textContent = step.name;
    var state = document.createElement("span");
    state.className = "step-state";
    state.textContent = step.done ? "完成" : "未完成";
    head.appendChild(name);
    head.appendChild(state);
    var times = document.createElement("div");
    times.className = "step-times";
    if (step.started) {
      times.appendChild(cell("开始", step.started));
    }
    if (step.ended) {
      times.appendChild(cell("结束", step.ended));
    }
    if (step.done && step.seconds != null) {
      times.appendChild(cell("耗时", step.seconds + " 秒"));
    }
    body.appendChild(head);
    body.appendChild(times);
    li.appendChild(mark);
    li.appendChild(body);
  }
  function showHits(hits, note) {
    result.textContent = "";
    result.className = "";
    var noteEl = document.getElementById("try-note");
    if (noteEl) {
      noteEl.textContent = note || "";
    }
    if (!hits || !hits.length) {
      status.textContent = note ? "命中 0 条" : "没有足够相关的片段";
      result.className = "empty-state";
      var title = document.createElement("strong");
      title.textContent = status.textContent;
      var hint = document.createElement("p");
      hint.textContent = note ? "这次没有可展示的片段。" : "可以换一种问法再试一次。";
      result.appendChild(title);
      result.appendChild(hint);
      return;
    }
    status.textContent = "命中 " + hits.length + " 条";
    result.className = "hit-list";
    hits.forEach(function (hit, index) {
      var card = document.createElement("article");
      card.className = "hit";
      var top = document.createElement("div");
      top.className = "hit-top";
      var rank = document.createElement("span");
      rank.className = "rank";
      rank.textContent = ("0" + String(index + 1)).slice(-2);
      var heading = document.createElement("h3");
      heading.textContent = hit.heading_path || "未标路径";
      var score = document.createElement("p");
      score.className = "score";
      var scoreName = document.createElement("span");
      scoreName.textContent = "原始分";
      var scoreVal = document.createElement("strong");
      scoreVal.title = hit.score == null ? "" : String(hit.score);
      scoreVal.textContent = formatScore(hit.score);
      score.appendChild(scoreName);
      score.appendChild(scoreVal);
      top.appendChild(rank);
      top.appendChild(heading);
      top.appendChild(score);
      card.appendChild(top);
      if (hit.parent) {
        var passage = document.createElement("p");
        passage.className = "passage";
        var parentName = document.createElement("span");
        parentName.className = "field";
        parentName.textContent = "父块";
        passage.appendChild(parentName);
        passage.appendChild(document.createTextNode(hit.parent));
        card.appendChild(passage);
      }
      if (hit.chunk) {
        var quote = document.createElement("blockquote");
        var chunkName = document.createElement("span");
        chunkName.className = "field";
        chunkName.textContent = "子块";
        quote.appendChild(chunkName);
        quote.appendChild(document.createTextNode(hit.chunk));
        card.appendChild(quote);
      }
      var foot = document.createElement("footer");
      var col = document.createElement("span");
      col.className = "chip";
      col.textContent = "集合 " + (hit.collection_id || "");
      var pos = document.createElement("span");
      pos.className = "chip";
      pos.textContent = "原文位置 " + String(hit.start_byte) + "–" + String(hit.end_byte);
      foot.appendChild(col);
      foot.appendChild(pos);
      card.appendChild(foot);
      result.appendChild(card);
    });
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
      if (logEmpty) {
        logEmpty.hidden = true;
      }
      upsert(payload);
      return;
    }
    if (eventName === "result") {
      showHits(payload.hits || [], payload.note || "");
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
    result.className = "";
    status.textContent = "检索中";
    var noteEl = document.getElementById("try-note");
    if (noteEl) {
      noteEl.textContent = "";
    }
    log.textContent = "";
    items = {};
    if (logEmpty) {
      logEmpty.hidden = true;
    }
    submit.disabled = true;
    submit.textContent = "检索中";
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
      submit.disabled = false;
      submit.textContent = "试查";
    });
  });
})();
</script>
{{end}}`

const evalHTML = `{{define "eval"}}
<p class="lead">用夹具比较正常切分和对照切分的 Recall 与 MRR。</p>
<form class="form" method="post" action="/eval">
<button type="submit">运行评价</button>
</form>
{{with .Eval}}{{if .Ready}}
<section class="report">
<h2>正常切分</h2>
<p>方向 {{.Normal.Direction}}，Recall 差 {{.Normal.RecallDelta}}，MRR 差 {{.Normal.MRRDelta}}</p>
<div class="table-wrap">
{{template "scores" .Normal.Scores}}
</div>
</section>
<section class="report">
<h2>对照切分</h2>
<p>方向 {{.Contrast.Direction}}，Recall 差 {{.Contrast.RecallDelta}}，MRR 差 {{.Contrast.MRRDelta}}</p>
<div class="table-wrap">
{{template "scores" .Contrast.Scores}}
</div>
</section>
{{else if not $.Error}}
<div class="empty-state">
<strong>还没有结果</strong>
<p>运行评价后，两套切分的分数会出现在这里。</p>
</div>
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
