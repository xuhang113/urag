# 清洗

## 要做什么

把原始对象变成 UTF-8 正文，并留下血缘。支持 txt、md、html、pdf、docx。识别编码后输出 UTF-8。去掉 nav、header、footer 和重复模板。按正文哈希在集合内去重。

docx 用纯 Go 读 zip 里的 `word/document.xml`，不把文件写入存储。标题样式写成 markdown 标题行：`#`、`##`、`###`，更深的标题继续加 `#`，最多六个。正文每个段落一块，段落之间空一行。样式认 `Heading N`、`标题 N`、大纲级别 0 到 8，以及 `basedOn` 链。段落上的大纲级别盖过样式。大纲级别 9 仍是正文。页眉页脚部件不读。

html 保留正文里的 `h1` 到 `h6`，同样写成 markdown 标题行，段落之间空一行。nav、header、footer 以及 class 或 id 里带这些样板词的块继续丢掉。

重复文档 `Duplicate` 为 true，`Lineage.DuplicateOf` 指向已有文档，不进入索引。`Lineage.Steps` 按顺序保留每一步，`Name` 取 detect、strip、dedupe。`Lineage.RawObjectKey` 等于输入对象键。

默认切分仍是 `NewParagraph`：先按空行分段，再按 `MaxLen` 打包，相邻片段带 `Overlap`。`MaxLen` 默认 800，小于等于 0 时按 800。`Overlap` 默认 100，0 表示默认，小于 0 时不重叠。重叠超过 `MaxLen` 一半时截到一半。单位是 `Document.Text` 的字节。`NewDropKeywords` 只用于对照，按整词且不区分大小写丢掉给定词，再切成不超过 20 字节的小块。这两种切法都把 `StartByte` 和 `EndByte` 标在 `Document.Text` 上，片段正文等于 `Text[StartByte:EndByte]`。`ChunkID` 为 `documentID + "#" + ordinal`。

`NewHeading` 的名称是 `heading`。输入是上面的 markdown。以标题行分节。`heading_path` 用 ` / ` 连接从根到当前标题的文字。节的正文不含标题行。字数按 Unicode 字符计，不计空白。不超过 800 字的节同时是父块和唯一子块。更长的节只在空行形成的段落边界切开，不在句子中间切。每块大约 400 到 800 字。相邻子块重叠大约 10% 到 15%，重叠也落在段落边界上。单个段落超过 800 字时不切开。对不齐这个重叠比例时，不为了重叠去切段落。`Options` 的字节长度不用于这种切法。

父块是 `chunk.Section`，不写入 `model.Chunk`，因此不进索引。`SplitSections` 返回各节。`NewHeading.Split` 返回各节子块按顺序拼成的 `[]model.Chunk`。同一节的子块共用 `heading_path`。ordinal 从 0 起。子块的 `StartByte` 和 `EndByte` 落在 `Document.Text` 里，`Text[StartByte:EndByte]` 包含子块正文；heading 切分不要求二者相等，因为正文已经去掉标题行。页码未知时 `PageFrom` 和 `PageTo` 为 0。现有 PDF 抽取看不到页界，不补造页码。

待嵌入的文本不在这个包里拼接。`chunk.Passage(headingPath, body)` 返回 `headingPath + "\n" + body`。`headingPath` 为空时只返回 body。

## 不做什么

不跨集合去重。不把重复文档送进索引。不把 `NewDropKeywords` 当作默认切法。不把父块塞进索引。不嵌入，不检索。扩展名或内容都认不出时返回 `ErrUnsupportedType`。名叫 docx 但 zip 里没有 `word/document.xml` 的对象也返回这个错误。

## 涉及的包

`internal/clean` 与 `internal/chunk` 只依赖 `internal/model`。字节区间断言同时用到清洗结果和切分结果。

## 数据格式

`Input` 含集合编号、对象键、来源地址、文件名和正文流。`Document.Text` 是 UTF-8。`Lineage.MediaType` 取 txt、md、html、pdf、docx。`Lineage.Encoding` 是识别到的原始编码。docx 的编码是 utf-8。`DuplicateOf` 为空表示原文。

`chunk.Section` 含 `HeadingPath`、`Body`、`PageFrom`、`PageTo` 和 `Children`。`Children` 的元素是已有的 `model.Chunk`。

`Deduper.Find` 未命中时返回空编号、false 和 nil，并且只查传入的集合。

## 测试

### T3.1 TestDetectEncoding

输入 GBK 编码的中文正文。断言 `Document.Text` 是对应的 UTF-8，码点与原文一致。`Lineage.Encoding` 非空。`Lineage.Steps` 含 `Name` 为 detect 的一步。

### T3.2 TestStripBoilerplate

输入含 nav、header、footer 和正文的 HTML。断言输出正文不含这三块样板文字，正文句子仍在。`Lineage.Steps` 含 `Name` 为 strip 的一步。

### T3.3 TestDedupePointsAtExisting

同一集合写入两篇相同正文。断言第二篇 `Duplicate` 为 true，`DuplicateOf` 等于第一篇编号，第二篇不进入索引。另一集合里的相同正文仍是原文，`Duplicate` 为 false。

### T3.4 TestLineageRoundTrip

清洗后再用 paragraph 切分。断言每个片段满足 `doc.Text[StartByte:EndByte] == chunk.Text`，`ChunkID` 等于 `documentID + "#" + ordinal`。`Lineage.RawObjectKey` 等于输入对象键，`Lineage.ChunkIDs` 包含这些片段编号。

### T3.5 TestDOCXHeadingsAndParagraphs

输入含 Heading1 到 Heading3、表格、修订删除和页眉部件的 docx。断言正文是 UTF-8 markdown，标题行是 `#`、`##`、`###`，段落之间空行，页眉和删除文字不在。`Lineage.MediaType` 为 docx。

### T3.6 TestHTMLKeepsBodyHeadings

输入含 nav、header、footer 和 h1 到 h3 的 HTML。断言样板被去掉，正文标题变成 markdown 标题行，`SplitSections` 的 `heading_path` 用 ` / ` 连接。

### T3.7 TestHeadingLongOverlap

一节超过 800 个非空白字符，且由多段组成。断言只在段落边界切开，每块大约 400 到 800 字，相邻块重叠大约 10% 到 15%。不超过 800 字的节只有一个子块。`Passage` 在路径为空时只返回正文。
