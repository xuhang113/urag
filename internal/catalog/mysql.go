package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "embed"

	"github.com/go-sql-driver/mysql"
)

//go:embed schema.sql
var schemaSQL string

// 调用方传入 DSN，本包不读取环境变量，也不把 DSN 写进日志。
// 口令由调用方保管，例如放在 URAG_MYSQL_DSN 里。
// 示例：example-user:fake-password@tcp(127.0.0.1:3306)/urag_example
const driverName = "mysql"

const (
	insertDocumentSQL = `
INSERT INTO source_document (
  collection_id, version, source_uri, checksum, media_type, license, title, fetched_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

	selectDocumentSQL = `
SELECT doc_id, checksum, version
FROM source_document
WHERE collection_id = ? AND source_uri = ?
FOR UPDATE`

	updateDocumentSQL = `
UPDATE source_document
SET version = version + 1,
    checksum = ?,
    media_type = ?,
    license = ?,
    title = ?,
    fetched_at = ?
WHERE doc_id = ?`

	lockDocumentSQL = `
SELECT doc_id FROM source_document WHERE doc_id = ? FOR UPDATE`

	insertParentSQL = `
INSERT INTO parent_section (
  doc_id, heading_path, page_from, page_to, body
) VALUES (?, ?, ?, ?, ?)`

	selectParentSQL = `
SELECT parent_id, doc_id, heading_path, page_from, page_to, body
FROM parent_section
WHERE parent_id = ?`

	insertChildSQL = `
INSERT INTO child_chunk (
  chunk_id, parent_id, doc_id, ordinal, start_byte, end_byte, heading_path, page, body
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

	selectChildrenSQL = `
SELECT chunk_id, parent_id, doc_id, ordinal, start_byte, end_byte, heading_path, page, body
FROM child_chunk
WHERE doc_id = ?
ORDER BY ordinal, chunk_id`

	countDocumentsSQL = `
SELECT COUNT(*) FROM source_document WHERE collection_id = ?`

	deleteChildrenSQL = `DELETE FROM child_chunk WHERE doc_id = ?`
	deleteParentsSQL  = `DELETE FROM parent_section WHERE doc_id = ?`

	lockParentSQL = `SELECT doc_id FROM parent_section WHERE parent_id = ? FOR UPDATE`
)

// ER_DUP_ENTRY。同一集合的同一个 source_uri，或同一个文档序号，都会落到这里。
const mySQLDupEntry uint16 = 1062

// MySQL 用 database/sql 保存台账。
// 表结构在 schema.sql。ApplySchema 负责建表，NewMySQL 不建表，也不立刻连库。
type MySQL struct {
	db *sql.DB
}

var _ Store = (*MySQL)(nil)

// NewMySQL 用调用方传入的 DSN 准备连接。
// 空字符串会返回错误。sql.Open 不会访问网络，第一次查询才连接。
// 时间按 UTC 写入，并打开 ParseTime，调用方不必自己拼这些参数。
func NewMySQL(dsn string) (*MySQL, error) {
	db, err := openMySQL(dsn)
	if err != nil {
		return nil, err
	}
	return &MySQL{db: db}, nil
}

func openMySQL(dsn string) (*sql.DB, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, errors.New("catalog: empty dsn")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("catalog: parse dsn: %w", err)
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["charset"] = "utf8mb4"
	db, err := sql.Open(driverName, cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("catalog: open: %w", err)
	}
	return db, nil
}

// Close 关闭连接池。
func (m *MySQL) Close() error {
	if m == nil || m.db == nil {
		return nil
	}
	return m.db.Close()
}

// ApplySchema 执行 schema.sql。缺表时创建，已有的表不会被修改。
func (m *MySQL) ApplySchema(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, stmt := range splitSQL(schemaSQL) {
		if _, err := m.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("catalog: apply schema: %w", err)
		}
	}
	return nil
}

func (m *MySQL) PutDocument(ctx context.Context, in Document) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := validateDocument(in); err != nil {
		return 0, err
	}
	in.FetchedAt = normalizeFetched(in.FetchedAt)
	tx, err := m.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	id, err := insertOrUpdateDocument(ctx, tx, in)
	if err != nil {
		return 0, err
	}
	if err := commit(tx); err != nil {
		return 0, err
	}
	return id, nil
}

