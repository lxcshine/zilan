package contextx

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAssembleStablePrefixIdenticalAcrossIntents 是 FR-11 的端到端守护测试：
// 稳定前缀（L0 系统指令 + L1 记忆常驻块）的渲染结果不得随意图分类变化，
// 否则供应商侧的 prompt 前缀缓存每轮都会 miss。
func TestAssembleStablePrefixIdenticalAcrossIntents(t *testing.T) {
	a := NewAssembler(NewCounter(VendorGeneric))
	base := Input{
		System: "You are WeKnora.\nAlways answer in Chinese.",
		MemoryPrompt: MemoryPrompt{
			Header: "## 关于用户的长期记忆\n",
			Sections: []MemorySection{{
				Title: "### 用户档案",
				Blocks: []MemoryBlock{
					{Text: "- 用户是后端工程师", Summary: "- 用户 职业 工程师", Score: 1.5, Resident: true},
				},
			}},
		},
		Retrieval:     strings.Repeat("检索片段。", 200),
		History:       []Turn{{User: "上一轮问题", Assistant: "上一轮回答"}},
		Query:         "本轮问题",
		Window:        32768,
		ReserveOutput: 4096,
	}

	var wantSystem, wantMemory string
	for i, intent := range []IntentClass{IntentTech, IntentChat, IntentAnalysis, IntentGeneral} {
		in := base
		in.Intent = intent
		got := a.Assemble(in)
		if i == 0 {
			wantSystem, wantMemory = got.System, got.Memory
			require.NotEmpty(t, wantSystem)
			require.NotEmpty(t, wantMemory)
			continue
		}
		require.Equal(t, wantSystem, got.System, "L0 must be byte-identical across intents (%s)", intent)
		require.Equal(t, wantMemory, got.Memory, "L1 must be byte-identical across intents (%s)", intent)
	}
}

// TestAssembleIsIdempotent 相同输入重复装配必须得到逐字节相同的输出（幂等），
// 这是"多轮渲染字节相同"的最小可验证单元。
func TestAssembleIsIdempotent(t *testing.T) {
	a := NewAssembler(NewCounter(VendorGeneric))
	in := Input{
		System: strings.Repeat("系统指令。", 50),
		Memory: "## 关于用户的长期记忆\n- 用户是后端工程师",
		// 故意让 L0/L1 超预算，覆盖截断分支的幂等性。
		Retrieval: strings.Repeat("检索片段。", 500),
		History: []Turn{
			{User: "问题一", Assistant: "回答一"},
			{User: "问题二", Assistant: "回答二", Sticky: true},
		},
		HistorySummary: "早前对话摘要内容",
		Query:          "本轮问题",
		Intent:         IntentGeneral,
		Window:         8192,
		ReserveOutput:  4096,
	}

	first := a.Assemble(in)
	for i := 0; i < 5; i++ {
		next := a.Assemble(in)
		require.Equal(t, first.System, next.System)
		require.Equal(t, first.Memory, next.Memory)
		require.Equal(t, first.Retrieval, next.Retrieval)
		require.Equal(t, first.Query, next.Query)
		require.Equal(t, first.History, next.History)
		require.Equal(t, first.Diag.UsedByLayer, next.Diag.UsedByLayer)
	}
}

// TestAssembleDiagnosticsNeverLeakIntoPrompt 确认 Diagnostics.Actions 只用于
// 可观测，绝不会出现在任何一层的渲染结果里（FR-11：诊断信息不得进入 prompt）。
func TestAssembleDiagnosticsNeverLeakIntoPrompt(t *testing.T) {
	a := NewAssembler(NewCounter(VendorGeneric))
	got := a.Assemble(Input{
		System:         strings.Repeat("系统指令。", 400),
		Memory:         strings.Repeat("记忆条目。", 400),
		Retrieval:      strings.Repeat("检索片段。", 400),
		History:        []Turn{{User: strings.Repeat("问题。", 400), Assistant: strings.Repeat("回答。", 400)}},
		HistorySummary: strings.Repeat("摘要。", 200),
		Query:          strings.Repeat("本轮问题。", 200),
		Intent:         IntentTech,
		Window:         8192,
		ReserveOutput:  4096,
	})

	require.NotEmpty(t, got.Diag.Actions, "over-budget input must produce diagnostics")
	for _, action := range got.Diag.Actions {
		for _, rendered := range []string{got.System, got.Memory, got.Retrieval, got.Query} {
			require.NotContains(t, rendered, action)
		}
		for _, turn := range got.History {
			require.NotContains(t, turn.User, action)
			require.NotContains(t, turn.Assistant, action)
		}
	}
	require.NotContains(t, got.System, "truncated")
	require.NotContains(t, got.Memory, "yielded")
}

