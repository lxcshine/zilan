package compaction

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	// maxSummarizationAttempts 限制一次压缩内部的重试次数。每次尝试都是一整次 LLM
	// 往返，一个不断失败的压缩更适合交给原始归档，而不是把本轮拖住。
	maxSummarizationAttempts = 2

	// llmCallLabel 在 tracing 与用量归因中标识压缩流量，把它与 Agent 自身的推理轮次
	// 区分开（PRD FR-12 的归因要求）。
	llmCallLabel = "agent_context_compaction"
)

// Reason 记录是什么触发了本次压缩，用于日志与前端展示。
type Reason string

const (
	// ReasonThreshold 是常规情况：上下文越过了预算。
	ReasonThreshold Reason = "threshold"
	// ReasonOverflow 是修复：供应商因为窗口已满而拒绝或截断了请求。
	ReasonOverflow Reason = "overflow"
	// ReasonFinalAnswer 是达到最大迭代次数后的最终答案合成前的压缩。
	ReasonFinalAnswer Reason = "final_answer"
)

// ErrNothingToCompact 表示上下文无法再变小：保留预算之外的内容已经全部清掉。调用方必须
// 把它当作停止信号而不是继续重试，否则会重现"每轮都摘要一次"的循环——这个包存在的意义
// 正是防止它。
var ErrNothingToCompact = errors.New("compaction: nothing outside the keep-recent budget")

// Result 描述一次压缩做了什么。
type Result struct {
	Messages       []chat.Message
	Summary        string
	Reason         Reason
	TokensBefore   int
	TokensAfter    int
	MessagesBefore int
	MessagesAfter  int
	// Degraded 报告至少有一份摘要来自原始归档（摘要模型失败）。上下文仍然缩小了，只是
	// 它的记忆更粗糙。调用方必须据此告知用户（PRD §5.3-3）。
	Degraded bool
	// SplitTurn 报告切点把一个轮次切开了，因此在历史摘要之外还生成了一份轮次前缀摘要。
	SplitTurn bool
	// Omitted 统计因装不下单次摘要请求而被排除的最旧消息数（见 fitSummarizerInput）。
	Omitted int
	// Checkpoint 是本次压缩可持久化的产物（PRD FR-10）：被摘要区间正好结束在一个已
	// 持久化轮次上时非 nil。调用方必须把它存下来，即使这次压缩释放的空间太小、
	// 本轮决定不采用压缩结果——被摘要掉的历史确实已经变成了摘要，下一轮不必重算。
	Checkpoint *Checkpoint
}

// Freed 是本次压缩真正回收的空间。非正值意味着摘要的开销不小于它替换掉的历史。
func (r *Result) Freed() int {
	return r.TokensBefore - r.TokensAfter
}

// Compactor 把对话历史摘要成一份检查点。
type Compactor struct {
	chatModel chat.Chat
	// counter 是 internal/contextx 的多厂商精确 tokenizer，而非上游的 BPE 估算器
	// （PRD §4.9）。
	counter  *contextx.Counter
	settings Settings
	// historyView 是本轮装载历史得到的轮次身份视图（PRD FR-10）。nil 表示没有可用的
	// 持久化轮次身份，此时压缩只在本轮内生效，不产生检查点。
	historyView *HistoryView
}

// New 构造压缩器。当压缩无法运行时返回 nil，于是调用方可以把 nil 压缩器直接当作
// "功能关闭"，不需要第二个开关。
func New(chatModel chat.Chat, counter *contextx.Counter, settings Settings) *Compactor {
	if chatModel == nil || settings.MaxContextTokens <= 0 {
		return nil
	}
	return &Compactor{
		chatModel: chatModel,
		counter:   genericCounter(counter),
		settings:  settings.Normalize(),
	}
}

// Settings 返回归一化后的配置（含推导出的阈值）。
func (c *Compactor) Settings() Settings {
	if c == nil {
		return Settings{}
	}
	return c.settings
}

// Counter 返回压缩使用的精确 token 计数器，供 engine 侧做同口径的阈值判断。
func (c *Compactor) Counter() *contextx.Counter {
	if c == nil || c.counter == nil {
		return genericCounter(nil)
	}
	return c.counter
}

// SetHistoryView 注入本轮装载历史得到的轮次身份视图（PRD FR-10）。视图里的区间坐标
// 必须与随后传给 Compact 的 messages 一致。
func (c *Compactor) SetHistoryView(view *HistoryView) {
	if c == nil {
		return
	}
	c.historyView = view
}

// HistoryView 返回当前生效的轮次身份视图（nil 表示不产生检查点）。
func (c *Compactor) HistoryView() *HistoryView {
	if c == nil {
		return nil
	}
	return c.historyView
}

// Compact 把保留预算之外的历史替换为摘要。不存在这样的历史时返回 ErrNothingToCompact。
// 整个过程只读对话消息、只调用摘要模型，绝不触碰工具执行或任何外部状态
// （PRD FR-06 规则 6、规则 7）。
func (c *Compactor) Compact(
	ctx context.Context, messages []chat.Message, reason Reason,
) (*Result, error) {
	if c == nil {
		return nil, ErrNothingToCompact
	}
	// 规则 9：用户在压缩过程中取消后，不再发起任何模型调用。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prep := Prepare(messages, c.settings, c.counter, c.historyView)
	if prep == nil {
		return nil, ErrNothingToCompact
	}

	history, historyDegraded := c.summarizeHistory(ctx, prep)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 检查点只承载历史摘要：轮次前缀摘要描述的是本轮还会原样重放的半截轮次
	// （PRD FR-10 规则 1「记录被压缩区间边界与摘要结果」）。
	checkpoint := prep.checkpoint(history, historyDegraded)
	summary, prefixDegraded := c.appendTurnPrefix(ctx, prep, history)
	summary += prep.artifacts.format()
	compacted := Apply(messages, prep, summary)

	return &Result{
		Messages:       compacted,
		Summary:        summary,
		Reason:         reason,
		TokensBefore:   prep.TokensBefore,
		TokensAfter:    CountMessages(c.counter, compacted),
		MessagesBefore: len(messages),
		MessagesAfter:  len(compacted),
		Degraded:       historyDegraded || prefixDegraded,
		SplitTurn:      prep.IsSplitTurn,
		Omitted:        prep.OmittedHistory + prep.OmittedTurnPrefix,
		Checkpoint:     checkpoint,
	}, nil
}

