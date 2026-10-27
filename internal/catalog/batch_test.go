package catalog

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestLedgerTablesIndexes(t *testing.T) {
	if strings.Count(schemaSQL, "CREATE TABLE IF NOT EXISTS") != 3 {
		t.Fatal("表的数量变了")
	}
	if strings.Count(schemaSQL, "ENGINE=InnoDB") != 3 {
		t.Fatal("三张表都应保持 InnoDB")
	}
	for _, want := range []string{
		"doc_id BIGINT NOT NULL AUTO_INCREMENT",
		"collection_id VARCHAR(128) NOT NULL",
		"version INT NOT NULL",
		"source_uri VARCHAR(512) NOT NULL",
		"checksum CHAR(64) NOT NULL",
		"media_type VARCHAR(128) NOT NULL",
		"license VARCHAR(128) NOT NULL",
		"title VARCHAR(512) NOT NULL",
		"fetched_at DATETIME(6) NULL",
		"parent_id BIGINT NOT NULL AUTO_INCREMENT",
		"page_from INT NOT NULL",
		"page_to INT NOT NULL",
		"chunk_id VARCHAR(64) NOT NULL",
		"ordinal INT NOT NULL",
		"start_byte INT NOT NULL",
		"end_byte INT NOT NULL",
		"page INT NOT NULL",
		"PRIMARY KEY (doc_id)",
		"PRIMARY KEY (parent_id)",
		"PRIMARY KEY (chunk_id)",
		"UNIQUE KEY uk_collection_source (collection_id, source_uri)",
		"KEY idx_parent_doc (doc_id)",
		"UNIQUE KEY uk_doc_ordinal (doc_id, ordinal)",
		"KEY idx_child_parent (parent_id)",
	} {
		if strings.Count(schemaSQL, want) != 1 {
			t.Fatalf("%s 出现 %d 次", want, strings.Count(schemaSQL, want))
		}
	}
	// 父块和子块都保存清洗后的路径和正文。doc_id 在这两张表里不是自增。
	if strings.Count(schemaSQL, "heading_path TEXT NOT NULL") != 2 || strings.Count(schemaSQL, "body MEDIUMTEXT NOT NULL") != 2 {
		t.Fatal("父块或子块少了路径或正文")
	}
	if strings.Count(schemaSQL, "doc_id BIGINT NOT NULL,") != 2 {
		t.Fatalf("非自增 doc_id 列数不对")
	}
	if strings.Count(schemaSQL, "parent_id BIGINT NOT NULL,") != 1 {
		t.Fatalf("子块 parent_id 列数不对")
	}
	// 按文档删除和按 parent 读取已经有索引，不再加同名二级索引。
	if strings.Count(schemaSQL, "KEY idx_") != 2 {
		t.Fatalf("二级索引数量 = %d", strings.Count(schemaSQL, "KEY idx_"))
	}
}

func TestSourceDoesNotDisableDurability(t *testing.T) {
	for _, name := range []string{"mysql.go", "batch.go", "schema.sql"} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s：%v", name, err)
		}
		low := strings.ToLower(string(body))
		for _, word := range []string{
			"foreign_key_checks",
			"unique_checks",
			"innodb_flush_log_at_trx_commit",
			"sql_log_bin",
			"engine=myisam",
		} {
			if strings.Contains(low, word) {
				t.Fatalf("%s 包含 %s", name, word)
			}
		}
	}
}

func TestWriteBatchRowLimit(t *testing.T) {
	if writeBatchRows < 100 || writeBatchRows > 200 {
		t.Fatalf("批量行数 %d，应在 100 到 200 之间", writeBatchRows)
	}
	const n = 5000
	groups := make([]ParentWithChildren, n)
	for i := range groups {
		groups[i].Parent = Parent{HeadingPath: "h", Body: "p"}
		groups[i].Children = []Child{{Ordinal: i, HeadingPath: "h", Body: "c"}}
	}
	parents := parentBatches(9, groups)
	children, err := childBatches(9, groups, seqIDs(n))
	if err != nil {
		t.Fatalf("childBatches：%v", err)
	}
	assertBatchShape(t, parents, n, parentArgWidth)
	assertBatchShape(t, children, n, childArgWidth)
	if _, err := childBatches(9, groups, nil); err == nil {
		t.Fatal("父块编号数量不一致时应失败")
	}
}

