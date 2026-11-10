package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/types"
)

// recalledFact 构造一条 active 的 L3 记忆。
func recalledFact(id, category, content string, score, importance float64) *types.RecalledMemory {
	return &types.RecalledMemory{
		Kind: "fact",
		Fact: &types.MemoryFact{
			ID: id, Category: category, Content: content,
			Status: types.MemoryStatusActive, Importance: importance,
		},
		Score:    score,
		Resident: types.IsResidentMemoryCategory(category),
	}
}

// TestFormatRecalledForPromptOrderIsStable 是 FR-11 ④ 的守护测试：召回顺序不同、
// 评分相同的记忆集合必须渲染出完全相同的字节（节内按 ID 稳定排序）。
func TestFormatRecalledForPromptOrderIsStable(t *testing.T) {
	svc := &memoryService{}
	a := recalledFact("a", types.MemoryCategoryFact, "用户提到过数据库迁移计划", 0.5, 0.4)
	b := recalledFact("b", types.MemoryCategoryFact, "用户上周部署了监控方案", 0.5, 0.4)
	c := recalledFact("c", types.MemoryCategoryPreference, "用户偏好 Python", 0.5, 0.4)

	forward := svc.FormatRecalledForPrompt([]*types.RecalledMemory{a, b, c})
	reverse := svc.FormatRecalledForPrompt([]*types.RecalledMemory{c, b, a})
	shuffled := svc.FormatRecalledForPrompt([]*types.RecalledMemory{b, c, a})

	require.NotEmpty(t, forward)
	require.Equal(t, forward, reverse, "recall order must not change the rendered bytes")
	require.Equal(t, forward, shuffled)
	for i := 0; i < 5; i++ {
		require.Equal(t, forward, svc.FormatRecalledForPrompt([]*types.RecalledMemory{a, b, c}))
	}
}

// TestFormatRecalledForPromptWithinTokenBudget 是 FR-20 ①② 的守护测试：
// 用精确 tokenizer 计量，超预算时按召回评分从低到高整条移除。
func TestFormatRecalledForPromptWithinTokenBudget(t *testing.T) {
	svc := &memoryService{}
	counter := contextx.NewCounter(contextx.VendorGeneric)

	high := recalledFact("h", types.MemoryCategoryFact, "高分记忆：用户负责支付网关", 0.9, 0.8)
	mid := recalledFact("m", types.MemoryCategoryFact, "中分记忆：用户上周部署了监控方案", 0.5, 0.5)
	low := recalledFact("l", types.MemoryCategoryFact, "低分记忆：用户偶尔写一点 Python 脚本", 0.1, 0.2)
	memories := []*types.RecalledMemory{high, mid, low}

	_, full := svc.FormatRecalledForPromptWithin(memories, counter, 4096)
	require.Zero(t, full.Dropped, "a generous budget keeps every entry")
	require.Contains(t, full.Text, "低分记忆")

	lowCost := counter.Count(factLine(low.Fact))
	// 预算刚好容不下最低分那条 → 只丢它。
	prompt, fit := svc.FormatRecalledForPromptWithin(memories, counter, counter.Count(full.Text)-lowCost/2)
	require.Equal(t, 1, fit.Dropped)
	require.NotContains(t, fit.Text, "低分记忆")
	require.Contains(t, fit.Text, "中分记忆")
	require.Contains(t, fit.Text, "高分记忆")
	require.LessOrEqual(t, fit.Used, counter.Count(full.Text)-lowCost/2)
	require.Equal(t, prompt.BlockCount(), 3, "the structured prompt is returned untrimmed for the assembler")

	// 预算再收紧 → 中分也走，高分最后才走。
	midCost := counter.Count(factLine(mid.Fact))
	_, tighter := svc.FormatRecalledForPromptWithin(memories, counter, counter.Count(full.Text)-lowCost-midCost/2)
	require.Equal(t, 2, tighter.Dropped)
	require.NotContains(t, tighter.Text, "中分记忆")
	require.Contains(t, tighter.Text, "高分记忆")

	// 计量口径必须是 token 而不是字符：字符数远大于 token 数时依然按 token 判定。
	require.Greater(t, len(full.Text), fit.Used, "sanity: chars and tokens use different scales")
}

