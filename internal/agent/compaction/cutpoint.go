package compaction

import (
	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/models/chat"
)

// CutPoint 是对话被划分为「摘要这部分」与「原样保留这部分」的位置。
type CutPoint struct {
	// FirstKeptIdx 是第一条被原样保留的消息下标。
	FirstKeptIdx int
	// TurnStartIdx 是切点所在轮次的起始 user 消息；切点正好落在轮次边界时为 -1。
	TurnStartIdx int
	// IsSplitTurn 表示切点把一个轮次切开了，因此其前半部分需要单独摘要，
	// 用来解释被保留的后半部分。
	IsSplitTurn bool
}

// isCutPointMessage 报告历史是否允许在本条消息之前切开。除工具结果之外的消息都合格。
// 工具结果被排除，是因为它必须与发起它的 assistant 消息待在一起——从两者之间切开会
// 留下孤儿工具结果，供应商会直接拒绝请求（PRD FR-06 规则 2）。
//
// 由此得到的推论正是轮次内压缩安全的原因：落在 assistant 消息上的切点会保留该消息
// 及其后的所有工具结果，因此配对从两侧都不会被拆开。
func isCutPointMessage(msg *chat.Message) bool {
	return msg.Role != "tool"
}

// isTurnStartMessage 报告本条消息是否开启一个轮次。压缩摘要本身也算：它代表其之前的
// 全部内容，所以由它开始的轮次自身就是完整的。
func isTurnStartMessage(msg *chat.Message) bool {
	return msg.Role == "user"
}

// historyStart 是压缩可以触碰的第一个下标。系统提示永远不是候选——它承载 Agent 的
// 指令，而不是它的历史。
func historyStart(messages []chat.Message) int {
	if len(messages) > 0 && messages[0].Role == "system" {
		return 1
	}
	return 0
}

// FindCutPoint 从最新消息向前累加精确 token 计数，保留仍能装进 keepRecentTokens 的
// 最大后缀。
//
// 该预算是上限而非下限。"停在第一条使预算耗尽的消息上并把它也保留"是直觉读法，也是
// 错的：单条知识检索结果动辄上万 token，于是"保留最近 16k"会交回 25k，只剩不到一轮
// 的余量就要再次压缩。
//
// 这里不考虑当前轮次。这是刻意的：预算是唯一规则，一个自身就超出预算的轮次会被切开
// 而不是被豁免。
func FindCutPoint(
	messages []chat.Message,
	start, keepRecentTokens int,
	counter *contextx.Counter,
) CutPoint {
	if start < 0 {
		start = 0
	}
	var cutPoints []int
	for i := start; i < len(messages); i++ {
		if isCutPointMessage(&messages[i]) {
			cutPoints = append(cutPoints, i)
		}
	}
	if len(cutPoints) == 0 {
		return CutPoint{FirstKeptIdx: start, TurnStartIdx: -1}
	}

	// 最坏情况下只保留最新的一组。单条消息就大于整个预算时，这里无法再切——收拾它是
	// 工具结果截断器的活，不是切点的活。
	cutIdx := cutPoints[len(cutPoints)-1]
	accumulated := 0
	for i := len(messages) - 1; i >= start; i-- {
		accumulated += CountMessage(counter, &messages[i])
		if accumulated > keepRecentTokens {
			break
		}
		// 仍在预算内，因此这是一个更好（更早）的切点。只记录合法切点，从而每个工具
		// 结果都与其 assistant 消息绑定在一起。
		if isCutPointMessage(&messages[i]) {
			cutIdx = i
		}
	}

	if isTurnStartMessage(&messages[cutIdx]) {
		return CutPoint{FirstKeptIdx: cutIdx, TurnStartIdx: -1}
	}
	turnStart := findTurnStartIdx(messages, cutIdx, start)
	return CutPoint{
		FirstKeptIdx: cutIdx,
		TurnStartIdx: turnStart,
		IsSplitTurn:  turnStart >= 0,
	}
}

// findTurnStartIdx 返回包含 entryIdx 的那个轮次的起始 user 消息下标；若该轮次起始于
// 可搜索范围之前，返回 -1。
func findTurnStartIdx(messages []chat.Message, entryIdx, start int) int {
	for i := entryIdx; i >= start; i-- {
		if isTurnStartMessage(&messages[i]) {
			return i
		}
	}
	return -1
}