// summarizeHistory 把被丢弃的完整轮次折进上一份摘要；摘要器失败时退化为原始归档。
func (c *Compactor) summarizeHistory(ctx context.Context, p *Preparation) (string, bool) {
	if len(p.MessagesToSummarize) == 0 {
		return p.PreviousSummary, false
	}
	instructions := initialSummarizationInstructions
	if p.PreviousSummary != "" {
		instructions = updateSummarizationInstructions
	}
	text, err := c.summarize(
		ctx, p.MessagesToSummarize, p.OmittedHistory, p.PreviousSummary, instructions, c.settings.summaryBudget(),
	)
	if err != nil {
		return c.fallbackHistory(p), true
	}
	return text, false
}

// appendTurnPrefix 追加被切开轮次丢弃头部的摘要，使保留的尾部仍有其原始请求。
func (c *Compactor) appendTurnPrefix(ctx context.Context, p *Preparation, history string) (string, bool) {
	if !p.IsSplitTurn || len(p.TurnPrefixMessages) == 0 {
		return history, false
	}
	degraded := false
	prefix, err := c.summarize(
		ctx, p.TurnPrefixMessages, p.OmittedTurnPrefix, "", turnPrefixInstructions, c.settings.turnPrefixBudget(),
	)
	if err != nil {
		degraded = true
		prefix = rawArchive(p.TurnPrefixMessages, c.settings.turnPrefixBudget(), c.counter)
	}
	if history == "" {
		history = "No prior history."
	}
	return history + splitTurnSeparator + prefix, degraded
}

// summarize 执行一次带重试的摘要调用。请求不携带任何 tools，因此摘要过程不可能重放
// 工具调用（PRD FR-06 规则 7）。
func (c *Compactor) summarize(
	ctx context.Context,
	messages []chat.Message,
	omitted int,
	previousSummary, instructions string,
	maxTokens int,
) (string, error) {
	request := []chat.Message{
		{Role: "system", Content: summarizationSystemPrompt},
		{Role: "user", Content: buildSummarizationPrompt(messages, omitted, previousSummary, instructions)},
	}
	opts := &chat.ChatOptions{
		Temperature: 0.3, // 事实性摘要用低温度
		MaxTokens:   maxTokens,
		// 摘要是一次性调用，前缀不会跨轮复用，显式关闭缓存避免占用厂商缓存额度。
		CacheRetention: chat.CacheRetentionNone,
	}
	var lastErr error

	for attempt := 1; attempt <= maxSummarizationAttempts; attempt++ {
		// 规则 9：本轮已被取消则不再发起模型调用。
		if err := ctx.Err(); err != nil {
			return "", err
		}
		callCtx := types.WithLLMCallMetadata(ctx, llmCallLabel, "")
		resp, err := c.chatModel.Chat(callCtx, request, opts)
		if err == nil {
			err = validateSummary(resp)
		}
		if err == nil {
			return strings.TrimSpace(resp.Content), nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break // 本轮已被停止，再试一次也无济于事
		}
	}

	return "", fmt.Errorf("summarization failed after %d attempts: %w",
		maxSummarizationAttempts, lastErr)
}

// validateSummary 拒绝无法充当检查点的响应。
//
// 值得单独点出的是 length 停止：被 token 上限截断的摘要看起来像一份合法摘要，却在
// 某个小节中间悄悄结束，而之后每一轮都把它当作被丢弃历史的唯一记忆继承下去。
func validateSummary(resp *types.ChatResponse) error {
	if resp == nil {
		return errors.New("nil response from LLM")
	}
	if strings.TrimSpace(resp.Content) == "" {
		return errors.New("empty response from LLM")
	}
	if isLengthStop(resp.FinishReason) {
		return errors.New("generation hit the token cap and the summary is incomplete")
	}
	return nil
}

// buildSummarizationPrompt 把 transcript 包进标签、把指令放在最后，使摘要器不会把对话
// 文本误认为自己的指令。
func buildSummarizationPrompt(messages []chat.Message, omitted int, previousSummary, instructions string) string {
	var sb strings.Builder
	sb.WriteString("<conversation>\n")
	if omitted > 0 {
		fmt.Fprintf(&sb, "[%d earlier messages are not shown: they did not fit in one summarization "+
			"request, and no previous summary covers them. Do not guess at what they said.]\n\n", omitted)
	}
	sb.WriteString(serializeConversation(messages))
	sb.WriteString("\n</conversation>\n\n")
	if previousSummary != "" {
		sb.WriteString("<previous-summary>\n")
		sb.WriteString(previousSummary)
		sb.WriteString("\n</previous-summary>\n\n")
	}
	sb.WriteString(instructions)
	return sb.String()
}

func joinNonEmpty(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}
