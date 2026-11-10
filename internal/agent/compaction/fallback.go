package compaction

import "strings"

// fallbackOmission 是摘要模型不可用时（PRD FR-06 异常：回退到无模型的规则式截断）
// 写进降级摘要的省略说明。
const fallbackOmission = "[Earlier context omitted to keep the fallback summary within its token budget.]"

// fallbackHistory 让上一份摘要与新的原始归档共享同一个文本预算。反复失败不得每次都再
// 追加一整份预算。产物追踪已由 Prepare 抽取并单独附加；这里先移除其旧块，避免截断标签
// 或累积过期副本。
func (c *Compactor) fallbackHistory(p *Preparation) string {
	budget := c.settings.summaryBudget()
	previous := withoutArtifacts(p.PreviousSummary)
	archive := rawArchive(p.MessagesToSummarize, budget, c.counter)
	if combined := joinNonEmpty(previous, archive); countString(c.counter, combined) <= budget {
		return combined
	}

	// 保留上一份摘要的开头（通常是其目标/约束），最多用掉一半预算。剩下的装最新的归档
	// 内容与省略说明；上一份摘要越短，留出的空间越多。
	previous = fitFallbackText(previous, func(text string) bool {
		return countString(c.counter, text) <= budget/2
	}, false)
	prefix := joinNonEmpty(previous, fallbackOmission)
	archive = fitFallbackText(archive, func(text string) bool {
		return countString(c.counter, joinNonEmpty(prefix, text)) <= budget
	}, true)
	return joinNonEmpty(prefix, archive)
}

// fitFallbackText 保留一个 rune 对齐的前缀或后缀。用 fits 提供的完整候选来度量，
// 使分隔符与省略标记也被计入。
func fitFallbackText(text string, fits func(string) bool, keepEnd bool) string {
	if fits(text) {
		return text
	}
	runes := []rune(text)
	part := func(n int) string {
		if keepEnd {
			return string(runes[len(runes)-n:])
		}
		return string(runes[:n])
	}
	low, high := 0, len(runes)
	for low < high {
		mid := low + (high-low+1)/2
		if fits(part(mid)) {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return part(low)
}

// withoutArtifacts 移除已渲染的产物追踪块，使其可以被重新附加而不会重复或截断。
func withoutArtifacts(summary string) string {
	for _, tag := range []string{producedTag, referencedTag} {
		open, closeTag := "<"+tag+">", "</"+tag+">"
		for {
			start := strings.Index(summary, open)
			if start < 0 {
				break
			}
			end := strings.Index(summary[start+len(open):], closeTag)
			if end < 0 {
				break
			}
			end += start + len(open) + len(closeTag)
			summary = summary[:start] + summary[end:]
		}
	}
	return strings.TrimSpace(summary)
}
