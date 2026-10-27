package catalog

import (
	"errors"
	"math"
	"strings"
)

const (
	// writeBatchRows 是一条多值 INSERT 的行数上限，落在 100 到 200 之间。
	// 最后一批可以更短。不把一篇文档以外的行拼进同一条语句。
	writeBatchRows = 128

	// writeBatchBytes 是这一批参数的字节上限。
	// 正常子块远小于它，所以仍然凑满 writeBatchRows 行。
	// 单行超过上限时单独成批，因为正文不能再拆。
	writeBatchBytes = 1 << 20

	parentArgWidth = 5
	childArgWidth  = 9
)

const (
	parentInsertHead = "INSERT INTO parent_section (\n  doc_id, heading_path, page_from, page_to, body\n) VALUES "
	childInsertHead  = "INSERT INTO child_chunk (\n  chunk_id, parent_id, doc_id, ordinal, start_byte, end_byte, heading_path, page, body\n) VALUES "
	parentValueTuple = "(?, ?, ?, ?, ?)"
	childValueTuple  = "(?, ?, ?, ?, ?, ?, ?, ?, ?)"
)

// errParentIDSequence 表示算出来的 parent_id 和表里的不一致。
// 调用方应回滚，不能把子块挂到错误的父块上。
var errParentIDSequence = errors.New("catalog: parent id sequence")

type writeBatch struct {
	query string
	args  []any
	rows  int
}

type queuedChild struct {
	parentID int64
	child    Child
}

func parentBatches(docID int64, groups []ParentWithChildren) []writeBatch {
	spans := batchSpans(len(groups), func(i int) int {
		return parentRowBytes(groups[i].Parent)
	})
	out := make([]writeBatch, 0, len(spans))
	for _, sp := range spans {
		out = append(out, buildParentBatch(docID, groups[sp[0]:sp[1]]))
	}
	return out
}

func childBatches(docID int64, groups []ParentWithChildren, parentIDs []int64) ([]writeBatch, error) {
	if len(parentIDs) != len(groups) {
		return nil, errors.New("catalog: parent id count")
	}
	rows := make([]queuedChild, 0)
	for i, group := range groups {
		pid := parentIDs[i]
		for _, child := range group.Children {
			rows = append(rows, queuedChild{parentID: pid, child: child})
		}
	}
	spans := batchSpans(len(rows), func(i int) int {
		return childRowBytes(rows[i].child)
	})
	out := make([]writeBatch, 0, len(spans))
	for _, sp := range spans {
		out = append(out, buildChildBatch(docID, rows[sp[0]:sp[1]]))
	}
	return out, nil
}

func batchSpans(n int, sizeAt func(int) int) [][2]int {
	if n == 0 {
		return nil
	}
	limit := writeBatchRows
	if limit < 1 {
		limit = 1
	}
	spans := make([][2]int, 0, (n+limit-1)/limit)
	start := 0
	for start < n {
		end := start
		bytes := 0
		for end < n && end-start < limit {
			sz := sizeAt(end)
			if sz < 0 {
				sz = 0
			}
			if end > start && (sz >= writeBatchBytes || bytes >= writeBatchBytes-sz) {
				break
			}
			bytes += sz
			end++
		}
		if end == start {
			end++
		}
		spans = append(spans, [2]int{start, end})
		start = end
	}
	return spans
}

func buildParentBatch(docID int64, groups []ParentWithChildren) writeBatch {
	args := make([]any, 0, len(groups)*parentArgWidth)
	var b strings.Builder
	b.Grow(len(parentInsertHead) + len(groups)*(len(parentValueTuple)+2))
	b.WriteString(parentInsertHead)
	for i, group := range groups {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(parentValueTuple)
		p := group.Parent
		args = append(args, docID, p.HeadingPath, p.PageFrom, p.PageTo, p.Body)
	}
	return writeBatch{query: b.String(), args: args, rows: len(groups)}
}

func buildChildBatch(docID int64, rows []queuedChild) writeBatch {
	args := make([]any, 0, len(rows)*childArgWidth)
	var b strings.Builder
	b.Grow(len(childInsertHead) + len(rows)*(len(childValueTuple)+2))
	b.WriteString(childInsertHead)
	for i, row := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(childValueTuple)
		ch := row.child
		args = append(args,
			ChunkID(docID, ch.Ordinal),
			row.parentID,
			docID,
			ch.Ordinal,
			ch.StartByte,
			ch.EndByte,
			ch.HeadingPath,
			ch.Page,
			ch.Body,
		)
	}
	return writeBatch{query: b.String(), args: args, rows: len(rows)}
}

func parentRowBytes(p Parent) int {
	return len(p.HeadingPath) + len(p.Body) + 32
}

func childRowBytes(c Child) int {
	return len(c.HeadingPath) + len(c.Body) + 48
}

// expandIDs 还原一条简单多值 INSERT 的自增值。
// MySQL 的 OK 包只带回第一行的编号，后续行按 auto_increment_increment 递增。
func expandIDs(first, step int64, n int) ([]int64, error) {
	if first <= 0 || step <= 0 || n <= 0 {
		return nil, errors.New("catalog: generated id")
	}
	if n > 1 {
		span := int64(n - 1)
		if step > (math.MaxInt64-first)/span {
			return nil, errors.New("catalog: generated id overflow")
		}
	}
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		ids[i] = first + int64(i)*step
	}
	return ids, nil
}
