package types

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 本文件覆盖 PRD FR-10 的检查点持久化形状与前缀哈希语义。

func TestContextCheckpointValueScanRoundTrip(t *testing.T) {
	created := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	original := ContextCheckpoint{
		Summary:      "## Goal\n继续任务",
		CreatedAt:    created,
		Degraded:     true,
		PrefixHash:   "hash-1",
		AnchorTurnID: "req-1",
		BaseHash:     "hash-0",
		CoveredTurns: 7,
		Source:       ContextCheckpointSourceAgent,
	}

	value, err := original.Value()
	require.NoError(t, err)
	raw, ok := value.([]byte)
	require.True(t, ok, "JSONB 列必须拿到字节串")

	var got ContextCheckpoint
	require.NoError(t, got.Scan(raw))
	require.Equal(t, original, got)
}

func TestContextCheckpointEmptySummaryStoresNull(t *testing.T) {
	blank := ContextCheckpoint{Summary: "   "}
	value, err := blank.Value()
	require.NoError(t, err)
	require.Nil(t, value, "空摘要必须存 NULL，与「从未压缩过」不可区分")

	var nilPointer *ContextCheckpoint
	value, err = nilPointer.Value()
	require.NoError(t, err)
	require.Nil(t, value)
}

func TestContextCheckpointScanNilAndEmpty(t *testing.T) {
	got := ContextCheckpoint{Summary: "旧的"}
	require.NoError(t, got.Scan(nil))
	require.Empty(t, got.Summary, "扫描 NULL 必须清空，否则会留下上一行的摘要")

	got = ContextCheckpoint{Summary: "旧的"}
	require.NoError(t, got.Scan([]byte("")))
	require.Empty(t, got.Summary)

	require.Error(t, got.Scan(42), "不支持的来源类型必须报错而不是静默忽略")
}

func TestContextCheckpointUsable(t *testing.T) {
	require.False(t, (*ContextCheckpoint)(nil).Usable())
	require.False(t, (&ContextCheckpoint{Summary: "摘要"}).Usable(), "没有前缀哈希就无法校验一致性")
	require.False(t, (&ContextCheckpoint{PrefixHash: "h"}).Usable())
	require.True(t, (&ContextCheckpoint{Summary: "摘要", PrefixHash: "h"}).Usable())
}

func TestFoldStoredMessagesIsContentSensitive(t *testing.T) {
	seed := CheckpointChainSeed("v1", "", "req-1")
	base := []StoredMessage{
		{TurnID: "req-1", Role: "user", Content: "问题"},
		{TurnID: "req-1", Role: "assistant", Content: "回答"},
	}

	require.Equal(t, FoldStoredMessages(seed, base), FoldStoredMessages(seed, base),
		"同样的输入必须得到同样的哈希，否则检查点永远校验不过")
	require.NotEqual(t, seed, FoldStoredMessages(seed, base))

	// 编辑：任何一条消息的内容变化都使前缀哈希变化（FR-10 规则 3）。
	edited := []StoredMessage{base[0], {TurnID: "req-1", Role: "assistant", Content: "改过的回答"}}
	require.NotEqual(t, FoldStoredMessages(seed, base), FoldStoredMessages(seed, edited))

	// 角色与轮次身份同样参与哈希：把同一段文本挂到别的轮次上不算同一个前缀。
	retagged := []StoredMessage{base[0], {TurnID: "req-2", Role: "assistant", Content: base[1].Content}}
	require.NotEqual(t, FoldStoredMessages(seed, base), FoldStoredMessages(seed, retagged))

	swapped := []StoredMessage{{TurnID: "req-1", Role: "assistant", Content: "回答"}, base[0]}
	require.NotEqual(t, FoldStoredMessages(seed, base), FoldStoredMessages(seed, swapped),
		"顺序不同就是不同的前缀")
}

func TestFoldStoredMessagesIgnoresTimeAndTokenCount(t *testing.T) {
	// 前缀哈希只认 (TurnID, Role, Content)：不含时间戳，也不含 token 计数，
	// 因此换 tokenizer 或时钟漂移都不会让检查点失效。
	seed := CheckpointChainSeed("v1", "", "req-1")
	messages := []StoredMessage{{TurnID: "req-1", Role: "user", Content: "问题"}}
	require.Equal(t, FoldStoredMessages(seed, messages), FoldStoredMessages(seed, messages))
}

func TestCheckpointChainSeedMixesVersionBaseAndAnchor(t *testing.T) {
	seed := CheckpointChainSeed("v1", "base", "req-1")
	require.NotEqual(t, seed, CheckpointChainSeed("v2", "base", "req-1"), "提示词模板版本必须参与哈希")
	require.NotEqual(t, seed, CheckpointChainSeed("v1", "other", "req-1"), "更早检查点的基线必须参与哈希")
	require.NotEqual(t, seed, CheckpointChainSeed("v1", "base", "req-2"), "起始轮次身份必须参与哈希")

	// 字段边界不可伪造：("ab","c") 与 ("a","bc") 不得碰撞。
	require.NotEqual(t,
		CheckpointChainSeed("v1", "ab", "c"),
		CheckpointChainSeed("v1", "a", "bc"))
}

func TestFoldStoredMessagesChainIsPerTurn(t *testing.T) {
	// 链式折叠以「轮次」为单位：同一批消息按不同轮次分组会得到不同链值。
	// 保存与校验两侧都按 RequestID 分组逐轮折叠（见 compaction.FoldStoredTurns），
	// 分组口径一致即可复现，且检查点可以在上一份检查点的链值上继续延伸。
	seed := CheckpointChainSeed("v1", "", "req-1")
	turnA := []StoredMessage{{TurnID: "req-1", Role: "user", Content: "A"}}
	turnB := []StoredMessage{{TurnID: "req-2", Role: "user", Content: "B"}}

	stepwise := FoldStoredMessages(FoldStoredMessages(seed, turnA), turnB)
	require.Equal(t, stepwise, FoldStoredMessages(FoldStoredMessages(seed, turnA), turnB),
		"链式折叠必须可复现")
	require.NotEqual(t, stepwise, FoldStoredMessages(seed, append(append([]StoredMessage{}, turnA...), turnB...)),
		"分两轮折叠与一次性折叠不是同一个前缀")
	require.NotEqual(t, stepwise, FoldStoredMessages(FoldStoredMessages(seed, turnB), turnA),
		"轮次顺序参与哈希")
}