// TestAssembleUsesStructuredMemoryBlocks 确认 L1 走结构化相关性裁剪：超预算时
// 低分条目被整条移除，高分条目完整保留，而不是把渲染文本从尾部切断。
func TestAssembleUsesStructuredMemoryBlocks(t *testing.T) {
	a := NewAssembler(NewCounter(VendorGeneric))

	high := MemoryPrompt{
		Header: "## 关于用户的长期记忆\n",
		Sections: []MemorySection{{Title: "### 相关长期记忆", Blocks: []MemoryBlock{
			{Text: "- 高分记忆条目", Score: 0.9},
		}}},
	}
	prompt := MemoryPrompt{
		Header: high.Header,
		Sections: []MemorySection{{Title: "### 相关长期记忆", Blocks: []MemoryBlock{
			{Text: "- 高分记忆条目", Score: 0.9},
			{Text: "- " + strings.Repeat("低分记忆条目", 100), Score: 0.1},
		}}},
	}
	highOnly := a.Counter.Count(RenderMemoryPrompt(high))
	full := a.Counter.Count(RenderMemoryPrompt(prompt))
	require.Greater(t, full, highOnly)

	// L1 = 10% × usable，反推出一个"装得下高分条目、装不下低分条目"的窗口。
	// 窗口必须大于 minUsableWindow，否则 usable 会被下限抬高，反推失效。
	target := highOnly + (full-highOnly)/2
	require.Greater(t, target*10, minUsableWindow)
	got := a.Assemble(Input{
		System:        "sys",
		MemoryPrompt:  prompt,
		Memory:        "不应被使用的兜底文本",
		Query:         "q",
		Intent:        IntentGeneral,
		Window:        target*10 + 1024,
		ReserveOutput: 1024,
	})

	require.InDelta(t, target, got.Diag.BudgetByLayer[LayerMemory], 2)
	require.Equal(t, 1, got.MemoryDropped, "the lowest-score entry must be removed whole")
	require.Contains(t, got.Memory, "高分记忆条目")
	require.NotContains(t, got.Memory, "低分记忆条目")
	require.NotContains(t, got.Memory, "不应被使用的兜底文本")
	require.Contains(t, strings.Join(got.Diag.Actions, "|"), "L1 dropped 1 lowest-score memory entries")
}

// TestAssembleFallsBackToMemoryText 没有结构化记忆块时，L1 退回文本口径并被
// 预算约束（兜底行为，保证任何输入都不会突破 L1 预算）。
func TestAssembleFallsBackToMemoryText(t *testing.T) {
	a := NewAssembler(NewCounter(VendorGeneric))
	memory := strings.Repeat("记忆条目。", 400)

	got := a.Assemble(Input{
		System:        "sys",
		Memory:        memory,
		Query:         "q",
		Intent:        IntentGeneral,
		Window:        8192,
		ReserveOutput: 4096,
	})

	budget := got.Diag.BudgetByLayer[LayerMemory]
	require.Positive(t, budget)
	require.LessOrEqual(t, got.Diag.UsedByLayer[LayerMemory], budget)
	require.Less(t, len(got.Memory), len(memory), "over-budget memory text must be truncated")
	require.Zero(t, got.MemoryDropped)
	require.False(t, got.MemoryDegraded)
}

// TestAssembleMemorySlackYieldsToElasticLayers 确认 L1 未用满的额度按块粒度让渡给
// L2/L3（FR-20 ③），且让渡额等于 L1 预算减去记忆实际用量。
func TestAssembleMemorySlackYieldsToElasticLayers(t *testing.T) {
	a := NewAssembler(NewCounter(VendorGeneric))
	in := Input{
		System:        "sys",
		MemoryPrompt:  MemoryPrompt{Header: "H\n", Sections: []MemorySection{{Title: "T", Blocks: []MemoryBlock{{Text: "- 一条记忆", Score: 1}}}}},
		Retrieval:     strings.Repeat("检索片段。", 2000),
		History:       []Turn{{User: strings.Repeat("问题。", 2000), Assistant: strings.Repeat("回答。", 2000)}},
		Query:         "q",
		Intent:        IntentGeneral,
		Window:        32768,
		ReserveOutput: 4096,
	}

	got := a.Assemble(in)
	memorySlack := got.Diag.BudgetByLayer[LayerMemory] - got.Diag.UsedByLayer[LayerMemory]
	require.Positive(t, memorySlack, "a small memory block must leave slack")

	elastic := got.Diag.BudgetByLayer[LayerRetrieval] + got.Diag.BudgetByLayer[LayerHistory]
	allocated := Allocate(in.Window, in.Intent, in.ReserveOutput, nil)
	require.Greater(t, elastic, allocated.Retrieval+allocated.History,
		"the L1 slack must be yielded to the elastic layers")
	require.Contains(t, strings.Join(got.Diag.Actions, "|"), "yielded")
}
