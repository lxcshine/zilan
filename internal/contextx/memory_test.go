package contextx

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// testMemoryPrompt 构造一个两节记忆块：一节常驻（含摘要形式），一节语义召回。
func testMemoryPrompt() MemoryPrompt {
	return MemoryPrompt{
		Header: "## 关于用户的长期记忆\n",
		Sections: []MemorySection{
			{
				Title: "### 用户档案",
				Blocks: []MemoryBlock{
					{Text: "- 用户是后端工程师", Summary: "- 用户 职业 后端工程师", Score: 1.5, Resident: true},
					{Text: "- 用户偏好使用 Go 语言编写服务端代码", Summary: "- 用户 偏好 Go", Score: 1.2, Resident: true},
				},
			},
			{
				Title: "### 相关长期记忆",
				Blocks: []MemoryBlock{
					{Text: "- 用户上周部署了监控方案", Score: 0.9},
					{Text: "- 用户提到过数据库迁移计划", Score: 0.5},
					{Text: "- 用户偶尔使用 Python 写脚本", Score: 0.1},
				},
			},
		},
	}
}

// TestRenderMemoryPromptDeterministic 是 FR-11 ④ 的守护测试：相同输入多次渲染
// 必须得到完全相同的字节（顺序固定、无时间戳、无随机性）。
func TestRenderMemoryPromptDeterministic(t *testing.T) {
	prompt := testMemoryPrompt()
	first := RenderMemoryPrompt(prompt)
	require.NotEmpty(t, first)
	for i := 0; i < 20; i++ {
		require.Equal(t, first, RenderMemoryPrompt(prompt), "memory block rendering must be byte-stable")
	}
	require.True(t, strings.HasPrefix(first, "## 关于用户的长期记忆\n"))
	require.Less(t, strings.Index(first, "### 用户档案"), strings.Index(first, "### 相关长期记忆"),
		"section order must stay fixed")
}

// TestRenderMemoryPromptEmpty 确认没有任何条目时不渲染块头。
func TestRenderMemoryPromptEmpty(t *testing.T) {
	require.Empty(t, RenderMemoryPrompt(MemoryPrompt{Header: "## 关于用户的长期记忆\n"}))
	require.Empty(t, RenderMemoryPrompt(MemoryPrompt{}))
}

// TestFitMemoryKeepsEverythingWithinBudget 预算充足时不裁剪、不降级。
func TestFitMemoryKeepsEverythingWithinBudget(t *testing.T) {
	a := NewAssembler(NewCounter(VendorGeneric))
	prompt := testMemoryPrompt()

	fit := a.FitMemory(prompt, 4096)
	require.Equal(t, RenderMemoryPrompt(prompt), fit.Text, "within budget the block must stay intact")
	require.Zero(t, fit.Dropped)
	require.False(t, fit.Degraded)
	require.Empty(t, fit.Actions)
	require.Equal(t, a.Counter.Count(fit.Text), fit.Used)
}

// TestFitMemoryDropsLowestScoreFirst 是 FR-20 ② 的守护测试：超预算时按召回评分
// 从低到高整条移除，而不是从尾部硬截断。
func TestFitMemoryDropsLowestScoreFirst(t *testing.T) {
	a := NewAssembler(NewCounter(VendorGeneric))
	prompt := testMemoryPrompt()

	full := a.Counter.Count(RenderMemoryPrompt(prompt))
	lowest := a.Counter.Count("- 用户偶尔使用 Python 写脚本")
	middle := a.Counter.Count("- 用户提到过数据库迁移计划")

	// 预算刚好容不下最低分那条 → 只丢它。
	fit := a.FitMemory(prompt, full-lowest/2)
	require.Equal(t, 1, fit.Dropped)
	require.LessOrEqual(t, fit.Used, full-lowest/2)
	require.NotContains(t, fit.Text, "Python 写脚本", "lowest-score entry goes first")
	require.Contains(t, fit.Text, "数据库迁移计划")
	require.Contains(t, fit.Text, "监控方案")
	require.False(t, fit.Degraded, "resident block is untouched while it fits the 60% rule")

	// 再收紧一档 → 按评分继续丢下一条，最高分的语义条目最后才走。
	tighter := a.FitMemory(prompt, full-lowest-middle/2)
	require.Equal(t, 2, tighter.Dropped)
	require.NotContains(t, tighter.Text, "数据库迁移计划")
	require.Contains(t, tighter.Text, "监控方案", "highest-score semantic entry survives longest")
	require.Contains(t, tighter.Text, "用户是后端工程师", "resident entries are never score-trimmed")
}

// TestFitMemoryNeverTailTruncatesEntries 确认裁剪是"整条"粒度：留下来的条目
// 必须是完整行，不会出现被截断的半句话。
func TestFitMemoryNeverTailTruncatesEntries(t *testing.T) {
	a := NewAssembler(NewCounter(VendorGeneric))
	prompt := testMemoryPrompt()
	full := a.Counter.Count(RenderMemoryPrompt(prompt))

	fit := a.FitMemory(prompt, full-4)
	require.NotContains(t, strings.Join(fit.Actions, "|"), "hard-truncated",
		"entry-wise trimming must not need the hard-truncate fallback")
	for _, line := range strings.Split(strings.TrimSpace(fit.Text), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		require.True(t, strings.HasPrefix(line, "- "), "every kept entry must be a complete bullet: %q", line)
	}
	require.Contains(t, strings.Split(fit.Text, "\n"), "- 用户上周部署了监控方案")
}

