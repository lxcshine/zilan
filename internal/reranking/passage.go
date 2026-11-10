package reranking

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// 本文件负责“喂给重排模型的那段文本”如何构造：先剥离 Markdown / 结构化噪声，
// 再拼接图片描述、OCR 文本与生成问题。构造结果只做模型输入与溯源用途，
// 绝不回写到 SearchResult.Content —— 原文片段必须完整保留（FR-19③）。

var (
	// reMarkdownImage 匹配 ![alt](url)，整个结构对语义相似度都是噪声。
	// URL 分组支持一层平衡括号。
	reMarkdownImage = regexp.MustCompile(`!\[[^\]]*\]\([^()\s]*(?:\([^)]*\)[^()\s]*)*\)`)
	// reLinkedImage 匹配 [![alt](img_url)](link_url)，先解包成 ![alt](img_url)
	// 以便后续 reMarkdownImage 能整体删除。
	reLinkedImage = regexp.MustCompile(
		`\[!\[([^\]]*)\]\(([^()\s]*(?:\([^)]*\)[^()\s]*)*)\)\]` +
			`\([^()\s]*(?:\([^)]*\)[^()\s]*)*\)`,
	)
	// reMarkdownLink 匹配 [text](url)，保留文本、丢弃 URL。
	reMarkdownLink = regexp.MustCompile(`\[([^\]]+)\]\([^()\s]*(?:\([^)]*\)[^()\s]*)*\)`)
	// reRawURL 匹配裸 http(s) URL。
	reRawURL = regexp.MustCompile(`https?://[^\s)\]>]+`)
	// reCodeBlock 匹配围栏代码块。
	reCodeBlock = regexp.MustCompile("(?s)```(?:\\w*)\n?.*?```")
	// reLatexBlock 匹配块级 LaTeX（$$...$$）。
	reLatexBlock = regexp.MustCompile(`(?s)\$\$.*?\$\$`)
	// reTableSep 匹配 |---|---| 这类表格分隔行。
	reTableSep = regexp.MustCompile(`(?m)^[ \t]*\|[ \t:|-]+\|[ \t]*$`)
	// reTableRow 匹配 | col1 | col2 | 这类表格数据行。
	reTableRow = regexp.MustCompile(`(?m)^[ \t]*\|(.+?)\|[ \t]*$`)
	// reHeadingPrefix 匹配标题前的 # 记号。
	reHeadingPrefix = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	// reBlockquote 匹配引用前缀 >。
	reBlockquote = regexp.MustCompile(`(?m)^>\s?`)
	// reBoldItalic3 匹配 ***text***（必须先于 2 与 1 处理）。
	reBoldItalic3 = regexp.MustCompile(`\*{3}(.+?)\*{3}`)
	// reBoldItalic2 匹配 **text**。
	reBoldItalic2 = regexp.MustCompile(`\*{2}(.+?)\*{2}`)
	// reBoldItalic1 匹配 *text*。
	reBoldItalic1 = regexp.MustCompile(`\*(.+?)\*`)
	// reExcessiveNewlines 把 3 个以上连续换行压成 2 个。
	reExcessiveNewlines = regexp.MustCompile(`\n{3,}`)
	// reListMarker 匹配无序（- * +）与有序（1.）列表前缀。
	reListMarker = regexp.MustCompile(`(?m)^[\t ]*(?:[-*+]|\d+\.)\s+`)
	// reHTMLTag 匹配 <br>、<div class="..."> 之类 HTML 标签。
	reHTMLTag = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)
)