// TestFormatRecalledForPromptWithinDualCapsTakeStricter 是 FR-20 ④ 的守护测试：
// 条数上限与 token 预算双重生效，取更严者。
func TestFormatRecalledForPromptWithinDualCapsTakeStricter(t *testing.T) {
	svc := &memoryService{}
	counter := contextx.NewCounter(contextx.VendorGeneric)

	// 10 条 profile 记忆：token 预算宽裕，但条数上限（memoryInjectProfileMax）更严。
	var memories []*types.RecalledMemory
	for i := 0; i < 10; i++ {
		memories = append(memories, recalledFact(
			string(rune('a'+i)), types.MemoryCategoryProfile, "用户档案条目 "+string(rune('0'+i)), 0.5, float64(i)/10))
	}
	_, fit := svc.FormatRecalledForPromptWithin(memories, counter, 8192)
	require.Zero(t, fit.Dropped, "token budget is not the binding constraint here")
	require.Equal(t, memoryInjectProfileMax, strings.Count(fit.Text, "- 用户档案条目"),
		"the per-module count cap must still apply")

	// 反过来：条数上限内，token 预算成为更严的一侧。
	few := []*types.RecalledMemory{
		recalledFact("a", types.MemoryCategoryFact, "第一条记忆内容比较长一点", 0.9, 0.5),
		recalledFact("b", types.MemoryCategoryFact, "第二条记忆内容也比较长一点", 0.1, 0.5),
	}
	_, all := svc.FormatRecalledForPromptWithin(few, counter, 8192)
	require.Contains(t, all.Text, "第二条记忆内容")
	_, capped := svc.FormatRecalledForPromptWithin(few, counter, counter.Count(all.Text)-2)
	require.Equal(t, 1, capped.Dropped, "the token budget becomes the stricter cap")
	require.NotContains(t, capped.Text, "第二条记忆内容")
}

// TestFormatRecalledForPromptWithinNeverInjectsHiddenStatus 是 FR-20 ② 的守护测试：
// pending / superseded / archived 永不注入，即使预算充足。
func TestFormatRecalledForPromptWithinNeverInjectsHiddenStatus(t *testing.T) {
	svc := &memoryService{}
	counter := contextx.NewCounter(contextx.VendorGeneric)

	pending := recalledFact("p", types.MemoryCategoryFact, "待确认的记忆内容", 0.99, 0.9)
	pending.Fact.Status = types.MemoryStatusPending
	superseded := recalledFact("s", types.MemoryCategoryProfile, "已被取代的记忆内容", 0.99, 0.9)
	superseded.Fact.Status = types.MemoryStatusSuperseded
	archived := recalledFact("x", types.MemoryCategoryFact, "已归档的记忆内容", 0.99, 0.9)
	archived.Fact.Status = types.MemoryStatusArchived
	active := recalledFact("a", types.MemoryCategoryFact, "正常生效的记忆内容", 0.1, 0.1)

	_, fit := svc.FormatRecalledForPromptWithin(
		[]*types.RecalledMemory{pending, superseded, archived, active}, counter, 8192)
	require.Contains(t, fit.Text, "正常生效的记忆内容")
	require.NotContains(t, fit.Text, "待确认的记忆内容")
	require.NotContains(t, fit.Text, "已被取代的记忆内容")
	require.NotContains(t, fit.Text, "已归档的记忆内容")
}

// TestFormatRecalledForPromptWithinResidentWholeBlockDegrade 是 FR-20 ③ 的守护测试：
// 常驻块只在"整块保留 / 整块降级为摘要"两态之间切换，不做逐字裁剪。
func TestFormatRecalledForPromptWithinResidentWholeBlockDegrade(t *testing.T) {
	svc := &memoryService{}
	counter := contextx.NewCounter(contextx.VendorGeneric)

	resident := recalledFact("r", types.MemoryCategoryProfile,
		strings.Repeat("用户是一位长期负责支付网关性能优化工作的后端工程师", 6), 1.5, 0.9)
	resident.Fact.Subject = "用户"
	resident.Fact.Predicate = "职业"
	resident.Fact.Object = "后端工程师"
	semantic := recalledFact("s", types.MemoryCategoryFact, "用户上周部署了监控方案", 0.9, 0.5)
	memories := []*types.RecalledMemory{resident, semantic}

	// 预算宽裕：常驻块整块保留原文。
	_, keep := svc.FormatRecalledForPromptWithin(memories, counter, 8192)
	require.False(t, keep.Degraded)
	require.Contains(t, keep.Text, "长期负责支付网关性能优化工作")

	// 预算压到常驻块整块成本的 60% 以下：整块降级为摘要形式（三元组）。
	residentCost := counter.Count(factLine(resident.Fact))
	budget := residentCost / 2
	_, degraded := svc.FormatRecalledForPromptWithin(memories, counter, budget)
	require.True(t, degraded.Degraded)
	require.Contains(t, degraded.Text, "- 用户 职业 后端工程师")
	require.NotContains(t, degraded.Text, "支付网关性能优化", "no partial original text may remain")
	require.NotEmpty(t, degraded.Actions)

	// 决策粘性：同一预算重复调用得到完全相同的字节。
	_, again := svc.FormatRecalledForPromptWithin(memories, counter, budget)
	require.Equal(t, degraded.Text, again.Text)
	require.Equal(t, degraded.Degraded, again.Degraded)
}

