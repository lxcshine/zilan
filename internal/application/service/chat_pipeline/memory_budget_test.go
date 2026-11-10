package chatpipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// memoryBudgetFakeService 实现 MEMORY_RECALL 需要的最小接口，并带上 FR-20 的
// 结构化裁剪能力（可选接口 memoryPromptFitter）。
type memoryBudgetFakeService struct {
	interfaces.MemoryService
	memories []*types.RecalledMemory
	// 记录最近一次收到的预算与 counter，供断言"归因口径是精确 tokenizer"。
	lastBudget  int
	lastCounter *contextx.Counter
}

func (f *memoryBudgetFakeService) Recall(ctx context.Context, params *types.MemoryRecallParams) ([]*types.RecalledMemory, error) {
	return f.memories, nil
}

func (f *memoryBudgetFakeService) FormatRecalledForPrompt(memories []*types.RecalledMemory) string {
	return "## 关于用户的长期记忆\n- 兜底渲染"
}

func (f *memoryBudgetFakeService) FormatRecalledForPromptWithin(
	memories []*types.RecalledMemory, counter *contextx.Counter, budgetTokens int,
) (contextx.MemoryPrompt, contextx.MemoryFitResult) {
	f.lastBudget = budgetTokens
	f.lastCounter = counter
	prompt := contextx.MemoryPrompt{Header: "## 关于用户的长期记忆\n"}
	section := contextx.MemorySection{Title: "### 相关长期记忆"}
	for _, m := range memories {
		section.Blocks = append(section.Blocks, contextx.MemoryBlock{
			Text:     "- " + m.Fact.Content,
			Score:    m.Score,
			Resident: m.Resident,
		})
	}
	prompt.Sections = append(prompt.Sections, section)
	return prompt, contextx.NewAssembler(counter).FitMemory(prompt, budgetTokens)
}

func fakeFacts(count int) []*types.RecalledMemory {
	out := make([]*types.RecalledMemory, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, &types.RecalledMemory{
			Kind: "fact",
			Fact: &types.MemoryFact{
				ID:       fmt.Sprintf("fact-%03d", i),
				Category: types.MemoryCategoryFact,
				Content:  fmt.Sprintf("记忆条目 %03d：%s", i, strings.Repeat("内容", 200)),
				Status:   types.MemoryStatusActive,
			},
			Score: float64(i) / float64(count), // 索引越大评分越高
		})
	}
	return out
}

// TestMemoryRecallAppliesTokenBudget 是 FR-20 ①②④ 的守护测试：记忆注入纳入 L1
// 预算、用精确 tokenizer 计量、超预算按评分从低到高整条移除，并把结构化记忆块
// 一并下传给五层装配器。
func TestMemoryRecallAppliesTokenBudget(t *testing.T) {
	fake := &memoryBudgetFakeService{memories: fakeFacts(40)}
	plugin := newMemoryRecallPluginForTest(fake)

	chatManage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{Query: "帮我看看部署方案", SessionID: "s-1"},
	}
	require.Nil(t, plugin.OnEvent(context.Background(), types.MEMORY_RECALL, chatManage, func() *PluginError { return nil }))

	require.NotEmpty(t, chatManage.MemoryContext)
	require.NotNil(t, chatManage.MemoryBlocks, "structured blocks must travel with the request")
	require.Positive(t, fake.lastBudget, "the L1 token budget must be resolved before rendering")
	require.NotNil(t, fake.lastCounter, "the metric must use a precise tokenizer, not char counts")

	counter := contextx.NewCounter(contextx.VendorGeneric)
	used := counter.Count(chatManage.MemoryContext)
	require.LessOrEqual(t, used, fake.lastBudget, "the injected block must fit the L1 budget")
	require.Less(t, strings.Count(chatManage.MemoryContext, "- 记忆条目"), 40, "the block must actually be trimmed")

	// 评分最低的条目先被丢弃，评分最高的保留。
	require.NotContains(t, chatManage.MemoryContext, "记忆条目 000")
	require.Contains(t, chatManage.MemoryContext, "记忆条目 039")

	// 预算与装配阶段一致：L1 占比是常量，与意图无关。
	_, expected := contextx.StableLayerBudgets(resolveContextWindow(context.Background(), chatManage), reserveOutputFor(chatManage))
	require.Equal(t, expected, fake.lastBudget)
}

// TestMemoryRecallDeterministicAcrossTurns 是 FR-11 ①④ 的守护测试：同一份记忆
// 集合在多轮之间渲染出完全相同的字节（稳定前缀的一部分）。
func TestMemoryRecallDeterministicAcrossTurns(t *testing.T) {
	memories := fakeFacts(6)
	var rendered []string
	for i := 0; i < 5; i++ {
		plugin := newMemoryRecallPluginForTest(&memoryBudgetFakeService{memories: memories})
		chatManage := &types.ChatManage{
			PipelineRequest: types.PipelineRequest{Query: fmt.Sprintf("第 %d 轮问题", i), SessionID: "s-1"},
		}
		require.Nil(t, plugin.OnEvent(context.Background(), types.MEMORY_RECALL, chatManage, func() *PluginError { return nil }))
		rendered = append(rendered, chatManage.MemoryContext)
	}
	for _, got := range rendered[1:] {
		require.Equal(t, rendered[0], got, "the memory block must not change between turns")
	}
}