// TestFitMemoryResidentBlockTwoStates 是 FR-20 ③ 的守护测试：常驻块只有
// "整块保留"与"整块降级为摘要"两态，不存在中间长度。
func TestFitMemoryResidentBlockTwoStates(t *testing.T) {
	a := NewAssembler(NewCounter(VendorGeneric))

	long := strings.Repeat("常驻记忆内容", 20)
	prompt := MemoryPrompt{
		Header: "## 关于用户的长期记忆\n",
		Sections: []MemorySection{
			{Title: "### 用户档案", Blocks: []MemoryBlock{
				{Text: "- " + long, Summary: "- 用户 职业 工程师", Score: 2, Resident: true},
			}},
			{Title: "### 相关长期记忆", Blocks: []MemoryBlock{
				{Text: "- 用户上周部署了监控方案", Score: 0.9},
			}},
		},
	}
	residentCost := a.Counter.Count("- " + long)
	degraded := a.Counter.Count(RenderMemoryPrompt(MemoryPrompt{
		Header: prompt.Header,
		Sections: []MemorySection{
			{Title: "### 用户档案", Blocks: []MemoryBlock{{Text: "- 用户 职业 工程师"}}},
			{Title: "### 相关长期记忆", Blocks: []MemoryBlock{{Text: "- 用户上周部署了监控方案"}}},
		},
	}))

	// 状态一：预算宽裕 → 常驻块整块保留原文。
	keep := a.FitMemory(prompt, residentCost*4)
	require.False(t, keep.Degraded)
	require.Contains(t, keep.Text, long)

	// 状态二：常驻块整块成本超过预算的 60% → 整块降级为摘要，且降级后能装下全部内容。
	budget := degraded + 4
	require.Greater(t, residentCost, int(float64(budget)*ResidentKeepRatio), "test must trigger degradation")
	fit := a.FitMemory(prompt, budget)
	require.True(t, fit.Degraded)
	require.Zero(t, fit.Dropped, "degraded block must leave room for the semantic entry")
	require.Contains(t, fit.Text, "- 用户 职业 工程师")
	require.NotContains(t, fit.Text, long, "no partial original text may remain")
	require.Contains(t, fit.Text, "监控方案")
	require.LessOrEqual(t, fit.Used, budget)
	require.NotEmpty(t, fit.Actions)

	// 两态之间没有第三种形态：同一预算重复调用结果完全一致（会话粘性）。
	again := a.FitMemory(prompt, budget)
	require.Equal(t, fit.Text, again.Text)
	require.Equal(t, fit.Degraded, again.Degraded)
	require.Equal(t, fit.Dropped, again.Dropped)
}

// TestFitMemoryResidentWithoutSummaryIsDroppedWhole 常驻条目没有摘要形式时整条丢弃，
// 同样不做逐字裁剪。
func TestFitMemoryResidentWithoutSummaryIsDroppedWhole(t *testing.T) {
	a := NewAssembler(NewCounter(VendorGeneric))
	prompt := MemoryPrompt{
		Header: "H\n",
		Sections: []MemorySection{{
			Title: "### 用户档案",
			Blocks: []MemoryBlock{
				{Text: "- 一条很长的常驻记忆内容，没有任何摘要形式可以用来降级", Score: 2, Resident: true},
			},
		}},
	}
	fit := a.FitMemory(prompt, 4)
	require.True(t, fit.Degraded)
	require.Empty(t, fit.Text, "a resident entry without a summary form is dropped whole")
	require.Zero(t, fit.Used)
}

// TestFitMemoryIdempotent 相同 (记忆块, 预算) 多轮渲染结果字节一致（FR-11 ①）。
func TestFitMemoryIdempotent(t *testing.T) {
	a := NewAssembler(NewCounter(VendorGeneric))
	prompt := testMemoryPrompt()
	full := a.Counter.Count(RenderMemoryPrompt(prompt))

	first := a.FitMemory(prompt, full/2)
	for i := 0; i < 10; i++ {
		next := a.FitMemory(prompt, full/2)
		require.Equal(t, first.Text, next.Text)
		require.Equal(t, first.Dropped, next.Dropped)
		require.Equal(t, first.Degraded, next.Degraded)
	}
}

// TestFitMemoryZeroBudget 预算为 0 时不注入任何记忆，且不 panic。
func TestFitMemoryZeroBudget(t *testing.T) {
	a := NewAssembler(NewCounter(VendorGeneric))
	fit := a.FitMemory(testMemoryPrompt(), 0)
	require.Empty(t, fit.Text)
	require.NotEmpty(t, fit.Actions)
}
