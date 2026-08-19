package chatpipeline

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// memoryRecallFakeService implements interfaces.MemoryService with canned
// recall output so the plugin test needs no database.
type memoryRecallFakeService struct {
	interfaces.MemoryService
	memories  []*types.RecalledMemory
	err       error
	lastQuery string
	calls     int
}

func (f *memoryRecallFakeService) Recall(ctx context.Context, params *types.MemoryRecallParams) ([]*types.RecalledMemory, error) {
	f.calls++
	f.lastQuery = params.Query
	return f.memories, f.err
}

func (f *memoryRecallFakeService) FormatRecalledForPrompt(memories []*types.RecalledMemory) string {
	if len(memories) == 0 {
		return ""
	}
	return "## 关于用户的长期记忆\n- 用户偏好 Python"
}

func newMemoryRecallPluginForTest(fake interfaces.MemoryService) *PluginMemoryRecall {
	return &PluginMemoryRecall{memoryService: fake}
}

func TestPluginMemoryRecallInjectsContext(t *testing.T) {
	fake := &memoryRecallFakeService{
		memories: []*types.RecalledMemory{
			{Kind: "fact", Fact: &types.MemoryFact{Content: "用户偏好 Python"}, Score: 0.9},
		},
	}
	plugin := newMemoryRecallPluginForTest(fake)

	chatManage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{Query: "async 框架怎么选", SessionID: "s-1"},
	}
	var continued bool
	err := plugin.OnEvent(context.Background(), types.MEMORY_RECALL, chatManage, func() *PluginError {
		continued = true
		return nil
	})

	require.Nil(t, err)
	require.True(t, continued, "plugin must always continue the pipeline")
	require.Equal(t, 1, fake.calls)
	require.Equal(t, "async 框架怎么选", fake.lastQuery)
	require.Contains(t, chatManage.MemoryContext, "用户偏好 Python")
}

func TestPluginMemoryRecallNoMatchLeavesContextEmpty(t *testing.T) {
	fake := &memoryRecallFakeService{} // no memories
	plugin := newMemoryRecallPluginForTest(fake)

	chatManage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{Query: "hello", SessionID: "s-1"},
	}
	err := plugin.OnEvent(context.Background(), types.MEMORY_RECALL, chatManage, func() *PluginError { return nil })
	require.Nil(t, err)
	require.Empty(t, chatManage.MemoryContext)
}

func TestPluginMemoryRecallErrorDoesNotFailPipeline(t *testing.T) {
	fake := &memoryRecallFakeService{err: fmt.Errorf("db down")}
	plugin := newMemoryRecallPluginForTest(fake)

	chatManage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{Query: "hello", SessionID: "s-1"},
	}
	var continued bool
	err := plugin.OnEvent(context.Background(), types.MEMORY_RECALL, chatManage, func() *PluginError {
		continued = true
		return nil
	})
	require.Nil(t, err, "recall failure must not fail the turn")
	require.True(t, continued)
	require.Empty(t, chatManage.MemoryContext)
}

func TestPluginMemoryRecallEmptyQuerySkips(t *testing.T) {
	fake := &memoryRecallFakeService{memories: []*types.RecalledMemory{{Kind: "fact", Fact: &types.MemoryFact{Content: "x"}}}}
	plugin := newMemoryRecallPluginForTest(fake)

	chatManage := &types.ChatManage{PipelineRequest: types.PipelineRequest{Query: "   "}}
	err := plugin.OnEvent(context.Background(), types.MEMORY_RECALL, chatManage, func() *PluginError { return nil })
	require.Nil(t, err)
	require.Equal(t, 0, fake.calls, "empty query must not hit the memory store")
}

func TestPluginMemoryRecallNilServiceNoop(t *testing.T) {
	plugin := newMemoryRecallPluginForTest(nil)
	chatManage := &types.ChatManage{PipelineRequest: types.PipelineRequest{Query: "hello"}}
	var continued bool
	err := plugin.OnEvent(context.Background(), types.MEMORY_RECALL, chatManage, func() *PluginError {
		continued = true
		return nil
	})
	require.Nil(t, err)
	require.True(t, continued)
	require.Empty(t, chatManage.MemoryContext)
}

func TestPrepareMessagesInjectsMemoryContext(t *testing.T) {
	chatManage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query:         "帮我看看部署方案",
			SummaryConfig: types.SummaryConfig{Prompt: "You are a helpful assistant."},
		},
		PipelineState: types.PipelineState{
			MemoryContext: "## 关于用户的长期记忆\n- 用户负责 Project X（截止 2026-08-20）",
		},
	}
	messages := prepareMessagesWithHistory(context.Background(), chatManage)
	require.NotEmpty(t, messages)
	require.Equal(t, "system", messages[0].Role)
	require.Contains(t, messages[0].Content, "You are a helpful assistant.")
	require.Contains(t, messages[0].Content, "用户负责 Project X")
}

func TestPrepareMessagesWithoutMemoryUnchanged(t *testing.T) {
	chatManage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query:         "hi",
			SummaryConfig: types.SummaryConfig{Prompt: "base prompt"},
		},
	}
	messages := prepareMessagesWithHistory(context.Background(), chatManage)
	require.Equal(t, "base prompt", messages[0].Content)
}

// Ensure recalled memories survive a ChatManage clone (agent mode clones the
// chat manage between stages).
func TestChatManageCloneCarriesMemoryContext(t *testing.T) {
	chatManage := &types.ChatManage{
		PipelineState: types.PipelineState{MemoryContext: "mem-block"},
	}
	cloned := chatManage.Clone()
	require.Equal(t, "mem-block", cloned.MemoryContext)
	cloned.MemoryContext = "changed"
	require.Equal(t, "mem-block", chatManage.MemoryContext)
}
