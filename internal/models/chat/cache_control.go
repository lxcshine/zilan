package chat

import (
	"context"
	"net/http"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/provider"
	"github.com/sashabaranov/go-openai"
)

// cache_control.go 实现 FR-11 ③（在稳定前缀边界插入缓存标记）。
//
// 移植自上游 WeKnora v0.9.0 的 internal/models/api/prompt_cache.go，按 Zilan 的
// models/chat 包结构改写：Zilan 没有 models/api 协议层，供应商差异由
// providerAdapter（provider.go）与 AnthropicChat（anthropic.go）承担。
//
// 前置条件（顺序约束）：缓存标记只有在"稳定前缀真的稳定"之后才有意义。前缀的
// 字节级稳定由 contextx（L0/L1 预算不随意图变化）、agent/prompts.go（系统提示不含
// 秒级时间戳）与 application/service/memory.go（记忆块确定性排序）共同保证。

// CacheRetention is the prompt-cache TTL preference. Empty means short (the
// provider default).
type CacheRetention string

const (
	// CacheRetentionNone 显式关闭本次请求的缓存标记。摘要/压缩类一次性调用应当
	// 使用它：这类 prompt 不会被复用，写缓存只会白白支付缓存写入成本。
	CacheRetentionNone CacheRetention = "none"
	// CacheRetentionShort is the provider's default (ephemeral) cache.
	CacheRetentionShort CacheRetention = "short"
	// CacheRetentionLong requests the extended TTL where the provider supports it.
	CacheRetentionLong CacheRetention = "long"
)

const (
	// LongCacheTTL 是 Anthropic 风格 cache_control 的长 TTL 取值。
	LongCacheTTL = "1h"
	// LongCacheRetentionField 是 OpenAI 兼容接口上长缓存的取值。
	LongCacheRetentionField = "24h"
	// openAIPromptCacheKeyMaxLength is the routing-key length OpenAI accepts.
	openAIPromptCacheKeyMaxLength = 64
)

// ResolveCacheRetention returns the caller's retention preference, defaulting
// to the provider's short cache.
func ResolveCacheRetention(opts *ChatOptions) CacheRetention {
	if opts != nil && opts.CacheRetention != "" {
		return opts.CacheRetention
	}
	return CacheRetentionShort
}

// ClampPromptCacheKey trims a routing key to the 64 characters OpenAI accepts.
func ClampPromptCacheKey(key string) string {
	if key == "" {
		return ""
	}
	runes := []rune(key)
	if len(runes) <= openAIPromptCacheKeyMaxLength {
		return key
	}
	return string(runes[:openAIPromptCacheKeyMaxLength])
}

// CacheControlMarker is the Anthropic-style cache breakpoint object that
// several OpenAI-compatible gateways (OpenRouter, DashScope) also accept.
type CacheControlMarker struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// CacheControlFor builds the marker for a retention preference, or nil when
// the caller disabled caching for this request.
func CacheControlFor(retention CacheRetention, longTTL string) *CacheControlMarker {
	if retention == CacheRetentionNone {
		return nil
	}
	marker := &CacheControlMarker{Type: "ephemeral"}
	if retention == CacheRetentionLong && longTTL != "" {
		marker.TTL = longTTL
	}
	return marker
}

// ApplyCacheControlBreakpoints injects Anthropic-style cache_control markers
// into an OpenAI-compatible JSON body: the first instruction message, the last
// tool definition and the last conversation message.
//
// 这三处正好框住"稳定前缀"的边界：指令与工具定义跨轮不变（缓存命中区），
// 最后一条会话消息之后的内容才是逐轮变化区。
func ApplyCacheControlBreakpoints(payload map[string]any, marker *CacheControlMarker) {
	if marker == nil {
		return
	}
	applyCacheControlToInstructionMessages(payload["messages"], marker)
	applyCacheControlToLastTool(payload["tools"], marker)
	applyCacheControlToLastConversationMessage(payload["messages"], marker)
}

func applyCacheControlToInstructionMessages(raw any, marker *CacheControlMarker) {
	messages, ok := raw.([]any)
	if !ok {
		return
	}
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "system" || role == "developer" {
			addCacheControlToMessageContent(msg, marker)
			return
		}
	}
}

func applyCacheControlToLastConversationMessage(raw any, marker *CacheControlMarker) {
	messages, ok := raw.([]any)
	if !ok {
		return
	}
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "user" || role == "assistant" || role == "tool" {
			if addCacheControlToMessageContent(msg, marker) {
				return
			}
		}
	}
}

func applyCacheControlToLastTool(raw any, marker *CacheControlMarker) {
	tools, ok := raw.([]any)
	if !ok || len(tools) == 0 {
		return
	}
	last, ok := tools[len(tools)-1].(map[string]any)
	if !ok {
		return
	}
	last["cache_control"] = marker
}

func addCacheControlToMessageContent(msg map[string]any, marker *CacheControlMarker) bool {
	content, ok := msg["content"]
	if !ok || content == nil {
		return false
	}
	if text, ok := content.(string); ok {
		if text == "" {
			return false
		}
		msg["content"] = []any{
			map[string]any{
				"type":          "text",
				"text":          text,
				"cache_control": marker,
			},
		}
		return true
	}
	parts, ok := content.([]any)
	if !ok {
		return false
	}
	for i := len(parts) - 1; i >= 0; i-- {
		part, ok := parts[i].(map[string]any)
		if !ok {
			continue
		}
		if partType, _ := part["type"].(string); partType == "text" || partType == "tool_result" {
			part["cache_control"] = marker
			return true
		}
	}
	return false
}

