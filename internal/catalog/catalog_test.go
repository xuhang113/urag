package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDuplicateSourceURIReturnsExistingDoc(t *testing.T) {
	st := NewMemory()
	mem := memoryOf(t, st)
	ctx := context.Background()
	in := sampleDocument("https://example.invalid/a", "旧标题", "one")
	in.DocID = 99
	in.Version = 3

	id1, err := st.PutDocument(ctx, in)
	if err != nil {
		t.Fatalf("首次写入：%v", err)
	}
	if id1 != 1 || id1 == in.DocID {
		t.Fatalf("doc_id = %d，应由存储生成", id1)
	}

	again := in
	again.DocID = 100
	again.Version = 9
	again.Title = "另一标题"
	id2, err := st.PutDocument(ctx, again)
	if err != nil {
		t.Fatalf("重复写入：%v", err)
	}
	if id2 != id1 {
		t.Fatalf("重复 source_uri 产生了另一个 doc_id：%d 和 %d", id1, id2)
	}
	if len(mem.docs) != 1 {
		t.Fatalf("source_document 行数 = %d，want 1", len(mem.docs))
	}
	if mem.docs[id1].Version != 3 || mem.docs[id1].Title != "旧标题" {
		t.Fatalf("校验和相同时改写了原行：%+v", mem.docs[id1])
	}

	other := in
	other.DocID = 0
	other.CollectionID = "beta"
	id3, err := st.PutDocument(ctx, other)
	if err != nil {
		t.Fatalf("另一个集合：%v", err)
	}
	if id3 == id1 || len(mem.docs) != 2 {
		t.Fatalf("另一个集合应是另一行，id=%d 行数=%d", id3, len(mem.docs))
	}
	alpha, err := st.CountIndexed(ctx, "alpha")
	if err != nil || alpha != 1 {
		t.Fatalf("alpha CountIndexed = %d, %v", alpha, err)
	}
	beta, err := st.CountIndexed(ctx, "beta")
	if err != nil || beta != 1 {
		t.Fatalf("beta CountIndexed = %d, %v", beta, err)
	}
}