func TestWriteBatchByteLimit(t *testing.T) {
	huge := strings.Repeat("a", writeBatchBytes)
	groups := []ParentWithChildren{
		{
			Parent:   Parent{HeadingPath: "h", Body: huge},
			Children: []Child{{Ordinal: 0, HeadingPath: "h", Body: huge}},
		},
		{
			Parent:   Parent{HeadingPath: "h2", Body: "b"},
			Children: []Child{{Ordinal: 1, HeadingPath: "h2", Body: "c"}},
		},
	}
	parents := parentBatches(3, groups)
	if len(parents) != 2 || parents[0].rows != 1 || parents[1].rows != 1 {
		t.Fatalf("大父块应拆成两批，得到 %d 批", len(parents))
	}
	children, err := childBatches(3, groups, []int64{8, 9})
	if err != nil {
		t.Fatalf("childBatches：%v", err)
	}
	if len(children) != 2 || children[0].rows != 1 || children[1].rows != 1 {
		t.Fatalf("大子块应拆成两批，得到 %d 批", len(children))
	}
	marker := strings.Repeat("a", 8)
	for _, batch := range append(parents, children...) {
		if strings.Contains(batch.query, marker) {
			t.Fatal("正文被拼进了 SQL")
		}
		if strings.Count(batch.query, "?") != len(batch.args) {
			t.Fatalf("占位符 %d，参数 %d", strings.Count(batch.query, "?"), len(batch.args))
		}
	}
	if parents[0].args[4] != huge || children[0].args[8] != huge {
		t.Fatal("正文应留在参数里")
	}
	if children[0].args[1] != int64(8) || children[1].args[1] != int64(9) {
		t.Fatalf("父块编号 %v %v", children[0].args[1], children[1].args[1])
	}
}

func TestBatchInsertColumns(t *testing.T) {
	groups := []ParentWithChildren{{
		Parent: Parent{HeadingPath: "章", PageFrom: 2, PageTo: 4, Body: "父"},
		Children: []Child{{
			ChunkID:     "caller",
			ParentID:    99,
			DocID:       7,
			Ordinal:     3,
			StartByte:   1,
			EndByte:     5,
			HeadingPath: "章",
			Page:        4,
			Body:        "it's a ?",
		}},
	}}
	parents := parentBatches(11, groups)
	if len(parents) != 1 {
		t.Fatalf("父块批次 %d", len(parents))
	}
	head, _, ok := strings.Cut(parents[0].query, "VALUES")
	if !ok || strings.Contains(head, "parent_id") {
		t.Fatal("批量插入父块不应写入 parent_id")
	}
	if !strings.Contains(head, "doc_id, heading_path, page_from, page_to, body") {
		t.Fatal("父块列与单行插入不一致")
	}
	children, err := childBatches(11, groups, []int64{20})
	if err != nil || len(children) != 1 {
		t.Fatalf("子块批次 %v %d", err, len(children))
	}
	chead, _, ok := strings.Cut(children[0].query, "VALUES")
	if !ok || !strings.Contains(chead, "chunk_id, parent_id, doc_id, ordinal, start_byte, end_byte, heading_path, page, body") {
		t.Fatal("子块列与单行插入不一致")
	}
	if strings.Contains(children[0].query, "it's") || strings.Contains(children[0].query, "caller") {
		t.Fatal("子块的值进了 SQL")
	}
	args := children[0].args
	if args[0] != "11#3" || args[1] != int64(20) || args[2] != int64(11) || args[3] != 3 || args[8] != "it's a ?" {
		t.Fatalf("子块参数 %#v", args)
	}
}

