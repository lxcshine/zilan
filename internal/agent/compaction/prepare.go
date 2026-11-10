package compaction

import (
	"strings"

	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/models/chat"
)

// 摘要以 `user` 消息注入，而不是第二条 `system`。system 消息是指令，而这是对活历史；
// 那些会合并或特殊加权 system 消息的供应商正是放错的地方。
//
// SummaryMarker 是压缩摘要消息的显眼前缀，同时承担识别职责：Zilan 的 chat.Message 没有
// Kind 字段，靠这个标记就能在后续压缩中找回上一份摘要，无需侵入消息结构体。
const SummaryMarker = "[上下文压缩]"

const (
	summaryPrefix = SummaryMarker + " 此前的对话历史与工具结果已被压缩为以下摘要，" +
		"原始内容不再逐条保留：\n\n<summary>\n"
	summarySuffix = "\n</summary>"
)

// IsSummaryMessage 报告一条消息是否是先前压缩留下的摘要占位。
func IsSummaryMessage(msg *chat.Message) bool {
	if msg.Role != "user" {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(messageText(*msg)), SummaryMarker)
}

// Preparation 是摘要步骤所需的全部内容，在任何 LLM 调用之前算好，以便调用方可以决定
// 干脆不调用。
type Preparation struct {
	// FirstKeptIdx 是原样保留的尾部起点。
	FirstKeptIdx int
	// MessagesToSummarize 是被散文替换掉的完整轮次。
	MessagesToSummarize []chat.Message
	// TurnPrefixMessages 是被切开轮次的丢弃头部，单独摘要，使保留的尾部仍有其原始请求。
	TurnPrefixMessages []chat.Message
	IsSplitTurn        bool
	// PreviousSummary 是上一次压缩的文本，就地更新而不是再摘要一遍。
	PreviousSummary string
	TokensBefore    int
	// OmittedHistory / OmittedTurnPrefix 统计各自区间中最旧、因单次摘要请求装不下而被
	// 排除的消息数（见 fitSummarizerInput）。它们的产物标识仍会被记录。
	OmittedHistory    int
	OmittedTurnPrefix int
	artifacts         artifactOps
	// historyArtifacts 是不含轮次前缀的产物集合，供检查点使用：轮次前缀描述的是下一轮
	// 仍会原样重放的那部分内容，不属于被覆盖的历史。
	historyArtifacts artifactOps
	// checkpointTurn 是被摘要区间正好结束于其上的已持久化轮次（PRD FR-10）。nil 表示
	// 区间结束在活跃轮次内部，或本轮没有可用的持久化轮次身份，于是不产生检查点。
	checkpointTurn *StoredTurn
	// AnchorTurnID / BaseHash 来自装载侧的轮次身份视图，原样写进检查点，使下一轮
	// 校验前缀时能定位到同一段区间。
	AnchorTurnID string
	BaseHash     string
}

// Prepare 选定切点并收集其周围的消息区间。
//
// 当压缩帮不上忙时返回 nil——保留预算之外没有任何消息，或上一次压缩已经无可移除。
// 这个 nil 正是对"每轮都花一次摘要调用去压一个压不动的上下文"这一失败模式的防护。
//
// view 是装载历史时算好的轮次身份视图（可为 nil），决定本次压缩能否产出可持久化的
// 检查点（PRD FR-10）。
func Prepare(
	messages []chat.Message, s Settings, counter *contextx.Counter, view *HistoryView,
) *Preparation {
	if len(messages) == 0 {
		return nil
	}
	s = s.Normalize()

	// 摘要从上一次的压缩边界之后开始。摘要消息自身被排除：把它喂回去正是让连续压缩
	// 退化成"摘要的摘要的摘要"的原因。
	boundaryStart := historyStart(messages)
	previousSummary := ""
	for i := len(messages) - 1; i >= boundaryStart; i-- {
		if !IsSummaryMessage(&messages[i]) {
			continue
		}
		previousSummary = unwrapSummary(messageText(messages[i]))
		boundaryStart = i + 1
		break
	}
	if boundaryStart >= len(messages) {
		return nil
	}

	cut := FindCutPoint(messages, boundaryStart, s.KeepRecentTokens, counter)

	historyEnd := cut.FirstKeptIdx
	if cut.IsSplitTurn {
		historyEnd = cut.TurnStartIdx
	}
	toSummarize := cloneRange(messages, boundaryStart, historyEnd)

	var turnPrefix []chat.Message
	if cut.IsSplitTurn {
		turnPrefix = cloneRange(messages, cut.TurnStartIdx, cut.FirstKeptIdx)
	}

	if len(toSummarize) == 0 && len(turnPrefix) == 0 {
		return nil
	}

	// FR-10：被压缩区间的前缀身份。链值由装载侧按持久化行 (RequestID, Role,
	// sha256(Content)) 逐轮折好，这里只在区间正好对齐到轮次边界时取出对应的一环；
	// 对齐不上（切点落在活跃轮次内部）就没有检查点。
	storedTurn := storedTurnEndingAt(view, historyEnd)

	p := &Preparation{
		FirstKeptIdx:     cut.FirstKeptIdx,
		IsSplitTurn:      cut.IsSplitTurn,
		PreviousSummary:  previousSummary,
		TokensBefore:     CountMessages(counter, messages),
		artifacts:        extractArtifacts(previousSummary, toSummarize, turnPrefix),
		historyArtifacts: extractArtifacts(previousSummary, toSummarize),
		checkpointTurn:   storedTurn,
	}
	if view != nil {
		p.AnchorTurnID = view.AnchorTurnID
		p.BaseHash = view.BaseHash
	}
	p.MessagesToSummarize, p.OmittedHistory = fitSummarizerInput(
		toSummarize, s.summarizerInputBudget(counter, previousSummary, s.summaryBudget()), counter)
	p.TurnPrefixMessages, p.OmittedTurnPrefix = fitSummarizerInput(
		turnPrefix, s.summarizerInputBudget(counter, "", s.turnPrefixBudget()), counter)
	return p
}