func TestGeneratedIDsAndChunkFormat(t *testing.T) {
	st := NewMemory()
	ctx := context.Background()
	docID, err := st.PutDocument(ctx, sampleDocument("https://example.invalid/chunks", "标题", "body"))
	if err != nil {
		t.Fatalf("PutDocument：%v", err)
	}
	if _, err := st.PutParent(ctx, Parent{ParentID: 7, DocID: 404, Body: "不存在"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("缺失文档的父块 err = %v", err)
	}

	parentID, err := st.PutParent(ctx, Parent{
		ParentID:    7,
		DocID:       docID,
		HeadingPath: "章/节",
		PageFrom:    2,
		PageTo:      5,
		Body:        "整节正文",
	})
	if err != nil {
		t.Fatalf("PutParent：%v", err)
	}
	if parentID != 1 || parentID == 7 {
		t.Fatalf("parent_id = %d，应由存储生成", parentID)
	}

	if err := st.PutChild(ctx, Child{ChunkID: "nope", ParentID: 404, DocID: docID, Ordinal: 0}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("缺失父块的子块 err = %v", err)
	}
	for _, ordinal := range []int{2, 0} {
		err := st.PutChild(ctx, Child{
			ChunkID:   "nope",
			ParentID:  parentID,
			DocID:     docID,
			Ordinal:   ordinal,
			StartByte: ordinal,
			EndByte:   ordinal + 1,
			Page:      3,
			Body:      strconv.Itoa(ordinal),
		})
		if err != nil {
			t.Fatalf("PutChild %d：%v", ordinal, err)
		}
	}
	if err := st.PutChild(ctx, Child{ParentID: parentID, DocID: docID, Ordinal: 0, Body: "重复"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复序号 err = %v", err)
	}

	kids, err := st.ListChildren(ctx, docID)
	if err != nil {
		t.Fatalf("ListChildren：%v", err)
	}
	if len(kids) != 2 || kids[0].Ordinal != 0 || kids[1].Ordinal != 2 {
		t.Fatalf("子块顺序 = %#v", kids)
	}
	for _, kid := range kids {
		want := strconv.FormatInt(docID, 10) + "#" + strconv.Itoa(kid.Ordinal)
		if kid.ChunkID != want || kid.ChunkID == "nope" {
			t.Fatalf("chunk_id = %s，want %s", kid.ChunkID, want)
		}
		if kid.ParentID != parentID || kid.DocID != docID {
			t.Fatalf("子块没有引用生成的编号：%+v", kid)
		}
	}
	if _, err := st.ListChildren(ctx, 404); !errors.Is(err, ErrNotFound) {
		t.Fatalf("缺失文档的子块列表 err = %v", err)
	}
}

func TestGetParentReturnsSectionBody(t *testing.T) {
	st := NewMemory()
	ctx := context.Background()
	docID, err := st.PutDocument(ctx, sampleDocument("https://example.invalid/section", "标题", "section"))
	if err != nil {
		t.Fatalf("PutDocument：%v", err)
	}
	body := "第一节\n" + strings.Repeat("甲乙丙丁。", 800)
	parentID, err := st.PutParent(ctx, Parent{
		DocID:       docID,
		HeadingPath: "卷一/第一节",
		PageFrom:    10,
		PageTo:      12,
		Body:        body,
	})
	if err != nil {
		t.Fatalf("PutParent：%v", err)
	}
	got, err := st.GetParent(ctx, parentID)
	if err != nil {
		t.Fatalf("GetParent：%v", err)
	}
	if got.ParentID != parentID || got.DocID != docID || got.HeadingPath != "卷一/第一节" || got.PageFrom != 10 || got.PageTo != 12 {
		t.Fatalf("父块字段 = %+v", got)
	}
	if got.Body != body {
		t.Fatalf("正文长度 %d，want %d", len(got.Body), len(body))
	}
	if _, err := st.GetParent(ctx, parentID+99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("缺失父块 err = %v", err)
	}
}

func TestChecksumChangeKeepsDocAndBumpsVersion(t *testing.T) {
	st := NewMemory()
	mem := memoryOf(t, st)
	ctx := context.Background()
	in := sampleDocument("https://example.invalid/rev", "旧标题", "v1")
	in.Version = 3
	docID, err := st.PutDocument(ctx, in)
	if err != nil {
		t.Fatalf("PutDocument：%v", err)
	}
	parentID, err := st.PutParent(ctx, Parent{DocID: docID, Body: "旧节"})
	if err != nil {
		t.Fatalf("PutParent：%v", err)
	}
	if err := st.PutChild(ctx, Child{ParentID: parentID, DocID: docID, Ordinal: 0, Body: "旧块"}); err != nil {
		t.Fatalf("PutChild：%v", err)
	}

	changed := in
	changed.Version = 100
	changed.Title = "新标题"
	changed.Checksum = sumText("v2")
	changed.FetchedAt = time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	id2, err := st.PutDocument(ctx, changed)
	if err != nil {
		t.Fatalf("校验和变化：%v", err)
	}
	if id2 != docID || len(mem.docs) != 1 {
		t.Fatalf("校验和变化后 doc_id=%d 行数=%d", id2, len(mem.docs))
	}
	got := mem.docs[docID]
	if got.Version != 4 || got.Title != "新标题" || got.Checksum != changed.Checksum {
		t.Fatalf("版本或标题 = %+v", got)
	}
	if !got.FetchedAt.Equal(changed.FetchedAt) {
		t.Fatalf("fetched_at = %s", got.FetchedAt)
	}
	kids, err := st.ListChildren(ctx, docID)
	if err != nil || len(kids) != 1 || kids[0].Body != "旧块" {
		t.Fatalf("校验和变化不应删掉子块：%#v %v", kids, err)
	}
	n, err := st.CountIndexed(ctx, in.CollectionID)
	if err != nil || n != 1 {
		t.Fatalf("CountIndexed = %d, %v", n, err)
	}
}

func TestReplaceSectionsSwapsInOneStep(t *testing.T) {
	st := NewMemory()
	ctx := context.Background()
	docID, err := st.PutDocument(ctx, sampleDocument("https://example.invalid/replace", "标题", "replace"))
	if err != nil {
		t.Fatalf("PutDocument：%v", err)
	}
	oldID, err := st.PutParent(ctx, Parent{DocID: docID, Body: "旧节"})
	if err != nil {
		t.Fatalf("PutParent：%v", err)
	}
	if err := st.PutChild(ctx, Child{ParentID: oldID, DocID: docID, Ordinal: 0, Body: "旧块"}); err != nil {
		t.Fatalf("PutChild：%v", err)
	}

	err = st.ReplaceSections(ctx, docID, []ParentWithChildren{{
		Parent:   Parent{Body: "半成品"},
		Children: []Child{{Ordinal: 1, Body: "甲"}, {Ordinal: 1, Body: "乙"}},
	}})
	if err == nil {
		t.Fatal("重复序号应失败")
	}
	old, err := st.GetParent(ctx, oldID)
	if err != nil || old.Body != "旧节" {
		t.Fatalf("失败的替换改掉了旧节：%+v %v", old, err)
	}

	err = st.ReplaceSections(ctx, docID, []ParentWithChildren{{
		Parent: Parent{HeadingPath: "章/新节", PageFrom: 2, PageTo: 4, Body: "新节"},
		Children: []Child{
			{Ordinal: 1, StartByte: 3, EndByte: 6, Page: 4, HeadingPath: "章/新节", Body: "节"},
			{ChunkID: "bad", Ordinal: 0, StartByte: 0, EndByte: 3, Body: "新"},
		},
	}})
	if err != nil {
		t.Fatalf("ReplaceSections：%v", err)
	}
	if _, err := st.GetParent(ctx, oldID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧父块仍在：%v", err)
	}
	kids, err := st.ListChildren(ctx, docID)
	if err != nil {
		t.Fatalf("ListChildren：%v", err)
	}
	if len(kids) != 2 || kids[0].Ordinal != 0 || kids[1].Ordinal != 1 {
		t.Fatalf("新子块 = %#v", kids)
	}
	if kids[0].ParentID == oldID || kids[0].ParentID != kids[1].ParentID || kids[0].ParentID == 0 {
		t.Fatalf("新子块应引用新的 parent_id：%#v", kids)
	}
	for _, kid := range kids {
		want := strconv.FormatInt(docID, 10) + "#" + strconv.Itoa(kid.Ordinal)
		if kid.ChunkID != want || kid.DocID != docID {
			t.Fatalf("chunk_id = %s doc = %d", kid.ChunkID, kid.DocID)
		}
	}
	fresh, err := st.GetParent(ctx, kids[0].ParentID)
	if err != nil || fresh.Body != "新节" || fresh.DocID != docID {
		t.Fatalf("新节 = %+v %v", fresh, err)
	}

	if err := st.ReplaceSections(ctx, docID, nil); err != nil {
		t.Fatalf("清空段落：%v", err)
	}
	kids, err = st.ListChildren(ctx, docID)
	if err != nil || len(kids) != 0 {
		t.Fatalf("清空后的子块 %#v %v", kids, err)
	}
	n, err := st.CountIndexed(ctx, "alpha")
	if err != nil || n != 1 {
		t.Fatalf("清空段落后 CountIndexed = %d, %v", n, err)
	}
	if err := st.ReplaceSections(ctx, docID+50, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("缺失文档 err = %v", err)
	}
}

func TestCountIndexedCountsSourceRows(t *testing.T) {
	st := NewMemory()
	ctx := context.Background()
	if n, err := st.CountIndexed(ctx, "alpha"); err != nil || n != 0 {
		t.Fatalf("空集合 = %d, %v", n, err)
	}
	if _, err := st.PutDocument(ctx, sampleDocument("https://example.invalid/bare", "无子块", "bare")); err != nil {
		t.Fatalf("PutDocument：%v", err)
	}
	if _, err := st.PutDocument(ctx, sampleDocument("https://example.invalid/other", "另一篇", "other")); err != nil {
		t.Fatalf("第二篇：%v", err)
	}
	beta := sampleDocument("https://example.invalid/bare", "别的集合", "beta")
	beta.CollectionID = "beta"
	if _, err := st.PutDocument(ctx, beta); err != nil {
		t.Fatalf("beta：%v", err)
	}
	n, err := st.CountIndexed(ctx, "alpha")
	if err != nil || n != 2 {
		t.Fatalf("alpha = %d, %v，无子块的来源也应计入", n, err)
	}
}

func TestSchemaHasNoObjectBytesOrVectors(t *testing.T) {
	stmts := splitSQL(schemaSQL)
	if len(stmts) != 3 {
		t.Fatalf("建表语句 %d 条，want 3", len(stmts))
	}
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS source_document",
		"CREATE TABLE IF NOT EXISTS parent_section",
		"CREATE TABLE IF NOT EXISTS child_chunk",
		"doc_id BIGINT NOT NULL AUTO_INCREMENT",
		"parent_id BIGINT NOT NULL AUTO_INCREMENT",
		"chunk_id VARCHAR(64) NOT NULL",
		"PRIMARY KEY (chunk_id)",
		"UNIQUE KEY uk_collection_source (collection_id, source_uri)",
		"UNIQUE KEY uk_doc_ordinal (doc_id, ordinal)",
	} {
		if !strings.Contains(schemaSQL, want) {
			t.Fatalf("schema.sql 缺少 %s", want)
		}
	}
	if strings.Contains(schemaSQL, "chunk_id VARCHAR(64) NOT NULL AUTO_INCREMENT") {
		t.Fatal("chunk_id 不应自动递增")
	}
	if strings.Contains(strings.ToLower(insertDocumentSQL), "doc_id") {
		t.Fatal("插入来源时不应写入 doc_id")
	}
	if strings.Contains(strings.ToLower(insertParentSQL), "parent_id") {
		t.Fatal("插入父块时不应写入 parent_id")
	}
	if !strings.Contains(updateDocumentSQL, "version = version + 1") {
		t.Fatal("校验和变化时应在数据库里把 version 加 1")
	}
	joined := strings.ToLower(strings.Join([]string{
		schemaSQL, insertDocumentSQL, updateDocumentSQL, insertParentSQL, insertChildSQL, selectParentSQL, selectChildrenSQL,
	}, "\n"))
	for _, word := range []string{"object_key", "vector", "embedding", "blob"} {
		if strings.Contains(joined, word) {
			t.Fatalf("台账语句包含 %s", word)
		}
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(Document{}), reflect.TypeOf(Parent{}), reflect.TypeOf(Child{})} {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if field.Type == reflect.TypeOf([]byte(nil)) {
				t.Fatalf("%s.%s 是字节切片", typ.Name(), field.Name)
			}
		}
	}
}

