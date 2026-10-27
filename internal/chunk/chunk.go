// Package chunk 定义把一篇文档切成带字节区间的片段。
// 本包不嵌入、不检索，也不把不同集合的正文切到一起。
package chunk

import (
	"errors"

	"github.com/xuhang113/urag/internal/model"
)

var errNotImplemented = errors.New("chunk: not implemented")

// Options 控制切分长度，单位是 Document.Text 的字节。
type Options struct {
	MaxLen  int // 默认 800。小于等于 0 时按 800。
	Overlap int // 默认 100。小于等于 0 时按 100。
}

// Splitter 把一篇文档切成片段。
// 每个 Chunk 的编号使用 documentID + "#" + ordinal。
// StartByte 与 EndByte 标在 Document.Text 的字节区间上，Text[StartByte:EndByte] 能取回该片段。
// 失败时返回错误，不返回半成品。空正文得到空切片。
type Splitter interface {
	// Name 返回切法名称，供评价报告引用。
	Name() string
	// Split 切分 doc。opts 的零值按 Options 的默认长度处理。
	Split(doc model.Document, opts Options) ([]model.Chunk, error)
}

type paragraphSplitter struct{}

// NewParagraph 返回默认切法，名称是 paragraph。
// 接通后先按空行分段，再按 MaxLen 打包，相邻片段带 Overlap。
// 当前 Split 返回未实现错误。
func NewParagraph() Splitter { return paragraphSplitter{} }

func (paragraphSplitter) Name() string { return "paragraph" }

func (paragraphSplitter) Split(model.Document, Options) ([]model.Chunk, error) {
	return nil, errNotImplemented
}

type dropSplitter struct {
	drop []string
}

// NewDropKeywords 返回只给召回对照实验的切法，名称是 drop_keywords。
// 接通后丢掉 drop 里的词并切碎正文。不得当作默认切法。
// StartByte 与 EndByte 同样标在 Document.Text 的字节区间上。
// 当前 Split 返回未实现错误。
func NewDropKeywords(drop []string) Splitter {
	return dropSplitter{drop: drop}
}

func (dropSplitter) Name() string { return "drop_keywords" }

func (dropSplitter) Split(model.Document, Options) ([]model.Chunk, error) {
	return nil, errNotImplemented
}