func TestExpandIDs(t *testing.T) {
	got, err := expandIDs(10, 2, 3)
	if err != nil || !reflect.DeepEqual(got, []int64{10, 12, 14}) {
		t.Fatalf("expand = %v %v", got, err)
	}
	if got, err := expandIDs(4, 1, 1); err != nil || !reflect.DeepEqual(got, []int64{4}) {
		t.Fatalf("单行 %v %v", got, err)
	}
	for _, tc := range []struct {
		first, step int64
		n           int
	}{
		{0, 1, 1},
		{1, 0, 1},
		{1, 1, 0},
		{math.MaxInt64, 1, 2},
	} {
		if _, err := expandIDs(tc.first, tc.step, tc.n); err == nil {
			t.Fatalf("应失败 %+v", tc)
		}
	}
}

func TestReplaceSectionsOneTransaction(t *testing.T) {
	fc := newScriptConn()
	m := openScriptMySQL(t, fc)
	docID := int64(42)
	nParents := writeBatchRows + 1
	nChildren := writeBatchRows + 2
	groups := make([]ParentWithChildren, nParents)
	kids := make([]Child, nChildren)
	for i := range kids {
		kids[i] = Child{
			ChunkID:     "caller",
			ParentID:    99,
			Ordinal:     i,
			StartByte:   i,
			EndByte:     i + 1,
			HeadingPath: "sec",
			Page:        3,
			Body:        "body-" + strconv.Itoa(i),
		}
	}
	kids[nChildren-1].Body = "it's a ?"
	for i := range groups {
		groups[i].Parent = Parent{
			HeadingPath: "h-" + strconv.Itoa(i),
			PageFrom:    1,
			PageTo:      2,
			Body:        "p-" + strconv.Itoa(i),
		}
	}
	groups[0].Children = kids

	if err := m.ReplaceSections(context.Background(), docID, groups); err != nil {
		t.Fatalf("ReplaceSections：%v", err)
	}
	want := []string{"begin", "lock", "delete-children", "delete-parents", "step"}
	want = append(want, repeatName("insert-parents", (nParents+writeBatchRows-1)/writeBatchRows)...)
	want = append(want, "read-parents")
	want = append(want, repeatName("insert-children", (nChildren+writeBatchRows-1)/writeBatchRows)...)
	want = append(want, "commit")
	if !reflect.DeepEqual(eventNames(fc), want) {
		t.Fatalf("事件 = %v", eventNames(fc))
	}
	if strings.Count(strings.Join(eventNames(fc), " "), "commit") != 1 {
		t.Fatal("应只提交一次")
	}
	assertDocArgs(t, fc, docID)
	parents := eventsNamed(fc, "insert-parents")
	if parents[0].rows() != writeBatchRows || parents[1].rows() != 1 {
		t.Fatalf("父块批次行数 %d %d", parents[0].rows(), parents[1].rows())
	}
	if strArg(t, parents[0].args[1]) != "h-0" || strArg(t, parents[1].args[1]) != "h-"+strconv.Itoa(writeBatchRows) {
		t.Fatal("父块批次没有按顺序切开")
	}
	children := eventsNamed(fc, "insert-children")
	if children[0].rows() != writeBatchRows || children[1].rows() != 2 {
		t.Fatalf("子块批次行数 %d %d", children[0].rows(), children[1].rows())
	}
	seen := 0
	for _, ev := range children {
		if strings.Contains(ev.query, "body-") || strings.Contains(ev.query, "it's") || strings.Contains(ev.query, "caller") {
			t.Fatal("子块的值进了 SQL")
		}
		for row := 0; row < ev.rows(); row++ {
			base := row * childArgWidth
			if strArg(t, ev.args[base]) != ChunkID(docID, seen) {
				t.Fatalf("chunk_id %v", ev.args[base])
			}
			if intArg(t, ev.args[base+1]) != 1 || intArg(t, ev.args[base+2]) != docID {
				t.Fatalf("引用 %v", ev.args[base:base+3])
			}
			if intArg(t, ev.args[base+3]) != int64(seen) {
				t.Fatalf("序号 %v", ev.args[base+3])
			}
			seen++
		}
	}
	if seen != nChildren {
		t.Fatalf("子块 %d", seen)
	}
	last := children[1].args
	if strArg(t, last[len(last)-1]) != "it's a ?" {
		t.Fatalf("最后一块正文 %v", last[len(last)-1])
	}
	if len(fc.parents) != nParents || fc.parents[0] != 1 || fc.parents[nParents-1] != int64(nParents) {
		t.Fatalf("父块编号 %v", fc.parents)
	}
}

