package compaction

import (
	"github.com/Tencent/WeKnora/internal/types"
)

// CheckpointPromptVersion 是混入前缀哈希的版本串：摘要信封标记（SummaryMarker）与
// 摘要提示词模板各占一段。两者任一变化，既有摘要就是按旧规格生成的，检查点必须整体
// 失效——否则下一轮会把旧格式的摘要当作自己的产物继续折叠。
const CheckpointPromptVersion = SummaryMarker + "|prompt-v1"

// Checkpoint 是一次压缩可持久化的产物（内存态）。只有当被摘要区间正好结束在一个已
// 持久化轮次上时才存在（见 Preparation.checkpoint）：结束在活跃轮次内部的摘要描述的
// 是本轮还会继续变化的内容，存下来只会立刻过期。
type Checkpoint struct {
	// TurnID 是被覆盖区间的终点轮次（types.Message.RequestID）。
	TurnID string
	// MessageID 是检查点要写入的 assistant 消息行 ID。
	MessageID string
	Summary   string
	Degraded  bool
	// PrefixHash / AnchorTurnID / BaseHash / CoveredTurns 与 types.ContextCheckpoint
	// 的同名字段一一对应。
	PrefixHash   string
	AnchorTurnID string
	BaseHash     string
	CoveredTurns int
}

// StoredTurn 是一段已持久化轮次在压缩所见消息切片中的位置与身份。
type StoredTurn struct {
	// TurnID 是轮次身份（types.Message.RequestID）。Zilan 的 chat.Message 没有轮次
	// 字段，身份只能由装载侧旁路提供。
	TurnID string
	// AssistantMessageID 是该轮的 assistant 行，也是检查点的落点。
	AssistantMessageID string
	// Start/End 是该轮的消息区间 [Start, End)，坐标与 Compact 收到的 messages 一致。
	Start, End int
	// Chain 是覆盖到本轮（含）为止的链式前缀哈希。
	Chain string
	// CoveredTurns 是覆盖到本轮（含）为止的轮次总数。
	CoveredTurns int
}

// HistoryView 是装载历史时算好的轮次身份视图（PRD FR-10）。引擎把它交给压缩器，
// 压缩器据此判断被摘要区间是否正好结束在某个已持久化轮次上，并直接取出那一环的
// 前缀哈希，无需在压缩时回读数据库。
type HistoryView struct {
	// Turns 按时间序排列，区间递增且互不重叠。
	Turns []StoredTurn
	// AnchorTurnID 是被覆盖区间的起点轮次身份。
	AnchorTurnID string
	// BaseHash 是覆盖 AnchorTurnID 之前内容的更早检查点哈希（信任，不重算）。
	BaseHash string
	// CoveredTurns 是复用检查点时已经覆盖的轮次数（不含 Turns）。
	CoveredTurns int
	// chain 是折到 Turns 末尾之后的链值。
	chain string
}

// NewHistoryView 以 anchorTurnID 为被覆盖区间起点新建视图。
//
// reusedChain 非空表示本轮复用了一份已有检查点：链从它的 PrefixHash 继续延伸，
// anchorTurnID / baseHash / coveredTurns 也照抄那份检查点，于是新检查点的可校验
// 区间与旧的连续。为空表示这是会话的第一份检查点，链从种子开始。
func NewHistoryView(anchorTurnID, baseHash, reusedChain string, coveredTurns int) *HistoryView {
	chain := reusedChain
	if chain == "" {
		chain = types.CheckpointChainSeed(CheckpointPromptVersion, baseHash, anchorTurnID)
	}
	return &HistoryView{
		AnchorTurnID: anchorTurnID,
		BaseHash:     baseHash,
		CoveredTurns: coveredTurns,
		chain:        chain,
	}
}

// AppendStoredTurn 把一个已持久化轮次追加到视图末尾，并推进链式前缀哈希。
//
// messages 是该轮参与哈希的持久化消息（时间序），由装载侧从 DB 行构造：
// (RequestID, Role, sha256(Content))。start/end 是该轮消息在装载结果中的区间。
func (v *HistoryView) AppendStoredTurn(
	turnID, assistantMessageID string, messages []types.StoredMessage, start, end int,
) {
	if v == nil || turnID == "" {
		return
	}
	v.chain = types.FoldStoredMessages(v.chain, messages)
	v.Turns = append(v.Turns, StoredTurn{
		TurnID:             turnID,
		AssistantMessageID: assistantMessageID,
		Start:              start,
		End:                end,
		Chain:              v.chain,
		CoveredTurns:       v.CoveredTurns + len(v.Turns) + 1,
	})
}

// Chain 返回折到当前末尾的链值，供调用方（增量摘要缓存）复用同一条链。
func (v *HistoryView) Chain() string {
	if v == nil {
		return ""
	}
	return v.chain
}

// FoldStoredTurns 把若干轮次（每轮一组持久化消息，时间序）依次折进链式前缀哈希。
// 装载侧用它按 DB 行重算一段被覆盖区间的哈希，与保存时逐轮延伸的结果逐字节相等。
func FoldStoredTurns(chain string, turns [][]types.StoredMessage) string {
	for _, messages := range turns {
		chain = types.FoldStoredMessages(chain, messages)
	}
	return chain
}

// Shift 返回把轮次区间整体平移 offset 的副本。装载侧给出的是历史消息坐标，而引擎
// 在历史之前还会插入系统提示，两者相差一个常量偏移。
func (v *HistoryView) Shift(offset int) *HistoryView {
	if v == nil || offset == 0 {
		return v
	}
	shifted := *v
	shifted.Turns = make([]StoredTurn, len(v.Turns))
	for i, t := range v.Turns {
		t.Start += offset
		t.End += offset
		shifted.Turns[i] = t
	}
	return &shifted
}

// storedTurnEndingAt 返回正好结束于消息下标 end 之前（即区间 [Start, end) 收尾）的那个
// 已持久化轮次；end 落在活跃轮次内部、本轮新追加的消息上，或某个轮次中间时返回 nil。
//
// 已持久化的历史永远排在活跃轮次之前，因此 end 能对齐到某个轮次边界，就意味着自上一份
// 摘要以来被压缩掉的全部是已落库的历史——检查点可以安全地写在那一轮的 assistant 行上。
func storedTurnEndingAt(view *HistoryView, end int) *StoredTurn {
	if view == nil || end <= 0 {
		return nil
	}
	for i := range view.Turns {
		if view.Turns[i].End == end {
			return &view.Turns[i]
		}
		if view.Turns[i].End > end {
			return nil
		}
	}
	return nil
}
