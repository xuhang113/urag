package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SourceChunk 是某个 source_uri 前缀下的一个子块。
// 正文是清洗后的子块，不是 PDF 或 DOCX，也不是整库转储。
type SourceChunk struct {
	CollectionID string
	SourceURI    string
	Version      int
	Child
}

const listBySourcePrefixSQL = `
SELECT d.collection_id, d.source_uri, d.version,
       c.chunk_id, c.parent_id, c.doc_id, c.ordinal,
       c.start_byte, c.end_byte, c.heading_path, c.page, c.body
FROM source_document d
INNER JOIN child_chunk c ON c.doc_id = d.doc_id
WHERE d.source_uri LIKE ? ESCAPE '\\'
ORDER BY d.source_uri, c.ordinal, c.chunk_id`

// ListBySourcePrefix 只返回 source_uri 以前缀开头的子块。
// 前缀为空时返回错误，避免把整库读进内存。
func (m *MySQL) ListBySourcePrefix(ctx context.Context, prefix string) ([]SourceChunk, error) {
	if m == nil || m.db == nil {
		return nil, errors.New("catalog: nil mysql")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pattern, err := likePrefix(prefix)
	if err != nil {
		return nil, err
	}
	rows, err := m.db.QueryContext(ctx, listBySourcePrefixSQL, pattern)
	if err != nil {
		return nil, fmt.Errorf("catalog: list source prefix: %w", err)
	}
	defer rows.Close()
	var out []SourceChunk
	for rows.Next() {
		var row SourceChunk
		if err := rows.Scan(
			&row.CollectionID, &row.SourceURI, &row.Version,
			&row.ChunkID, &row.ParentID, &row.DocID, &row.Ordinal,
			&row.StartByte, &row.EndByte, &row.HeadingPath, &row.Page, &row.Body,
		); err != nil {
			return nil, fmt.Errorf("catalog: list source prefix: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: list source prefix: %w", err)
	}
	return out, nil
}

func likePrefix(prefix string) (string, error) {
	if strings.TrimSpace(prefix) == "" {
		return "", errors.New("catalog: empty source prefix")
	}
	var b strings.Builder
	for _, r := range prefix {
		switch r {
		case '%', '_', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('%')
	return b.String(), nil
}
