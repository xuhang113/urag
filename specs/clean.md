# 清洗

## 要做什么

把原始对象变成 UTF-8 正文，并留下血缘。支持 txt、md、html、pdf。识别编码后输出 UTF-8。去掉 nav、header、footer 和重复模板。按正文哈希在集合内去重。

重复文档 `Duplicate` 为 true，`Lineage.DuplicateOf` 指向已有文档，不进入索引。`Lineage.Steps` 按顺序保留每一步，`Name` 取 detect、strip、dedupe。`Lineage.RawObjectKey` 等于输入对象键。

默认切分是 `NewParagraph`：先按空行分段，再按 `MaxLen` 打包，相邻片段带 `Overlap`。`MaxLen` 默认 800，`Overlap` 默认 100，单位是 `Document.Text` 的字节。`NewDropKeywords` 只用于对照，丢掉给定词并切碎。两种切法都把 `StartByte` 和 `EndByte` 标在 `Document.Text` 上，片段正文等于 `Text[StartByte:EndByte]`。`ChunkID` 为 `documentID + "#" + ordinal`。

## 不做什么

不跨集合去重。不把重复文档送进索引。不把 `NewDropKeywords` 当作默认切法。不嵌入，不检索。不能识别的媒体类型返回 `ErrUnsupportedType`。

## 涉及的包

`internal/clean` 与 `internal/chunk` 只依赖 `internal/model`。字节区间断言同时用到清洗结果和切分结果。

## 数据格式

`Input` 含集合编号、对象键、来源地址、文件名和正文流。`Document.Text` 是 UTF-8。`Lineage.MediaType` 取 txt、md、html、pdf。`Lineage.Encoding` 是识别到的原始编码。`DuplicateOf` 为空表示原文。

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