func (m *MySQL) PutParent(ctx context.Context, in Parent) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := validateParent(in); err != nil {
		return 0, err
	}
	tx, err := m.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	if err := lockDocument(ctx, tx, in.DocID); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, insertParentSQL, in.DocID, in.HeadingPath, in.PageFrom, in.PageTo, in.Body)
	if err != nil {
		return 0, fmt.Errorf("catalog: insert parent: %w", err)
	}
	id, err := generatedID(res)
	if err != nil {
		return 0, err
	}
	if err := commit(tx); err != nil {
		return 0, err
	}
	return id, nil
}

func (m *MySQL) PutChild(ctx context.Context, in Child) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateChild(in); err != nil {
		return err
	}
	tx, err := m.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	// 先锁来源行，再锁父块。ReplaceSections 也先锁来源行，两边不会交叉等待。
	if err := lockDocument(ctx, tx, in.DocID); err != nil {
		return err
	}
	var parentDoc int64
	err = tx.QueryRowContext(ctx, lockParentSQL, in.ParentID).Scan(&parentDoc)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && parentDoc != in.DocID) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("catalog: lookup parent: %w", err)
	}
	chunkID := ChunkID(in.DocID, in.Ordinal)
	_, err = tx.ExecContext(ctx, insertChildSQL,
		chunkID, in.ParentID, in.DocID, in.Ordinal, in.StartByte, in.EndByte, in.HeadingPath, in.Page, in.Body)
	if isDuplicateKey(err) {
		return ErrConflict
	}
	if err != nil {
		return fmt.Errorf("catalog: insert child: %w", err)
	}
	return commit(tx)
}

func (m *MySQL) GetParent(ctx context.Context, parentID int64) (Parent, error) {
	if err := ctx.Err(); err != nil {
		return Parent{}, err
	}
	if parentID <= 0 {
		return Parent{}, ErrNotFound
	}
	var out Parent
	err := m.db.QueryRowContext(ctx, selectParentSQL, parentID).Scan(
		&out.ParentID, &out.DocID, &out.HeadingPath, &out.PageFrom, &out.PageTo, &out.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return Parent{}, ErrNotFound
	}
	if err != nil {
		return Parent{}, fmt.Errorf("catalog: get parent: %w", err)
	}
	return out, nil
}

func (m *MySQL) ListChildren(ctx context.Context, docID int64) ([]Child, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if docID <= 0 {
		return nil, ErrNotFound
	}
	tx, err := m.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	if err := lockDocument(ctx, tx, docID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, selectChildrenSQL, docID)
	if err != nil {
		return nil, fmt.Errorf("catalog: list children: %w", err)
	}
	defer rows.Close()
	out := []Child{}
	for rows.Next() {
		var child Child
		if err := rows.Scan(
			&child.ChunkID, &child.ParentID, &child.DocID, &child.Ordinal,
			&child.StartByte, &child.EndByte, &child.HeadingPath, &child.Page, &child.Body,
		); err != nil {
			return nil, fmt.Errorf("catalog: list children: %w", err)
		}
		out = append(out, child)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: list children: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("catalog: list children: %w", err)
	}
	if err := commit(tx); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *MySQL) CountIndexed(ctx context.Context, collectionID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var n int64
	if err := m.db.QueryRowContext(ctx, countDocumentsSQL, collectionID).Scan(&n); err != nil {
		return 0, fmt.Errorf("catalog: count: %w", err)
	}
	limit := int64(^uint(0) >> 1)
	if n < 0 || n > limit {
		return 0, errors.New("catalog: count out of range")
	}
	return int(n), nil
}

