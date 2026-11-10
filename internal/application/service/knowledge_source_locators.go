package service

import (
	"strings"

	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/sourceloc"
	"github.com/Tencent/WeKnora/internal/types"
)

// attachStructureBlocks 为缺少定位块的解析结果补充结构对齐（FR-05 引用溯源）。
// 优先使用解析器上报的 SourceBlocks（PDF 页级定位）；为空时按文件类型回退：
//   - office 文档：读取原始文件结构（docx 段落/表格行、pptx 形状、xlsx 行）逐单元对齐；
//   - 纯文本透传类型（md/txt/json）：按段落切分，text locator 指回原文偏移；
//   - 其余类型：不生成定位块（best-effort，不阻断解析）。
//
// 叠加而非替代：既有章节路径溯源（structure/section）逻辑不受影响。
func (s *knowledgeService) attachStructureBlocks(
	fileType string,
	fileContent []byte,
	result *types.ReadResult,
) {
	if result == nil || result.Error != "" || result.IsAudio {
		return
	}
	if len(result.SourceBlocks) > 0 {
		return // 解析器已上报页级定位块
	}
	if sourceloc.SupportsStructure(fileType) {
		if len(fileContent) == 0 {
			return // URL 模式拿不到原始字节，放弃结构对齐
		}
		units, err := sourceloc.StructureUnits(fileType, fileContent)
		if err != nil || len(units) == 0 {
			return
		}
		result.SourceBlocks = sourceloc.Align(result.MarkdownContent, units)
		return
	}
	if isPassThroughFileType(fileType) {
		result.SourceBlocks = sourceloc.TextBlocks(result.MarkdownContent)
	}
}

// isPassThroughFileType 判断解析器是否对原文做近乎透传的处理
// （此类类型的 Markdown 输出与原文共享 code point 偏移，text locator 可直接指回）。
func isPassThroughFileType(fileType string) bool {
	switch strings.ToLower(fileType) {
	case "md", "markdown", "txt", "text", "json":
		return true
	}
	return false
}

// transcriptWithSegments 将 ASR 分段转录结果拼接为 Markdown 文本，
// 每段独占一个段落（段间空行），与 transcriptSourceBlocks 的偏移一一对应。
// 无分段时原样返回全文。
func transcriptWithSegments(fullText string, segments []transcriptSegment) string {
	if len(segments) == 0 {
		return fullText
	}
	var b strings.Builder
	for _, seg := range segments {
		t := strings.TrimSpace(seg.Text)
		if t == "" {
			continue
		}
		b.WriteString(t)
		b.WriteString("\n\n")
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return fullText
	}
	return out
}

// transcriptSegment 是 ASR 分段的归一化视图（时间单位统一为毫秒）。
type transcriptSegment struct {
	Text    string
	StartMs int64
	EndMs   int64
}

// transcriptSourceBlocks 依据分段文本在拼接结果中的偏移生成 time 定位块，
// 使音频引用可定位到原始录音的时间区间。
func transcriptSourceBlocks(segments []transcriptSegment) []types.SourceBlock {
	var blocks []types.SourceBlock
	pos := 0
	for _, seg := range segments {
		t := strings.TrimSpace(seg.Text)
		if t == "" {
			continue
		}
		n := len([]rune(t))
		if seg.EndMs > seg.StartMs && n > 0 {
			blocks = append(blocks, types.SourceBlock{
				Start: pos, End: pos + n,
				Locator: types.SourceLocator{
					Type:    types.SourceLocatorTime,
					Mapping: "exact",
					StartMs: seg.StartMs,
					EndMs:   seg.EndMs,
				},
			})
		}
		pos += n + 2 // 段落文本 + "\n\n"
	}
	return blocks
}

// buildSourceIndex 建立解析器定位块 → 最终 Markdown 的定位索引（FR-05 核心钩子）。
//
// 处理管线在切分前会改写 Markdown（图片 URL 替换为 provider:// 地址），
// 记录于 parsedMarkdown 的定位块必须重新对齐到改写后的 markdown；
// 同时为每张图片标注其在原文中的位置（LocatorsAt 图片引用处），
// 使图片 OCR/描述 chunk 的引用也能回跳原文页码。
//
// best-effort：定位块为空或重对齐失败时返回 nil，调用方拿到 nil 索引
// 后所有 chunk 均无定位信息，链路照常运行。
func (s *knowledgeService) buildSourceIndex(
	knowledge *types.Knowledge,
	parsedMarkdown string,
	sourceBlocks []types.SourceBlock,
	markdown string,
	images []docparser.StoredImage,
) *sourceloc.Index {
	if len(sourceBlocks) == 0 {
		return nil
	}
	// 定位块绑定到当前源文件版本：文件更新（FileHash 变化）后旧定位失效。
	for i := range sourceBlocks {
		sourceBlocks[i].Locator.SourceHash = knowledge.FileHash
	}
	blocks := sourceloc.RemapBlocks(sourceBlocks, parsedMarkdown, markdown)
	idx := sourceloc.NewIndex(markdown, blocks)
	if idx == nil {
		return nil
	}
	for i := range images {
		at := strings.Index(markdown, images[i].ServingURL)
		if at < 0 {
			continue
		}
		images[i].SourceLocators = idx.LocatorsAt(len([]rune(markdown[:at])))
	}
	return idx
}
