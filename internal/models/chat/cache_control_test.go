package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/models/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClampPromptCacheKey(t *testing.T) {
	assert.Empty(t, ClampPromptCacheKey(""))
	assert.Equal(t, "session-1", ClampPromptCacheKey("session-1"))

	long := strings.Repeat("a", 100)
	assert.Len(t, ClampPromptCacheKey(long), 64, "OpenAI accepts at most 64 characters")
}

func TestCacheControlFor(t *testing.T) {
	assert.Nil(t, CacheControlFor(CacheRetentionNone, LongCacheTTL), "none disables the marker")
	assert.Equal(t, &CacheControlMarker{Type: "ephemeral"}, CacheControlFor(CacheRetentionShort, LongCacheTTL))
	assert.Equal(t, &CacheControlMarker{Type: "ephemeral", TTL: "1h"}, CacheControlFor(CacheRetentionLong, LongCacheTTL))
	assert.Equal(t, CacheRetentionShort, ResolveCacheRetention(nil), "empty retention defaults to short")
	assert.Equal(t, CacheRetentionLong, ResolveCacheRetention(&ChatOptions{CacheRetention: CacheRetentionLong}))
}

func TestApplyCacheControlBreakpoints(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "stable instructions"},
			map[string]any{"role": "user", "content": "first turn"},
			map[string]any{"role": "assistant", "content": "first answer"},
			map[string]any{"role": "user", "content": "second turn"},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "a"}},
			map[string]any{"type": "function", "function": map[string]any{"name": "b"}},
		},
	}

	ApplyCacheControlBreakpoints(payload, CacheControlFor(CacheRetentionLong, LongCacheTTL))

	messages := payload["messages"].([]any)
	system := messages[0].(map[string]any)
	parts, ok := system["content"].([]any)
	require.True(t, ok, "the instruction message must be rewritten to a content block array")
	require.Len(t, parts, 1)
	block := parts[0].(map[string]any)
	assert.Equal(t, "stable instructions", block["text"])
	marker, ok := block["cache_control"].(*CacheControlMarker)
	require.True(t, ok)
	assert.Equal(t, "ephemeral", marker.Type)
	assert.Equal(t, "1h", marker.TTL)

	// 中间的会话消息不打断点，只有最后一条打：断点框住的是稳定前缀的边界。
	assert.Equal(t, "first turn", messages[1].(map[string]any)["content"])
	last := messages[3].(map[string]any)
	lastParts, ok := last["content"].([]any)
	require.True(t, ok, "the last conversation message carries the incremental breakpoint")
	assert.NotNil(t, lastParts[0].(map[string]any)["cache_control"])

	tools := payload["tools"].([]any)
	assert.NotNil(t, tools[1].(map[string]any)["cache_control"], "the last tool definition is part of the prefix")
	assert.Nil(t, tools[0].(map[string]any)["cache_control"])
}

func TestApplyCacheControlBreakpointsNilMarkerNoop(t *testing.T) {
	payload := map[string]any{
		"messages": []any{map[string]any{"role": "system", "content": "instructions"}},
	}
	ApplyCacheControlBreakpoints(payload, nil)
	assert.Equal(t, "instructions", payload["messages"].([]any)[0].(map[string]any)["content"])
}

func TestResolvePromptCacheSupport(t *testing.T) {
	tests := []struct {
		name        string
		provider    provider.ProviderName
		model       string
		extra       map[string]string
		wantKey     bool
		wantControl bool
	}{
		{name: "openai key only", provider: provider.ProviderOpenAI, model: "gpt-4o", wantKey: true},
		{name: "azure key only", provider: provider.ProviderAzureOpenAI, model: "gpt-4o", wantKey: true},
		{name: "aliyun cache_control", provider: provider.ProviderAliyun, model: "qwen3-max", wantControl: true},
		{name: "openrouter claude", provider: provider.ProviderOpenRouter, model: "anthropic/claude-sonnet-4.5", wantKey: true, wantControl: true},
		{name: "openrouter non-claude", provider: provider.ProviderOpenRouter, model: "openai/gpt-4o", wantKey: true},
		{name: "deepseek unverified", provider: provider.ProviderDeepSeek, model: "deepseek-chat"},
		{name: "generic unverified", provider: provider.ProviderGeneric, model: "qwen2.5"},
		{
			name: "extra config forces anthropic markers", provider: provider.ProviderDeepSeek, model: "deepseek-chat",
			extra: map[string]string{"prompt_cache": "anthropic"}, wantControl: true,
		},
		{
			name: "extra config disables everything", provider: provider.ProviderOpenAI, model: "gpt-4o",
			extra: map[string]string{"prompt_cache": "off"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvePromptCacheSupport(tt.provider, tt.model, tt.extra)
			assert.Equal(t, tt.wantKey, got.CacheKey)
			assert.Equal(t, tt.wantControl, got.CacheControl)
		})
	}
}

