// Package chunk 定义把一篇文档切成带字节区间的片段。
// 本包不嵌入、不检索，也不把不同集合的正文切到一起。
package chunk

import (
	"strings"

	"github.com/xuhang113/urag/internal/model"
)

// Options 控制切分长度，单位是 Document.Text 的字节。
type Options struct {
	MaxLen  int // 默认 800。小于等于 0 时按 800。
	Overlap int // 默认 100。0 表示默认，小于 0 时不重叠。超过 MaxLen 的一半时截到一半。
}

// Splitter 把一篇文档切成片段。
// 每个 Chunk 的编号使用 documentID + "#" + ordinal。
// StartByte 与 EndByte 落在 Document.Text 的字节区间上。
// paragraph 和 drop_keywords 的片段正文等于 Text[StartByte:EndByte]。
// heading 的正文已经去掉标题行，Text[StartByte:EndByte] 包含该片段，两者不一定相等。
// 失败时返回错误，不返回半成品。空正文得到空切片。
type Splitter interface {
	// Name 返回切法名称，供评价报告引用。
	Name() string
	// Split 切分 doc。opts 的零值按 Options 的默认长度处理。
	Split(doc model.Document, opts Options) ([]model.Chunk, error)
}

type paragraphSplitter struct{}

// NewParagraph 返回默认切法，名称是 paragraph。
// 先按空行分段，再按 MaxLen 打包，相邻片段重叠 Overlap 字节。
// 片段正文等于 Document.Text[StartByte:EndByte]。
func NewParagraph() Splitter { return paragraphSplitter{} }

func (paragraphSplitter) Name() string { return "paragraph" }

type dropSplitter struct {
	drop []string
}

// NewDropKeywords 返回只给召回对照实验的切法，名称是 drop_keywords。
// 按整词、不区分大小写丢掉 drop 里的词，再把留下的文字切成不超过 20 字节的小块。
// 不得当作默认切法。StartByte 与 EndByte 落在 Document.Text 上。
func NewDropKeywords(drop []string) Splitter {
	cp := make([]string, 0, len(drop))
	for _, word := range drop {
		word = strings.TrimSpace(word)
		if word == "" {
			continue
		}
		cp = append(cp, word)
	}
	return dropSplitter{drop: cp}
}

func (dropSplitter) Name() string { return "drop_keywords" }