func TestMySQLDSNNameStaysInComments(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir：%v", err)
	}
	sawName := false
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s：%v", name, err)
		}
		if strings.Contains(string(body), "URAG_MYSQL_DSN") {
			sawName = true
		}
		file, err := parser.ParseFile(fset, name, body, 0)
		if err != nil {
			t.Fatalf("解析 %s：%v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if strings.Contains(lit.Value, "URAG_MYSQL_DSN") || strings.Contains(lit.Value, "@tcp") {
				t.Fatalf("%s 在代码里写了 DSN 或变量名：%s", name, lit.Value)
			}
			return true
		})
	}
	if !sawName {
		t.Fatal("注释里应写出 URAG_MYSQL_DSN")
	}
	spec, err := os.ReadFile(repoFile(t, "specs", "catalog.md"))
	if err != nil {
		t.Fatalf("读取规格：%v", err)
	}
	if !strings.Contains(string(spec), "URAG_MYSQL_DSN") || !strings.Contains(string(spec), "fake-password") {
		t.Fatal("规格示例应使用假的 DSN")
	}
}

func TestNewMySQLRejectsEmptyDSN(t *testing.T) {
	db, err := NewMySQL("   ")
	if err == nil {
		_ = db.Close()
		t.Fatal("空 DSN 应返回错误")
	}
	db, err = NewMySQL("not-a-dsn")
	if err == nil {
		_ = db.Close()
		t.Fatal("无法解析的 DSN 应返回错误")
	}
}