func TestApplyPromptCacheMarkers(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"messages": []any{map[string]any{"role": "system", "content": "instructions"}}}
	}
	support := promptCacheSupport{CacheKey: true, CacheControl: true}

	// 默认（短缓存）：写路由键 + 打断点，不写 retention 字段。
	payload := base()
	applyPromptCacheMarkers(payload, &ChatOptions{PromptCacheKey: "session-42"}, support)
	assert.Equal(t, "session-42", payload["prompt_cache_key"])
	assert.NotContains(t, payload, "prompt_cache_retention")
	assert.NotEqual(t, "instructions", payload["messages"].([]any)[0].(map[string]any)["content"],
		"cache_control breakpoints must be applied")

	// 长缓存：额外写 retention。
	payload = base()
	applyPromptCacheMarkers(payload, &ChatOptions{PromptCacheKey: "s", CacheRetention: CacheRetentionLong}, support)
	assert.Equal(t, LongCacheRetentionField, payload["prompt_cache_retention"])

	// 显式关缓存：什么都不写（摘要/压缩类一次性调用）。
	payload = base()
	applyPromptCacheMarkers(payload, &ChatOptions{PromptCacheKey: "s", CacheRetention: CacheRetentionNone}, support)
	assert.NotContains(t, payload, "prompt_cache_key")
	assert.Equal(t, "instructions", payload["messages"].([]any)[0].(map[string]any)["content"])

	// 供应商不支持：保持请求原样。
	payload = base()
	applyPromptCacheMarkers(payload, &ChatOptions{PromptCacheKey: "s"}, promptCacheSupport{})
	assert.NotContains(t, payload, "prompt_cache_key")
	assert.Equal(t, "instructions", payload["messages"].([]any)[0].(map[string]any)["content"])
}

func TestPromptCacheRoutingKey(t *testing.T) {
	assert.Empty(t, promptCacheRoutingKey(nil))
	assert.Empty(t, promptCacheRoutingKey(&ChatOptions{PromptCacheKey: "s", CacheRetention: CacheRetentionNone}))
	assert.Equal(t, "session-1", promptCacheRoutingKey(&ChatOptions{PromptCacheKey: "session-1"}))
}

func TestAttachSessionAffinityHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/chat/completions", nil)
	require.NoError(t, err)

	AttachSessionAffinityHeaders(req, "")
	assert.Empty(t, req.Header.Get("session_id"), "no key -> no headers")

	AttachSessionAffinityHeaders(req, "sess-9")
	assert.Equal(t, "sess-9", req.Header.Get("session_id"))
	assert.Equal(t, "sess-9", req.Header.Get("x-client-request-id"))
	assert.Equal(t, "sess-9", req.Header.Get("x-session-affinity"))
}

// TestSessionAffinityDoerInjectsHeadersFromContext 覆盖 SDK 路径：路由键放在
// 上下文里，由包装后的 HTTP 客户端在发出请求前附加 header。
func TestSessionAffinityDoerInjectsHeadersFromContext(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := wrapSessionAffinityClient(nil)
	ctx := withSessionAffinity(context.Background(), "sess-1")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, "sess-1", got.Get("x-session-affinity"))

	// 没有路由键时完全透传，不附加任何 header。
	req, err = http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Empty(t, got.Get("x-session-affinity"))
}

func TestAnthropicContentMarshal(t *testing.T) {
	// 无缓存标记：输出纯字符串，与改造前的请求字节一致。
	data, err := json.Marshal(newAnthropicText("hello"))
	require.NoError(t, err)
	assert.Equal(t, `"hello"`, string(data))

	// 有缓存标记：输出内容块数组。
	marker := CacheControlFor(CacheRetentionShort, LongCacheTTL)
	data, err = json.Marshal(newAnthropicCachedText("hello", marker))
	require.NoError(t, err)
	assert.JSONEq(t, `[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]`, string(data))

	// 反序列化同时接受两种写法。
	var fromString, fromBlocks anthropicContent
	require.NoError(t, json.Unmarshal([]byte(`"plain"`), &fromString))
	assert.Equal(t, "plain", fromString.Text)
	require.NoError(t, json.Unmarshal(data, &fromBlocks))
	require.Len(t, fromBlocks.Blocks, 1)
	assert.Equal(t, "hello", fromBlocks.Blocks[0].Text)
}

func TestAnthropicBuildRequestCacheBreakpoints(t *testing.T) {
	c := &AnthropicChat{modelName: "claude-sonnet-4-5"}
	messages := []Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hi"},
	}

	// 默认（短缓存）：system 与最后一条会话消息都带 cache_control 断点。
	req := c.buildRequest(messages, nil)
	require.NotNil(t, req.System)
	require.Len(t, req.System.Blocks, 1)
	assert.Equal(t, "You are helpful.", req.System.Blocks[0].Text)
	require.NotNil(t, req.System.Blocks[0].CacheControl)
	assert.Equal(t, "ephemeral", req.System.Blocks[0].CacheControl.Type)
	require.Len(t, req.Messages, 1)
	require.Len(t, req.Messages[0].Content.Blocks, 1)
	assert.NotNil(t, req.Messages[0].Content.Blocks[0].CacheControl)

	// 显式关缓存：退回纯字符串形态，请求字节与改造前一致。
	off := c.buildRequest(messages, &ChatOptions{CacheRetention: CacheRetentionNone})
	require.NotNil(t, off.System)
	assert.Empty(t, off.System.Blocks)
	assert.Equal(t, "You are helpful.", off.System.Text)
	assert.Equal(t, "Hi", off.Messages[0].Content.Text)
	assert.Empty(t, off.Messages[0].Content.Blocks)

	data, err := json.Marshal(off)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"system":"You are helpful."`)
	assert.Contains(t, string(data), `"content":"Hi"`)
	assert.NotContains(t, string(data), "cache_control")
}

func TestAnthropicBuildRequestOmitsEmptySystem(t *testing.T) {
	c := &AnthropicChat{modelName: "claude-sonnet-4-5"}
	req := c.buildRequest([]Message{{Role: "user", Content: "Hi"}}, nil)
	assert.Nil(t, req.System, "no system message -> the field must stay absent")

	data, err := json.Marshal(req)
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"system"`)
}