func TestReplaceSectionsParentIDStep(t *testing.T) {
	fc := newScriptConn()
	fc.step = 2
	fc.nextParent = 10
	m := openScriptMySQL(t, fc)
	docID := int64(5)
	groups := []ParentWithChildren{
		{
			Parent: Parent{HeadingPath: "a", Body: "pa"},
			Children: []Child{{
				Ordinal: 0, StartByte: 1, EndByte: 2, HeadingPath: "a", Page: 4, Body: "ca",
			}},
		},
		{
			Parent:   Parent{HeadingPath: "b", Body: "pb"},
			Children: []Child{{Ordinal: 1, Body: "cb"}},
		},
	}
	if err := m.ReplaceSections(context.Background(), docID, groups); err != nil {
		t.Fatalf("ReplaceSections：%v", err)
	}
	kids := eventsNamed(fc, "insert-children")
	if len(kids) != 1 || kids[0].rows() != 2 {
		t.Fatalf("子块批次 %d", len(kids))
	}
	if intArg(t, kids[0].args[1]) != 10 || intArg(t, kids[0].args[childArgWidth+1]) != 12 {
		t.Fatalf("父块编号步长不对 %v", kids[0].args)
	}
	if !reflect.DeepEqual(fc.parents, []int64{10, 12}) {
		t.Fatalf("父块编号 %v", fc.parents)
	}
}

func TestReplaceSectionsRollsBackOnMismatch(t *testing.T) {
	fc := newScriptConn()
	fc.skew = 1
	m := openScriptMySQL(t, fc)
	err := m.ReplaceSections(context.Background(), 3, []ParentWithChildren{{
		Parent:   Parent{Body: "p"},
		Children: []Child{{Ordinal: 0, Body: "c"}},
	}})
	if !errors.Is(err, errParentIDSequence) {
		t.Fatalf("err = %v", err)
	}
	names := eventNames(fc)
	if strings.Contains(strings.Join(names, " "), "commit") || strings.Contains(strings.Join(names, " "), "insert-children") {
		t.Fatalf("编号不一致仍继续写了 %v", names)
	}
	if names[len(names)-1] != "rollback" {
		t.Fatalf("事件 %v", names)
	}
}

