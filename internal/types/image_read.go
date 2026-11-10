package types

import "strings"

// 图片读取的详略档位。brief 是默认档；full 才做完整转写（PRD FR-22 ③）。
const (
	ImageDetailBrief = "brief"
	ImageDetailFull  = "full"
)

// 图片读取提示词：普通描述有字数上限，完整转写单独一档。
const (
	ImageReadBriefPrompt = "Describe this image in at most 120 words. " +
		"Include its type, key labels, numbers and relationships. " + ImageReadChartPrompt +
		"Do not transcribe long passages. Say when content is illegible; do not guess. Plain text only."
	ImageReadFullPrompt = "Read this image closely. Transcribe readable text and all chart/table values; " +
		"describe diagram elements and relationships. " + ImageReadChartPrompt +
		"Mark illegible content rather than guessing. Plain text only."
	// 图表观察必须保留线性 OCR 会丢掉的空间对应关系。
	ImageReadChartPrompt = "For charts and tables, associate every reported value with its category, " +
		"series/legend and unit, using one labelled row per category. Distinguish axis tick labels from data values. " +
		"Never infer these associations from text reading order; mark ambiguous labels or values as unclear. " +
		"Keep observed values separate from estimates or calculations. "
	ImageReadFocusRunes = 300
)

// NormalizeImageRead 补齐 brief 默认值，并限制可选 focus 的长度。
// 归一化后的 (detail, focus) 才参与图片理解缓存键，避免同一张图因空白差异
// 产生两份缓存（PRD FR-22 ⑤）。
func NormalizeImageRead(detail, focus string) (string, string) {
	if detail != ImageDetailFull {
		detail = ImageDetailBrief
	}
	focus = strings.TrimSpace(focus)
	if runes := []rune(focus); len(runes) > ImageReadFocusRunes {
		focus = string(runes[:ImageReadFocusRunes])
	}
	return detail, focus
}

// ImageReadPrompt 由附件图片读取与通用工具图片兜底共用。
func ImageReadPrompt(detail, focus string) string {
	detail, focus = NormalizeImageRead(detail, focus)
	prompt := ImageReadBriefPrompt
	if detail == ImageDetailFull {
		prompt = ImageReadFullPrompt
	}
	if focus != "" {
		prompt += "\nThe assistant is looking for: " + focus +
			"\nAddress that first, saying plainly if the image does not show it."
	}
	return prompt
}