func TestContextCancelSkipsWrite(t *testing.T) {
	st := NewMemory()
	mem := memoryOf(t, st)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.PutDocument(ctx, sampleDocument("https://example.invalid/cancel", "标题", "cancel")); !errors.Is(err, context.Canceled) {
		t.Fatalf("PutDocument err = %v", err)
	}
	if len(mem.docs) != 0 {
		t.Fatal("取消后仍写入了来源")
	}
}

func TestRejectsBadChecksum(t *testing.T) {
	st := NewMemory()
	mem := memoryOf(t, st)
	ctx := context.Background()
	bad := sampleDocument("https://example.invalid/bad", "标题", "bad")
	bad.Checksum = "ABC"
	if _, err := st.PutDocument(ctx, bad); err == nil {
		t.Fatal("大写或过短的校验和应被拒绝")
	}
	longURI := sampleDocument("https://example.invalid/"+strings.Repeat("a", 500), "标题", "long")
	if _, err := st.PutDocument(ctx, longURI); err == nil {
		t.Fatal("过长的 source_uri 应被拒绝")
	}
	if len(mem.docs) != 0 {
		t.Fatalf("非法写入产生了 %d 行", len(mem.docs))
	}
}

func TestChunkIDUsesDecimalOrdinal(t *testing.T) {
	if got, want := ChunkID(12, 0), "12#0"; got != want {
		t.Fatalf("ChunkID(12, 0) = %s", got)
	}
	want := strconv.FormatInt(12, 10) + "#" + strconv.Itoa(3)
	if got := ChunkID(12, 3); got != want {
		t.Fatalf("ChunkID(12, 3) = %s，want %s", got, want)
	}
}

func sampleDocument(uri, title, text string) Document {
	return Document{
		CollectionID: "alpha",
		Version:      1,
		SourceURI:    uri,
		Checksum:     sumText(text),
		MediaType:    "txt",
		License:      "CC-BY-4.0",
		Title:        title,
		FetchedAt:    time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC),
	}
}

func sumText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func memoryOf(t *testing.T, st Store) *memoryStore {
	t.Helper()
	mem, ok := st.(*memoryStore)
	if !ok {
		t.Fatalf("NewMemory 的类型是 %T", st)
	}
	return mem
}

func repoFile(t *testing.T, parts ...string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	segs := []string{filepath.Dir(file), "..", ".."}
	segs = append(segs, parts...)
	return filepath.Join(segs...)
}