// checkpoint 在历史摘要正好结束于一个已持久化轮次时把它变成检查点（PRD FR-10 规则 1）。
//
// 轮次前缀摘要被排除在外：它描述的是下一轮仍会原样重放的半截轮次。没有新摘要内容
// （MessagesToSummarize 为空）意味着上一份检查点仍然成立，不必覆盖。
//
// 退化的原始归档同样保留。不保留它会让失败的摘要器变成一个循环：下一轮装载同样的
// 历史、同样压缩、同样失败。归档与摘要一样有界，且下一次压缩会把它作为上一份摘要
// 折进去，于是它是被精炼而不是被原样继承。
func (p *Preparation) checkpoint(history string, degraded bool) *Checkpoint {
	if p.checkpointTurn == nil || len(p.MessagesToSummarize) == 0 {
		return nil
	}
	return &Checkpoint{
		TurnID:       p.checkpointTurn.TurnID,
		MessageID:    p.checkpointTurn.AssistantMessageID,
		Summary:      history + p.historyArtifacts.format(),
		Degraded:     degraded,
		PrefixHash:   p.checkpointTurn.Chain,
		AnchorTurnID: p.AnchorTurnID,
		BaseHash:     p.BaseHash,
		CoveredTurns: p.checkpointTurn.CoveredTurns,
	}
}

// fitSummarizerInput 保留 transcript 能装进 budget 的最新消息，并报告丢下了多少条更旧的。
// 最新那条无论如何都保留；transcript 会逐条截断，所以单条不构成问题。
//
// 历史按设计可以长到整个窗口（装载器把溢出交给压缩处理），因此被摘要的部分可能大于
// 单次请求能承载的量。超大的请求会被拒绝，摘要退化为原始归档，而退化的摘要不会被持久化，
// 于是下一轮装载同样的历史、以同样的方式失败。丢下最旧的消息打破这个循环；提示词会
// 明说这一点，而不是把它藏起来。
func fitSummarizerInput(
	messages []chat.Message, budget int, counter *contextx.Counter,
) ([]chat.Message, int) {
	if budget <= 0 {
		return messages, 0
	}
	used := 0
	for i := len(messages) - 1; i >= 0; i-- {
		used += countString(counter, serializeConversation(messages[i:i+1]))
		if used > budget && i < len(messages)-1 {
			return messages[i+1:], i + 1
		}
	}
	return messages, 0
}

// Apply 把消息列表重建为「系统提示 + 摘要 + 原样保留的尾部」。系统提示与切点之间的
// 内容一概不留存。
func Apply(messages []chat.Message, p *Preparation, summary string) []chat.Message {
	tailStart := min(max(p.FirstKeptIdx, 0), len(messages))
	out := make([]chat.Message, 0, 2+len(messages)-tailStart)
	if historyStart(messages) == 1 {
		out = append(out, messages[0])
	}
	out = append(out, SummaryMessage(summary))
	return append(out, messages[tailStart:]...)
}

// SummaryMessage 把摘要文本包进模型看到的信封里（PRD FR-06 规则 3：被压缩的内容以明确
// 占位说明呈现，使模型知道此处曾有内容而非凭空缺失）。前缀同时是下一次压缩识别它的标记。
func SummaryMessage(summary string) chat.Message {
	return chat.Message{
		Role:    "user",
		Content: summaryPrefix + summary + summarySuffix,
	}
}

// unwrapSummary 从摘要消息中取回裸文本，以便不带信封地交给更新提示词。
func unwrapSummary(content string) string {
	start := strings.Index(content, "<summary>")
	if start < 0 {
		return strings.TrimSpace(content)
	}
	start += len("<summary>")
	end := strings.LastIndex(content, "</summary>")
	if end < start {
		return strings.TrimSpace(content[start:])
	}
	return strings.TrimSpace(content[start:end])
}

func cloneRange(messages []chat.Message, start, end int) []chat.Message {
	if start < 0 {
		start = 0
	}
	if end > len(messages) {
		end = len(messages)
	}
	if start >= end {
		return nil
	}
	out := make([]chat.Message, end-start)
	copy(out, messages[start:end])
	return out
}
