// Package compaction 把较早的对话历史替换为结构化摘要，使 Agent 的单轮请求始终
// 落在模型上下文窗口之内（PRD FR-06）。
//
// 切点只由 token 预算决定，不以"保留当前轮"为约束：一个 ReAct 轮次可能跑二十个
// round、产生十万级 token 的工具流量，拒绝触碰当前轮的压缩器无内容可压，只会每轮
// 白烧一次摘要调用却毫无效果。
//
// 与上游 WeKnora v0.9.0 的同名包保持同构，唯一实质差异是 token 计量：本包使用
// internal/contextx 的多厂商精确 tokenizer（Counter），而非上游的 BPE 估算器
// （PRD §4.9 净领先点）。
package compaction

import (
	"github.com/Tencent/WeKnora/internal/contextx"
)

// DefaultReserveTokens 是为下一次回复预留的窗口空间下限。
const DefaultReserveTokens = 16384

// DefaultKeepRecentTokens 是一次压缩后原样保留的近期对话预算。它是压缩能够终止的
// 原因：无论上下文长到多少，压缩后都约为「摘要 + 该预算」，远低于任何合理阈值，
// 因此下一轮不会立刻再次触发压缩。
const DefaultKeepRecentTokens = 20000

// Settings 配置压缩的触发时机与保留规模。
type Settings struct {
	// Enabled 打开自动压缩。关闭时仍允许显式的溢出恢复压缩——那是修复而非策略。
	Enabled bool
	// MaxContextTokens 是模型上下文窗口。为 0 时彻底禁用压缩：没有窗口就没有可
	// 执行的预算。
	MaxContextTokens int
	// ReserveTokens 是历史不得占用的那部分窗口，因为下一次回复必须放得进去。
	ReserveTokens int
	// KeepRecentTokens 是原样保留的近期消息 token 预算。
	KeepRecentTokens int
	// MaxSummaryTokens 限制摘要补全的长度。为 0 时只使用由 reserve 推导的预算。
	MaxSummaryTokens int
}

// Normalize 填充默认值，并调和无法同时满足的各项预算。
func (s Settings) Normalize() Settings {
	if s.ReserveTokens <= 0 {
		s.ReserveTokens = DefaultReserveTokens
	}
	if s.KeepRecentTokens <= 0 {
		s.KeepRecentTokens = DefaultKeepRecentTokens
	}
	// 关键不是压缩释放了多少，而是压缩后到下次压缩之间还剩多少余量。保留可用窗口的
	// 一半虽然技术上算压缩，但一两轮内就会再次触发：摘要本身还要占一块，而单条工具
	// 结果就可能上万 token。保留四分之一能让压缩后的上下文约为阈值的一半，够跑好几轮。
	//
	// 大窗口不受影响：默认保留预算本就远低于其四分之一。工具 schema 不在此扣减——
	// keepRecentTokens 是固定的对话预算，不是"扣掉工具列表后的余量"。
	if s.MaxContextTokens > 0 {
		usable := s.MaxContextTokens - s.ReserveTokens
		if quarter := usable / 4; quarter > 0 && s.KeepRecentTokens > quarter {
			s.KeepRecentTokens = quarter
		}
	}
	return s
}

// Threshold 是历史必须被压缩的上下文规模上界。它是绝对预留量而非窗口比例，因为
// 需要放下的是回复，而回复的大小不随窗口缩放。
func (s Settings) Threshold() int {
	if s.MaxContextTokens <= 0 {
		return 0
	}
	return max(s.MaxContextTokens-s.ReserveTokens, 1)
}

// ShouldCompact 报告当前上下文是否已越过阈值。
func (s Settings) ShouldCompact(contextTokens int) bool {
	if !s.Enabled {
		return false
	}
	threshold := s.Threshold()
	return threshold > 0 && contextTokens > threshold
}

// summaryBudget 是一次摘要调用的补全上限：reserveTokens 的 0.8，并受模型自身输出
// 上限约束。写死的 2000 正是长会话摘要总是断在半句的原因。
//
// 它同时受保留预算约束。摘要与保留的尾部共同占据压缩后的上下文，且后续压缩会就地
// 更新同一份摘要——没有与该预算挂钩的上限时，摘要会逐轮增长，慢慢把压缩刚释放的
// 空间重新吃回去。
func (s Settings) summaryBudget() int {
	budget := s.ReserveTokens * 4 / 5
	if s.MaxSummaryTokens > 0 && s.MaxSummaryTokens < budget {
		budget = s.MaxSummaryTokens
	}
	if s.KeepRecentTokens > 0 && s.KeepRecentTokens < budget {
		budget = s.KeepRecentTokens
	}
	return max(budget, 1024)
}

// summarizerInputBudget 是一次摘要请求能够承载的对话文本量：窗口减去回复、上一份
// 摘要与提示词自身文本，再留十分之一余量（供应商按自己的方式计数）。
// 返回 0 表示不受限（未配置窗口）。
func (s Settings) summarizerInputBudget(
	counter *contextx.Counter, previousSummary string, maxTokens int,
) int {
	if s.MaxContextTokens <= 0 {
		return 0
	}
	fixed := maxTokens + summarizerPromptTokens(counter) + countString(counter, previousSummary)
	return max((s.MaxContextTokens-fixed)*9/10, 1)
}

// summarizerPromptTokens 是包裹对话文本的提示词开销：系统提示与最长的那份指令，
// 外加其间的标签。
func summarizerPromptTokens(counter *contextx.Counter) int {
	instructions := max(
		countString(counter, initialSummarizationInstructions),
		countString(counter, updateSummarizationInstructions),
		countString(counter, turnPrefixInstructions),
	)
	return countString(counter, summarizationSystemPrompt) + instructions + summarizerPromptSlack
}

// summarizerPromptSlack 覆盖提示词在对话文本周围添加的标签与省略说明。
const summarizerPromptSlack = 256

// turnPrefixBudget 是摘要被切开轮次丢弃前缀时更小的上限：它只需解释被保留的后缀，
// 不必解释整个会话。
func (s Settings) turnPrefixBudget() int {
	return max(s.summaryBudget()/2, 512)
}
