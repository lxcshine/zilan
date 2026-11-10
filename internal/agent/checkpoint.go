package agent

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/compaction"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// SetContextCheckpointSink 打开压缩检查点的持久化（PRD FR-10 规则 4：检查点随会话
// 持久化，会话恢复后仍可用）。nil（默认）使每次压缩只在本轮内生效。
func (e *AgentEngine) SetContextCheckpointSink(sink types.ContextCheckpointSink) {
	e.checkpointSink = sink
}

// SetHistoryCheckpointView 注入本轮装载历史得到的轮次身份视图。视图里的区间坐标对应
// 传给 Execute 的 llmContext；Execute 会把它平移到引擎消息坐标后交给压缩器。
//
// Zilan 的 chat.Message 没有轮次字段，轮次身份（RequestID）与检查点落点（assistant
// 消息 ID）只能由装载侧旁路提供。
func (e *AgentEngine) SetHistoryCheckpointView(view *compaction.HistoryView) {
	e.historyView = view
}

// attachHistoryView 把装载侧给出的轮次区间平移到引擎消息坐标，并交给压缩器。
//
// 引擎的消息布局是「系统提示 + 逐条转写的历史 + 当前 user 消息 + 本轮实时追加」，
// 历史段与 llmContext 一一对应。任何一条历史被丢弃（例如 llmContext 里混进了 system
// 消息）都会让下标错位，此时宁可不产生检查点，也不能把摘要存到错误的轮次上。
func (e *AgentEngine) attachHistoryView(ctx context.Context, messages, llmContext []chat.Message) {
	if e == nil || e.compactor == nil || e.historyView == nil {
		return
	}
	if e.historyAppended != len(llmContext) {
		logger.Warnf(ctx, "[Agent] 历史转写不是 1:1（%d/%d），本轮不产生压缩检查点",
			e.historyAppended, len(llmContext))
		e.compactor.SetHistoryView(nil)
		return
	}
	// 末尾的当前 user 消息不属于历史，因此偏移量是历史段之前的消息数。
	offset := len(messages) - e.historyAppended - 1
	if offset < 0 {
		e.compactor.SetHistoryView(nil)
		return
	}
	e.compactor.SetHistoryView(e.historyView.Shift(offset))
}

// invalidateHistoryView 丢弃本轮的轮次身份视图。
//
// 视图里的区间是消息下标：一旦消息列表被结构性改写（压缩重建、规则式截断、草稿压缩），
// 下标就不再对应原来的轮次，继续用它会把摘要写到错误的 assistant 行上。宁可本轮不再
// 产生检查点（下一轮重付一次摘要），也不能写下一份边界错误的检查点（PRD FR-10 规则 3）。
func (e *AgentEngine) invalidateHistoryView() {
	if e == nil {
		return
	}
	e.historyView = nil
	if e.compactor != nil {
		e.compactor.SetHistoryView(nil)
	}
}

// saveContextCheckpoint 把一次结束在已持久化轮次上的压缩写到那一轮（PRD FR-10 规则 1）。
// 尽力而为：写失败只是让下一轮多付一次摘要调用，绝不影响本轮。
func (e *AgentEngine) saveContextCheckpoint(ctx context.Context, cp *compaction.Checkpoint, round int) {
	if cp == nil || e == nil || e.checkpointSink == nil || cp.MessageID == "" {
		return
	}
	err := e.checkpointSink.SaveContextCheckpoint(ctx, cp.MessageID, &types.ContextCheckpoint{
		Summary:      cp.Summary,
		CreatedAt:    time.Now(),
		Degraded:     cp.Degraded,
		PrefixHash:   cp.PrefixHash,
		AnchorTurnID: cp.AnchorTurnID,
		BaseHash:     cp.BaseHash,
		CoveredTurns: cp.CoveredTurns,
		Source:       types.ContextCheckpointSourceAgent,
	})
	if err != nil {
		logger.Warnf(ctx, "[Agent][Round-%d] 压缩检查点写入失败（轮次 %s / 消息 %s）: %v",
			round, cp.TurnID, cp.MessageID, err)
		return
	}
	if cp.Degraded {
		logger.Warnf(ctx, "[Agent][Round-%d] 已保存退化的压缩检查点（覆盖 %d 轮，规则式归档 %d 字符）："+
			"摘要模型失败，下一次压缩会把它折进新摘要", round, cp.CoveredTurns, len(cp.Summary))
		return
	}
	logger.Infof(ctx, "[Agent][Round-%d] 已保存压缩检查点：覆盖到轮次 %s（%d 轮，%d 字符）",
		round, cp.TurnID, cp.CoveredTurns, len(cp.Summary))
}