func (m *MySQL) ReplaceSections(ctx context.Context, docID int64, groups []ParentWithChildren) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateBatch(docID, groups); err != nil {
		return err
	}
	tx, err := m.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := lockDocument(ctx, tx, docID); err != nil {
		return err
	}
	// 先删子块，再删父块，外键才不会拦住。任一步失败都会回滚，旧段落仍在。
	if _, err := tx.ExecContext(ctx, deleteChildrenSQL, docID); err != nil {
		return fmt.Errorf("catalog: delete children: %w", err)
	}
	if _, err := tx.ExecContext(ctx, deleteParentsSQL, docID); err != nil {
		return fmt.Errorf("catalog: delete parents: %w", err)
	}
	for _, group := range groups {
		res, err := tx.ExecContext(ctx, insertParentSQL,
			docID, group.Parent.HeadingPath, group.Parent.PageFrom, group.Parent.PageTo, group.Parent.Body)
		if err != nil {
			return fmt.Errorf("catalog: insert parent: %w", err)
		}
		parentID, err := generatedID(res)
		if err != nil {
			return err
		}
		for _, child := range group.Children {
			_, err := tx.ExecContext(ctx, insertChildSQL,
				ChunkID(docID, child.Ordinal), parentID, docID, child.Ordinal,
				child.StartByte, child.EndByte, child.HeadingPath, child.Page, child.Body)
			if err != nil {
				return fmt.Errorf("catalog: insert child: %w", err)
			}
		}
	}
	return commit(tx)
}

func (m *MySQL) begin(ctx context.Context) (*sql.Tx, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("catalog: begin: %w", err)
	}
	return tx, nil
}

func insertOrUpdateDocument(ctx context.Context, tx *sql.Tx, in Document) (int64, error) {
	id, checksum, version, found, err := lookupDocument(ctx, tx, in.CollectionID, in.SourceURI)
	if err != nil {
		return 0, err
	}
	if !found {
		res, err := tx.ExecContext(ctx, insertDocumentSQL,
			in.CollectionID, in.Version, in.SourceURI, in.Checksum, in.MediaType, in.License, in.Title, fetchedArg(in.FetchedAt))
		if err != nil {
			if !isDuplicateKey(err) {
				return 0, fmt.Errorf("catalog: insert document: %w", err)
			}
			id, checksum, version, found, err = lookupDocument(ctx, tx, in.CollectionID, in.SourceURI)
			if err != nil {
				return 0, err
			}
			if !found {
				return 0, errors.New("catalog: document disappeared")
			}
			return reviseDocument(ctx, tx, id, checksum, version, in)
		}
		return generatedID(res)
	}
	return reviseDocument(ctx, tx, id, checksum, version, in)
}

func reviseDocument(ctx context.Context, tx *sql.Tx, id int64, checksum string, version int, in Document) (int64, error) {
	if checksum == in.Checksum {
		return id, nil
	}
	if version >= maxVersion {
		return 0, errVersionOverflow
	}
	res, err := tx.ExecContext(ctx, updateDocumentSQL,
		in.Checksum, in.MediaType, in.License, in.Title, fetchedArg(in.FetchedAt), id)
	if err != nil {
		return 0, fmt.Errorf("catalog: update document: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("catalog: update document: %w", err)
	}
	if n != 1 {
		return 0, errors.New("catalog: document was not updated")
	}
	return id, nil
}

func lookupDocument(ctx context.Context, tx *sql.Tx, collectionID, sourceURI string) (int64, string, int, bool, error) {
	var (
		id       int64
		checksum string
		version  int
	)
	err := tx.QueryRowContext(ctx, selectDocumentSQL, collectionID, sourceURI).Scan(&id, &checksum, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", 0, false, nil
	}
	if err != nil {
		return 0, "", 0, false, fmt.Errorf("catalog: lookup document: %w", err)
	}
	return id, strings.TrimRight(checksum, " "), version, true, nil
}

func lockDocument(ctx context.Context, tx *sql.Tx, docID int64) error {
	var id int64
	err := tx.QueryRowContext(ctx, lockDocumentSQL, docID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("catalog: lookup document: %w", err)
	}
	return nil
}

func generatedID(res sql.Result) (int64, error) {
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("catalog: generated id: %w", err)
	}
	if id <= 0 {
		return 0, errors.New("catalog: id was not generated")
	}
	return id, nil
}

func commit(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("catalog: commit: %w", err)
	}
	return nil
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}

func fetchedArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == mySQLDupEntry
}

func splitSQL(script string) []string {
	var b strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	parts := strings.Split(b.String(), ";")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		stmt := strings.TrimSpace(part)
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}