// TestFormatRecalledForPromptWithinZeroBudget 预算为 0 时不注入任何记忆。
func TestFormatRecalledForPromptWithinZeroBudget(t *testing.T) {
	svc := &memoryService{}
	_, fit := svc.FormatRecalledForPromptWithin([]*types.RecalledMemory{
		recalledFact("a", types.MemoryCategoryFact, "任意记忆", 0.5, 0.5),
	}, nil, 0)
	require.Empty(t, fit.Text)
	require.Zero(t, fit.Used)
}

// TestBuildMemoryPromptSectionOrderFixed 确认分节顺序固定（FR-11 ④）。
func TestBuildMemoryPromptSectionOrderFixed(t *testing.T) {
	svc := &memoryService{}
	prompt := svc.BuildMemoryPrompt([]*types.RecalledMemory{
		recalledFact("k", types.MemoryCategorySkill, "回答前先给结论", 1.4, 0.7),
		recalledFact("t", types.MemoryCategoryTodo, "演示部署方案", 0.7, 0.6),
		recalledFact("s", types.MemoryCategorySoul, "称呼用户为老板", 1.9, 0.9),
		recalledFact("p", types.MemoryCategoryProfile, "用户是后端工程师", 1.5, 0.8),
		{Kind: "session_summary", Summary: &types.MemorySessionSummary{ID: "sum-1", Summary: "上次讨论了监控方案"}, Score: 0.4},
	})

	rendered := contextx.RenderMemoryPrompt(prompt)
	order := []string{"### 助手风格指令（用户设定）", "### 用户档案", "### 相关长期记忆", "相关历史对话摘要：", "### 助手经验"}
	last := -1
	for _, title := range order {
		idx := strings.Index(rendered, title)
		require.Positive(t, idx, "section %q must be rendered", title)
		require.Greater(t, idx, last, "section order must be fixed")
		last = idx
	}

	// 常驻块条目必须带摘要降级形式，语义条目不带。
	for _, section := range prompt.Sections {
		for _, blk := range section.Blocks {
			if blk.Resident {
				require.NotEmpty(t, blk.Summary, "resident entries need a summary form to degrade to")
			} else {
				require.Empty(t, blk.Summary)
			}
		}
	}
}

// TestResidentSummaryLineDeterministic 确认摘要形式是纯函数且有长度上界。
func TestResidentSummaryLineDeterministic(t *testing.T) {
	fact := &types.MemoryFact{
		Subject: "用户", Predicate: "职业",
		Object:  "后端工程师",
		Content: strings.Repeat("很长的记忆内容", 30),
	}
	first := residentSummaryLine(fact)
	require.Equal(t, first, residentSummaryLine(fact))
	require.Equal(t, "- 用户 职业 后端工程师", first)
	require.LessOrEqual(t, len([]rune(first)), memorySummaryMaxRunes+3)

	// 三元组不完整时退回内容截断，同样确定性且有上界。
	contentOnly := residentSummaryLine(&types.MemoryFact{Content: strings.Repeat("长内容", 40)})
	require.Equal(t, contentOnly, residentSummaryLine(&types.MemoryFact{Content: strings.Repeat("长内容", 40)}))
	require.True(t, strings.HasSuffix(contentOnly, "…"))
	require.LessOrEqual(t, len([]rune(contentOnly)), memorySummaryMaxRunes+3)
}