// CleanPassage 从文本中剥离 Markdown / 结构化噪声，得到供重排模型使用的
// 干净语义片段。清洗的设计目标是保留全部自然语言内容，只去掉会干扰
// 文本相似度打分的排版记号。
func CleanPassage(text string) string {
	// 1. 先删代码块，避免后续规则命中片段
	text = reCodeBlock.ReplaceAllString(text, "")
	// 2. 删块级 LaTeX 公式
	text = reLatexBlock.ReplaceAllString(text, "")
	// 3. 删 HTML 标签
	text = reHTMLTag.ReplaceAllString(text, "")
	// 3.5 解包 [![alt](img_url)](link_url) → ![alt](img_url)
	text = reLinkedImage.ReplaceAllString(text, "![$1]($2)")
	// 4. 整体删除 Markdown 图片
	text = reMarkdownImage.ReplaceAllString(text, "")
	// 5. Markdown 链接只保留显示文本
	text = reMarkdownLink.ReplaceAllString(text, "$1")
	// 6. 删除裸 URL
	text = reRawURL.ReplaceAllString(text, "")
	// 7. 删除表格分隔行
	text = reTableSep.ReplaceAllString(text, "")
	// 7.5 表格数据行转纯文本（去掉 | 分隔符）
	text = reTableRow.ReplaceAllStringFunc(text, func(match string) string {
		inner := reTableRow.FindStringSubmatch(match)
		if len(inner) < 2 {
			return match
		}
		cells := strings.Split(inner[1], "|")
		var parts []string
		for _, cell := range cells {
			cell = strings.TrimSpace(cell)
			if cell != "" {
				parts = append(parts, cell)
			}
		}
		return strings.Join(parts, ", ")
	})
	// 8. 去掉标题记号但保留标题文本
	text = reHeadingPrefix.ReplaceAllString(text, "")
	// 9. 去掉引用记号
	text = reBlockquote.ReplaceAllString(text, "")
	// 10. 展开粗体/斜体记号，保留内部文本（顺序：*** → ** → *）
	text = reBoldItalic3.ReplaceAllString(text, "$1")
	text = reBoldItalic2.ReplaceAllString(text, "$1")
	text = reBoldItalic1.ReplaceAllString(text, "$1")
	// 11. 去掉列表记号
	text = reListMarker.ReplaceAllString(text, "")
	// 12. 压缩多余换行
	text = reExcessiveNewlines.ReplaceAllString(text, "\n\n")

	return strings.TrimSpace(text)
}

// ModelPassage 合并 Content、ImageInfo 与 GeneratedQuestions，得到重排模型
// 实际看到的文本。它是只读投影：不会修改入参行的任何字段。
func ModelPassage(ctx context.Context, result *types.SearchResult) string {
	if result == nil {
		return ""
	}
	combinedText := CleanPassage(result.Content)
	var enrichments []string

	// 解析 ImageInfo，提取图片描述与 OCR 文本
	if result.ImageInfo != "" {
		var imageInfos []types.ImageInfo
		if err := json.Unmarshal([]byte(result.ImageInfo), &imageInfos); err != nil {
			logger.Warnf(ctx, "[Rerank] failed to parse image info of chunk %s: %v", result.ID, err)
		} else {
			for _, img := range imageInfos {
				if img.Caption != "" {
					enrichments = append(enrichments, img.Caption)
				}
				if img.OCRText != "" {
					enrichments = append(enrichments, img.OCRText)
				}
			}
		}
	}

	// 解析 ChunkMetadata 里的 GeneratedQuestions
	if len(result.ChunkMetadata) > 0 {
		var docMeta types.DocumentChunkMetadata
		if err := json.Unmarshal(result.ChunkMetadata, &docMeta); err != nil {
			logger.Warnf(ctx, "[Rerank] failed to parse chunk metadata of chunk %s: %v", result.ID, err)
		} else if questionStrings := docMeta.GetQuestionStrings(); len(questionStrings) > 0 {
			enrichments = append(enrichments, strings.Join(questionStrings, "; "))
		}
	}

	if len(enrichments) == 0 {
		return combinedText
	}
	if combinedText != "" {
		combinedText += "\n\n"
	}
	return combinedText + strings.Join(enrichments, "\n")
}

// FitPassages 把超过 limit 个字符的 passage 截断（limit <= 0 表示不限）。
// passage 的顺序是“标题 → 正文 → 图片描述/OCR → 生成问题”，因此从尾部截断
// 丢掉的是最不关键的文本。厂商会拒绝超长 document，而协议层出于正确性拒绝
// 自行截断，所以一条带大段截图 OCR 的候选过去会让整批候选都拿不到重排分。
// 返回被截断的条数，供观测使用。
func FitPassages(ctx context.Context, passages []string, limit int) int {
	if limit <= 0 {
		return 0
	}
	trimmed := 0
	for i, p := range passages {
		if utf8.RuneCountInString(p) > limit {
			passages[i] = string([]rune(p)[:limit])
			trimmed++
		}
	}
	if trimmed > 0 {
		logger.Infof(ctx, "[Rerank] trimmed %d passages to the model's %d-character limit", trimmed, limit)
	}
	return trimmed
}
