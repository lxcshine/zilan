package contextx

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAllocateStablePrefixBudgetIntentIndependent 是 FR-11 的核心不变量：
// L0（系统指令）与 L1（记忆常驻块）的 token 预算绝不能随意图分类变化，
// 否则稳定前缀的截断点会逐轮漂移，供应商侧的 prompt 缓存永远命中不了。
func TestAllocateStablePrefixBudgetIntentIndependent(t *testing.T) {
	intents := []IntentClass{IntentTech, IntentChat, IntentAnalysis, IntentGeneral, IntentClass("unknown")}

	var wantSystem, wantMemory int
	for i, intent := range intents {
		b := Allocate(128000, intent, 4096, nil)
		if i == 0 {
			wantSystem, wantMemory = b.System, b.Memory
			require.Positive(t, wantSystem)
			require.Positive(t, wantMemory)
			continue
		}
		require.Equal(t, wantSystem, b.System, "L0 budget must not depend on intent (%s)", intent)
		require.Equal(t, wantMemory, b.Memory, "L1 budget must not depend on intent (%s)", intent)
	}
}

// TestAllocateElasticLayersAbsorbIntentChanges 确认意图差异只落在 L2/L3/L4 上，
// 且五层之和不超过可用窗口。
func TestAllocateElasticLayersAbsorbIntentChanges(t *testing.T) {
	tech := Allocate(128000, IntentTech, 4096, nil)
	chat := Allocate(128000, IntentChat, 4096, nil)

	require.NotEqual(t, tech.Retrieval, chat.Retrieval, "L2 must still adapt to intent")
	require.NotEqual(t, tech.History, chat.History, "L3 must still adapt to intent")

	for _, b := range []Budget{tech, chat} {
		sum := b.System + b.Memory + b.Retrieval + b.History + b.Query
		require.Equal(t, b.Usable, sum, "layers must exactly cover the usable window")
		require.LessOrEqual(t, sum, b.Total)
	}
}

// TestAllocateIgnoresStableLayerOverrides 确认调用方无法通过 Override 改写 L0/L1
// 占比——这是"弹性只发生在 L2/L3/L4"的代码级约束。
func TestAllocateIgnoresStableLayerOverrides(t *testing.T) {
	base := Allocate(64000, IntentGeneral, 4096, nil)
	overridden := Allocate(64000, IntentGeneral, 4096, &BudgetProfile{
		System: 0.5, Memory: 0.4, Retrieval: 0.05, History: 0.03, Query: 0.02,
	})

	require.Equal(t, base.System, overridden.System, "L0 override must be ignored")
	require.Equal(t, base.Memory, overridden.Memory, "L1 override must be ignored")
	require.NotEqual(t, base.Retrieval, overridden.Retrieval, "elastic overrides still apply")
}

// TestSetLayerRejectsStablePrefixLayers 确认弹性重分配无法写入 L0/L1。
func TestSetLayerRejectsStablePrefixLayers(t *testing.T) {
	b := Allocate(32768, IntentGeneral, 4096, nil)
	system, memory := b.System, b.Memory

	b.SetLayer(LayerSystem, 1)
	b.SetLayer(LayerMemory, 1)
	require.Equal(t, system, b.System, "SetLayer must refuse to rewrite L0")
	require.Equal(t, memory, b.Memory, "SetLayer must refuse to rewrite L1")

	b.SetLayer(LayerRetrieval, 111)
	b.SetLayer(LayerHistory, 222)
	b.SetLayer(LayerQuery, 333)
	require.Equal(t, 111, b.Retrieval)
	require.Equal(t, 222, b.History)
	require.Equal(t, 333, b.Query)
	require.Equal(t, 111, b.ForLayer(LayerRetrieval))
}

// TestStableLayerBudgetsMatchesAllocate 确认记忆召回阶段（意图未知时）算出的 L1
// 额度与装配阶段完全一致，FR-20 的预算才不会出现两套口径。
func TestStableLayerBudgetsMatchesAllocate(t *testing.T) {
	for _, intent := range []IntentClass{IntentTech, IntentChat, IntentAnalysis, IntentGeneral} {
		b := Allocate(131072, intent, 8192, nil)
		system, memory := StableLayerBudgets(131072, 8192)
		require.Equal(t, b.System, system)
		require.Equal(t, b.Memory, memory)
	}
}