func TestReplaceSectionsRollsBackOnDuplicateChild(t *testing.T) {
	fc := newScriptConn()
	fc.failChild = &mysql.MySQLError{Number: mySQLDupEntry, Message: "Duplicate entry"}
	m := openScriptMySQL(t, fc)
	err := m.ReplaceSections(context.Background(), 3, []ParentWithChildren{{
		Parent:   Parent{Body: "p"},
		Children: []Child{{Ordinal: 0, Body: "c"}},
	}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	names := eventNames(fc)
	if strings.Contains(strings.Join(names, " "), "commit") || names[len(names)-1] != "rollback" {
		t.Fatalf("事件 %v", names)
	}
}

func TestReplaceSectionsMissingDocument(t *testing.T) {
	fc := newScriptConn()
	fc.missing = 9
	m := openScriptMySQL(t, fc)
	err := m.ReplaceSections(context.Background(), 9, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	names := eventNames(fc)
	if !reflect.DeepEqual(names, []string{"begin", "lock", "rollback"}) {
		t.Fatalf("事件 %v", names)
	}
}

func TestReplaceSectionsEmptyClearsWithoutInsert(t *testing.T) {
	fc := newScriptConn()
	m := openScriptMySQL(t, fc)
	if err := m.ReplaceSections(context.Background(), 3, nil); err != nil {
		t.Fatalf("ReplaceSections：%v", err)
	}
	if !reflect.DeepEqual(eventNames(fc), []string{"begin", "lock", "delete-children", "delete-parents", "commit"}) {
		t.Fatalf("事件 %v", eventNames(fc))
	}
}

func TestReplaceSectionsRejectsBeforeTransaction(t *testing.T) {
	fc := newScriptConn()
	m := openScriptMySQL(t, fc)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.ReplaceSections(ctx, 3, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消 err = %v", err)
	}
	err := m.ReplaceSections(context.Background(), 3, []ParentWithChildren{{
		Children: []Child{{Ordinal: 1, Body: "a"}, {Ordinal: 1, Body: "b"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "duplicate ordinal") {
		t.Fatalf("重复序号 err = %v", err)
	}
	if len(fc.events) != 0 {
		t.Fatalf("校验前不应访问数据库 %v", eventNames(fc))
	}
}

func assertBatchShape(t *testing.T, batches []writeBatch, n, width int) {
	t.Helper()
	if len(batches) < 2 {
		t.Fatal("多行被拼进了一条语句")
	}
	total := 0
	full := 0
	for _, batch := range batches {
		if batch.rows <= 0 || batch.rows > writeBatchRows {
			t.Fatalf("批行数 %d", batch.rows)
		}
		if batch.rows == writeBatchRows {
			full++
		}
		if strings.Count(batch.query, "?") != len(batch.args) || len(batch.args) != batch.rows*width {
			t.Fatalf("占位符与参数不一致 rows=%d args=%d", batch.rows, len(batch.args))
		}
		if strings.Contains(batch.query, "';") {
			t.Fatal("批量语句里出现了拼接的字符串")
		}
		total += batch.rows
	}
	if total != n || full < 2 {
		t.Fatalf("合计 %d 满批 %d", total, full)
	}
}

func seqIDs(n int) []int64 {
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	return ids
}

func assertDocArgs(t *testing.T, fc *scriptConn, docID int64) {
	t.Helper()
	for _, ev := range fc.events {
		low := strings.ToLower(ev.query)
		for _, word := range []string{"foreign_key_checks", "unique_checks", "innodb_flush_log_at_trx_commit", "@tcp"} {
			if strings.Contains(low, word) {
				t.Fatalf("%s 包含 %s", ev.name, word)
			}
		}
		switch ev.name {
		case "lock", "delete-children", "delete-parents", "read-parents":
			if len(ev.args) != 1 || intArg(t, ev.args[0]) != docID {
				t.Fatalf("%s 参数 %v", ev.name, ev.args)
			}
		}
	}
}

func repeatName(name string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = name
	}
	return out
}

func eventNames(fc *scriptConn) []string {
	names := make([]string, len(fc.events))
	for i, ev := range fc.events {
		names[i] = ev.name
	}
	return names
}

func eventsNamed(fc *scriptConn, name string) []scriptEvent {
	var out []scriptEvent
	for _, ev := range fc.events {
		if ev.name == name {
			out = append(out, ev)
		}
	}
	return out
}

func intArg(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	default:
		t.Fatalf("参数类型 %T", v)
		return 0
	}
}

func strArg(t *testing.T, v any) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("参数类型 %T", v)
	}
	return s
}

type scriptEvent struct {
	name  string
	query string
	args  []any
}

func (e scriptEvent) rows() int {
	width := childArgWidth
	if e.name == "insert-parents" {
		width = parentArgWidth
	}
	return len(e.args) / width
}

func openScriptMySQL(t *testing.T, fc *scriptConn) *MySQL {
	t.Helper()
	db := sql.OpenDB(scriptConnector{conn: fc})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return &MySQL{db: db}
}

func newScriptConn() *scriptConn {
	return &scriptConn{step: 1, nextParent: 1}
}

type scriptConnector struct{ conn *scriptConn }

func (c scriptConnector) Connect(context.Context) (driver.Conn, error) {
	if c.conn == nil {
		return nil, errors.New("catalog test: no conn")
	}
	return c.conn, nil
}

func (c scriptConnector) Driver() driver.Driver { return scriptDriver{} }

type scriptDriver struct{}

func (scriptDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("catalog test: open not used")
}

type scriptConn struct {
	step       int64
	nextParent int64
	missing    int64
	skew       int64
	failChild  error
	parents    []int64
	inTx       bool
	events     []scriptEvent
}

func (c *scriptConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("catalog test: prepare not used")
}

func (c *scriptConn) Close() error { return nil }

func (c *scriptConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *scriptConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Isolation != 0 || opts.ReadOnly {
		return nil, errors.New("catalog test: non-default transaction")
	}
	if c.inTx {
		return nil, errors.New("catalog test: nested transaction")
	}
	c.inTx = true
	c.events = append(c.events, scriptEvent{name: "begin"})
	return scriptTx{c}, nil
}

func (c *scriptConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.inTx {
		return nil, errors.New("catalog test: statement outside transaction")
	}
	if err := rejectScript(query); err != nil {
		return nil, err
	}
	switch classifyQuery(query) {
	case "delete-children":
		c.record("delete-children", query, args)
		return scriptResult{}, nil
	case "delete-parents":
		c.parents = nil
		c.record("delete-parents", query, args)
		return scriptResult{}, nil
	case "insert-parents":
		n := strings.Count(query, "?")
		if n%parentArgWidth != 0 || n == 0 {
			return nil, errors.New("catalog test: bad parent insert")
		}
		nrows := n / parentArgWidth
		if nrows > writeBatchRows {
			return nil, fmt.Errorf("catalog test: parent batch %d", nrows)
		}
		if c.step <= 0 || c.nextParent <= 0 {
			return nil, errors.New("catalog test: bad step")
		}
		first := c.nextParent
		for i := 0; i < nrows; i++ {
			c.parents = append(c.parents, first+int64(i)*c.step)
		}
		c.nextParent = first + int64(nrows)*c.step
		c.record("insert-parents", query, args)
		return scriptResult{id: first, n: int64(nrows)}, nil
	case "insert-children":
		n := strings.Count(query, "?")
		if n%childArgWidth != 0 || n == 0 {
			return nil, errors.New("catalog test: bad child insert")
		}
		nrows := n / childArgWidth
		if nrows > writeBatchRows {
			return nil, fmt.Errorf("catalog test: child batch %d", nrows)
		}
		c.record("insert-children", query, args)
		if c.failChild != nil {
			return nil, c.failChild
		}
		return scriptResult{n: int64(nrows)}, nil
	default:
		return nil, fmt.Errorf("catalog test: unexpected exec %q", clip(query))
	}
}

func (c *scriptConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.inTx {
		return nil, errors.New("catalog test: query outside transaction")
	}
	if err := rejectScript(query); err != nil {
		return nil, err
	}
	switch classifyQuery(query) {
	case "step":
		c.record("step", query, args)
		return oneCol("step", c.step), nil
	case "lock":
		if len(args) != 1 {
			return nil, errors.New("catalog test: lock args")
		}
		id, ok := args[0].Value.(int64)
		if !ok || id <= 0 {
			return nil, errors.New("catalog test: lock id")
		}
		c.record("lock", query, args)
		if c.missing != 0 && id == c.missing {
			return oneCol("doc_id"), nil
		}
		return oneCol("doc_id", id), nil
	case "read-parents":
		c.record("read-parents", query, args)
		ids := append([]int64(nil), c.parents...)
		for i := range ids {
			ids[i] += c.skew
		}
		return oneCol("parent_id", ids...), nil
	default:
		return nil, fmt.Errorf("catalog test: unexpected query %q", clip(query))
	}
}

func (c *scriptConn) record(name, query string, args []driver.NamedValue) {
	vals := make([]any, len(args))
	for i, arg := range args {
		vals[i] = arg.Value
	}
	c.events = append(c.events, scriptEvent{name: name, query: query, args: vals})
}

type scriptTx struct{ c *scriptConn }

func (t scriptTx) Commit() error {
	if !t.c.inTx {
		return errors.New("catalog test: commit without transaction")
	}
	t.c.inTx = false
	t.c.events = append(t.c.events, scriptEvent{name: "commit"})
	return nil
}

func (t scriptTx) Rollback() error {
	if !t.c.inTx {
		return errors.New("catalog test: rollback without transaction")
	}
	t.c.inTx = false
	t.c.events = append(t.c.events, scriptEvent{name: "rollback"})
	return nil
}

type scriptResult struct{ id, n int64 }

func (r scriptResult) LastInsertId() (int64, error) {
	if r.id <= 0 {
		return 0, errors.New("catalog test: no insert id")
	}
	return r.id, nil
}

func (r scriptResult) RowsAffected() (int64, error) { return r.n, nil }

type scriptRows struct {
	cols []string
	data [][]driver.Value
	i    int
}

func (r *scriptRows) Columns() []string { return r.cols }

func (r *scriptRows) Close() error { return nil }

func (r *scriptRows) Next(dest []driver.Value) error {
	if r.i >= len(r.data) {
		return io.EOF
	}
	row := r.data[r.i]
	r.i++
	if len(dest) != len(row) {
		return fmt.Errorf("catalog test: dest %d row %d", len(dest), len(row))
	}
	copy(dest, row)
	return nil
}

func oneCol(name string, values ...int64) *scriptRows {
	data := make([][]driver.Value, len(values))
	for i, v := range values {
		data[i] = []driver.Value{v}
	}
	return &scriptRows{cols: []string{name}, data: data}
}

func classifyQuery(query string) string {
	q := strings.TrimSpace(query)
	switch {
	case strings.Contains(q, "auto_increment_increment"):
		return "step"
	case strings.Contains(q, "FROM source_document"):
		return "lock"
	case strings.HasPrefix(q, "DELETE FROM child_chunk"):
		return "delete-children"
	case strings.HasPrefix(q, "DELETE FROM parent_section"):
		return "delete-parents"
	case strings.HasPrefix(q, "INSERT INTO parent_section"):
		return "insert-parents"
	case strings.HasPrefix(q, "INSERT INTO child_chunk"):
		return "insert-children"
	case strings.HasPrefix(q, "SELECT") && strings.Contains(q, "FROM parent_section"):
		return "read-parents"
	default:
		return ""
	}
}

func rejectScript(query string) error {
	if strings.Contains(query, ";") {
		return errors.New("catalog test: multiple statements")
	}
	low := strings.ToLower(query)
	for _, word := range []string{"foreign_key_checks", "unique_checks", "innodb_flush_log_at_trx_commit", "sql_log_bin"} {
		if strings.Contains(low, word) {
			return errors.New("catalog test: durability setting")
		}
	}
	return nil
}

func clip(query string) string {
	q := strings.TrimSpace(query)
	if len(q) > 80 {
		return q[:80]
	}
	return q
}

var (
	_ driver.Conn           = (*scriptConn)(nil)
	_ driver.ConnBeginTx    = (*scriptConn)(nil)
	_ driver.ExecerContext  = (*scriptConn)(nil)
	_ driver.QueryerContext = (*scriptConn)(nil)
	_ driver.Connector      = scriptConnector{}
)