// TestFiveLayerStablePrefixAcrossIntents 是 FR-11 的核心守护测试：意图分类变化
// 只影响 L2/L3/L4，system 消息（L0 系统指令 + L1 记忆常驻块）必须逐字节相同。
func TestFiveLayerStablePrefixAcrossIntents(t *testing.T) {
	base := func() *types.ChatManage {
		return &types.ChatManage{
			PipelineRequest: types.PipelineRequest{
				Query:         "帮我看看部署方案",
				SessionID:     "s-1",
				SummaryConfig: types.SummaryConfig{Prompt: "You are WeKnora.\nCurrent time: {{current_time}}\nQuery: {{query}}"},
			},
			PipelineState: types.PipelineState{
				UserContent:      "帮我看看部署方案",
				MemoryContext:    "## 关于用户的长期记忆\n- 用户是后端工程师",
				MemoryBlocks:     fakeTypesMemoryPrompt(),
				RenderedContexts: "检索片段",
				History: []*types.History{
					{Query: "上一轮问题", Answer: "上一轮回答"},
				},
			},
		}
	}

	var want string
	for i, intent := range []types.QueryIntent{types.IntentKBSearch, types.IntentChitchat, types.IntentSummarize, types.IntentGreeting} {
		chatManage := base()
		chatManage.Intent = intent
		messages := prepareMessagesFiveLayer(context.Background(), chatManage)
		require.Equal(t, "system", messages[0].Role)
		if i == 0 {
			want = messages[0].Content
			require.Contains(t, want, "You are WeKnora.")
			require.Contains(t, want, "用户是后端工程师", "L1 must stay inside the stable prefix")
			continue
		}
		require.Equal(t, want, messages[0].Content, "the stable prefix must not depend on the intent (%s)", intent)
	}
}

// TestFiveLayerKeepsQueryOutOfSystemPrompt 是 FR-11 ② 的守护测试：当前查询属于
// 逐轮变化内容，只能出现在用户消息里，不能写进 L0 系统提示。
func TestFiveLayerKeepsQueryOutOfSystemPrompt(t *testing.T) {
	chatManage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query:         "本轮唯一的查询标记 XYZ-123",
			SessionID:     "s-1",
			SummaryConfig: types.SummaryConfig{Prompt: "You are WeKnora.\nQuery: {{query}}"},
		},
		PipelineState: types.PipelineState{
			UserContent: "本轮唯一的查询标记 XYZ-123",
		},
	}

	messages := prepareMessagesFiveLayer(context.Background(), chatManage)
	require.NotContains(t, messages[0].Content, "XYZ-123", "the query must not leak into the stable prefix")
	last := messages[len(messages)-1]
	require.Equal(t, "user", last.Role)
	require.Contains(t, last.Content, "XYZ-123", "the query stays in the per-turn user message")
}

// TestNonSmartModeMemoryIsTokenBudgeted 是 FR-20 在非 smart 模式下的口径：
// 记忆块在进入 system prompt 之前已经过同一套 token 预算裁剪。
func TestNonSmartModeMemoryIsTokenBudgeted(t *testing.T) {
	fake := &memoryBudgetFakeService{memories: fakeFacts(40)}
	plugin := newMemoryRecallPluginForTest(fake)

	chatManage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query:         "帮我看看部署方案",
			SessionID:     "s-1",
			SummaryConfig: types.SummaryConfig{Prompt: "You are WeKnora."},
		},
	}
	require.Nil(t, plugin.OnEvent(context.Background(), types.MEMORY_RECALL, chatManage, func() *PluginError { return nil }))

	// 非 smart 模式（CompressionStrategy 未设置为 smart）。
	messages := prepareMessagesWithHistory(context.Background(), chatManage)
	require.Equal(t, "system", messages[0].Role)

	counter := contextx.NewCounter(contextx.VendorGeneric)
	require.LessOrEqual(t, counter.Count(chatManage.MemoryContext), fake.lastBudget,
		"the legacy path must inject a budget-trimmed memory block")
	require.Contains(t, messages[0].Content, strings.TrimSpace(chatManage.MemoryContext))
}

func fakeTypesMemoryPrompt() *types.MemoryPrompt {
	return &types.MemoryPrompt{
		Header: "## 关于用户的长期记忆\n",
		Sections: []types.MemoryPromptSection{{
			Title: "### 用户档案",
			Blocks: []types.MemoryPromptBlock{
				{Text: "- 用户是后端工程师", Summary: "- 用户 职业 工程师", Score: 1.5, Resident: true},
			},
		}},
	}
}