// promptCacheSupport 描述一个供应商在 OpenAI 兼容接口上接受的缓存标记能力。
// 未被验证支持的供应商一律保持关闭：贸然注入 cache_control / prompt_cache_key
// 可能被严格校验的网关拒绝（400），得不偿失。
type promptCacheSupport struct {
	// CacheKey 接受 prompt_cache_key / prompt_cache_retention 路由字段。
	CacheKey bool
	// CacheControl 接受 Anthropic 风格的 cache_control 断点。
	CacheControl bool
}

// resolvePromptCacheSupport 按供应商/模型给出默认的缓存能力，并允许模型级
// extra_config 覆盖：
//
//	prompt_cache = "off"       关闭全部缓存标记
//	prompt_cache = "key"       只发 prompt_cache_key
//	prompt_cache = "anthropic" 发 cache_control 断点（含 prompt_cache_key）
//
// 默认值对齐上游 v0.9.0 已验证的供应商清单（OpenAI / Azure / OpenRouter 支持
// prompt_cache_key；DashScope 与 OpenRouter 的 Claude 家族支持 cache_control）。
func resolvePromptCacheSupport(name provider.ProviderName, model string, extraConfig map[string]string) promptCacheSupport {
	var support promptCacheSupport
	switch name {
	case provider.ProviderOpenAI, provider.ProviderAzureOpenAI, provider.ProviderOpenRouter:
		support.CacheKey = true
	}
	switch name {
	case provider.ProviderAliyun:
		support.CacheControl = true
	case provider.ProviderOpenRouter:
		support.CacheControl = strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "anthropic/")
	}

	switch strings.ToLower(strings.TrimSpace(extraConfig["prompt_cache"])) {
	case "off", "false", "none":
		return promptCacheSupport{}
	case "key":
		return promptCacheSupport{CacheKey: true}
	case "anthropic", "cache_control":
		return promptCacheSupport{CacheKey: support.CacheKey, CacheControl: true}
	case "on", "true":
		return promptCacheSupport{CacheKey: true, CacheControl: true}
	}
	return support
}

// applyPromptCacheMarkers 在 OpenAI 兼容的 JSON 请求体上写入缓存路由字段与
// cache_control 断点。retention 为 none 时什么都不做（摘要类调用显式关缓存）。
func applyPromptCacheMarkers(payload map[string]any, opts *ChatOptions, support promptCacheSupport) {
	if payload == nil {
		return
	}
	retention := ResolveCacheRetention(opts)
	if retention == CacheRetentionNone {
		return
	}
	if support.CacheKey && opts != nil {
		if key := ClampPromptCacheKey(opts.PromptCacheKey); key != "" {
			payload["prompt_cache_key"] = key
			if retention == CacheRetentionLong {
				payload["prompt_cache_retention"] = LongCacheRetentionField
			}
		}
	}
	if support.CacheControl {
		ApplyCacheControlBreakpoints(payload, CacheControlFor(retention, LongCacheTTL))
	}
}

// promptCacheRoutingKey 返回本次请求的缓存路由键：会话亲和 header 与
// prompt_cache_key 共用它。调用方显式关闭缓存（CacheRetentionNone）时返回空串，
// 此时既不写 header 也不写字段。
func promptCacheRoutingKey(opts *ChatOptions) string {
	if opts == nil || ResolveCacheRetention(opts) == CacheRetentionNone {
		return ""
	}
	return ClampPromptCacheKey(opts.PromptCacheKey)
}

// AttachSessionAffinityHeaders sets the sticky-routing headers OpenAI-style
// gateways use to keep one conversation on one cache shard.
func AttachSessionAffinityHeaders(req *http.Request, sessionID string) {
	if req == nil {
		return
	}
	if sessionID == "" {
		return
	}
	req.Header.Set("session_id", sessionID)
	req.Header.Set("x-client-request-id", sessionID)
	req.Header.Set("x-session-affinity", sessionID)
}

// sessionAffinityKeyType 是携带缓存路由键的上下文键（包内私有，避免键冲突）。
type sessionAffinityKeyType struct{}

var sessionAffinityKey sessionAffinityKeyType

// withSessionAffinity 把缓存路由键放进请求上下文：raw HTTP 路径直接读取，
// SDK 路径由 sessionAffinityDoer 在发出请求前读取。
func withSessionAffinity(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionAffinityKey, key)
}

func sessionAffinityFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	key, _ := ctx.Value(sessionAffinityKey).(string)
	return key
}

// sessionAffinityDoer 包装 SDK 使用的 HTTP 客户端，为每个请求附加会话亲和 header
// （FR-11 ③）。上下文里没有路由键时完全透传，不改变既有行为。
type sessionAffinityDoer struct {
	base openai.HTTPDoer
}

func (d sessionAffinityDoer) Do(req *http.Request) (*http.Response, error) {
	if req != nil {
		AttachSessionAffinityHeaders(req, sessionAffinityFromContext(req.Context()))
	}
	return d.base.Do(req)
}

// wrapSessionAffinityClient 在 SDK 客户端外层套上会话亲和 header 注入。
func wrapSessionAffinityClient(base openai.HTTPDoer) openai.HTTPDoer {
	if base == nil {
		base = &http.Client{}
	}
	if _, ok := base.(sessionAffinityDoer); ok {
		return base
	}
	return sessionAffinityDoer{base: base}
}
