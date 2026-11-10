package types

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
)

// PromptCacheStatus distinguishes a real cache miss from providers that do not
// expose prompt-cache accounting. Treating both as zero makes fleet-level hit
// rate dashboards misleading.
type PromptCacheStatus string

const (
	PromptCacheStatusUnsupported PromptCacheStatus = "unsupported"
	PromptCacheStatusUnreported  PromptCacheStatus = "unreported"
	PromptCacheStatusMiss        PromptCacheStatus = "miss"
	PromptCacheStatusHit         PromptCacheStatus = "hit"
)

// TokenUsage holds token consumption statistics returned by the model API.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CachedTokens is the legacy alias for CacheReadTokens. It remains on the
	// wire for compatibility with existing API consumers.
	CachedTokens     int               `json:"cached_tokens,omitempty"`
	CacheReadTokens  int               `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int               `json:"cache_write_tokens,omitempty"`
	CacheMissTokens  int               `json:"cache_miss_tokens,omitempty"`
	CacheReported    bool              `json:"cache_reported"`
	CacheStatus      PromptCacheStatus `json:"cache_status,omitempty"`
}

// Accumulate 把另一次模型调用的用量累加进来。
//
// 一轮对话可能包含多次模型往返（Agent 的每个 ReAct 轮次、压缩摘要调用、最终答案
// 合成），持久化到 messages.usage 上的是本轮的总量，因此需要逐次累加。缓存计数
// 同样累加（本轮一共从供应商前缀缓存里读到多少 token）；CacheReported 取或，
// CacheStatus 保留最后一次非空值——它描述的是本轮结束时的缓存状态。
func (u *TokenUsage) Accumulate(other TokenUsage) {
	if u == nil {
		return
	}
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.TotalTokens += other.TotalTokens
	u.CachedTokens += other.CachedTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.CacheMissTokens += other.CacheMissTokens
	if other.CacheReported {
		u.CacheReported = true
	}
	if other.CacheStatus != "" {
		u.CacheStatus = other.CacheStatus
	}
}

// IsZero 报告这份用量是否没有任何计量（供应商未回报 usage）。
func (u *TokenUsage) IsZero() bool {
	return u == nil || (u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 && u.CacheMissTokens == 0)
}

// Value 实现 driver.Valuer：用量随 assistant 消息持久化到 messages.usage
// （PRD FR-12）。零值与 nil 都存 NULL——用户消息不消耗模型用量，供应商未回报
// usage 的轮次也不该在库里留下一份全零的假计量。
func (u *TokenUsage) Value() (driver.Value, error) {
	if u.IsZero() {
		return nil, nil
	}
	return json.Marshal(u)
}

// Scan 实现 sql.Scanner。
func (u *TokenUsage) Scan(value interface{}) error {
	if value == nil {
		*u = TokenUsage{}
		return nil
	}
	var b []byte
	switch v := value.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("types: cannot scan token usage from unsupported type")
	}
	if len(b) == 0 {
		*u = TokenUsage{}
		return nil
	}
	return json.Unmarshal(b, u)
}

// SetPromptCacheUsage normalizes provider-specific cache counters into the
// shared usage model. promptTokens remains the provider's total input-token
// count; read/write/miss are descriptive subsets and must not be added to it.
func (u *TokenUsage) SetPromptCacheUsage(read, write, miss int, reported bool) {
	if u == nil {
		return
	}
	if read < 0 {
		read = 0
	}
	if write < 0 {
		write = 0
	}
	if miss < 0 {
		miss = 0
	}
	u.CachedTokens = read
	u.CacheReadTokens = read
	u.CacheWriteTokens = write
	u.CacheMissTokens = miss
	u.CacheReported = reported
	switch {
	case !reported:
		u.CacheStatus = PromptCacheStatusUnreported
	case read > 0:
		u.CacheStatus = PromptCacheStatusHit
	default:
		u.CacheStatus = PromptCacheStatusMiss
	}
}

// MarkPromptCacheUnsupported marks a provider/model path that cannot report
// provider-side prompt-cache usage.
func (u *TokenUsage) MarkPromptCacheUnsupported() {
	if u == nil {
		return
	}
	u.SetPromptCacheUsage(0, 0, 0, false)
	u.CacheStatus = PromptCacheStatusUnsupported
}

// LLMToolCall represents a function/tool call from the LLM
type LLMToolCall struct {
	ID               string           `json:"id"`
	Type             string           `json:"type"` // "function"
	Function         FunctionCall     `json:"function"`
	ProviderMetadata ToolCallMetadata `json:"provider_metadata,omitempty"`

	// ModelArguments and the resolution fields are request-local observability
	// state. ModelArguments preserves the exact JSON emitted by the model while
	// Function.Arguments is decoded to durable application identifiers before
	// tool execution. These fields must never be sent back to a provider or
	// persisted in chat history.
	ModelArguments     string   `json:"-"`
	ArgumentResolution string   `json:"-"`
	UnresolvedHandles  []string `json:"-"`
}

// ToolCallMetadata carries provider-specific tool-call state that must round-trip
// with the assistant tool call, without teaching core agent code vendor fields.
type ToolCallMetadata map[string]json.RawMessage

// FunctionCall represents the function details
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// ChatResponse chat response
type ChatResponse struct {
	Content string `json:"content"`
	// ReasoningContent 是支持思考链的模型（DeepSeek thinking、小米 MiMo、vLLM reasoning 等）
	// 在本轮输出的推理内容。需要在后续多轮请求中原样回传给那些严格校验的供应商。
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []LLMToolCall `json:"tool_calls,omitempty"`
	FinishReason     string        `json:"finish_reason,omitempty"`
	Usage            TokenUsage    `json:"usage"`

	// AnswerStreamed reports whether the user-facing answer text was already
	// streamed live to the final-answer UI area during this round (i.e. the
	// model answered with plain content). When true, the natural-stop branch
	// must only emit the closing
	// Done marker for AnswerEventID instead of re-emitting the whole answer —
	// otherwise the answer would render twice and "jump" at end of stream.
	// Transient, never persisted.
	AnswerStreamed bool `json:"-"`
	// AnswerEventID is the EventBus event ID under which the live answer
	// chunks were streamed, so the natural-stop branch can close the same
	// stream with a Done marker. Empty when AnswerStreamed is false.
	AnswerEventID string `json:"-"`
}

// Response type
type ResponseType string

const (
	// Answer response type
	ResponseTypeAnswer ResponseType = "answer"
	// References response type
	ResponseTypeReferences ResponseType = "references"
	// Thinking response type (for agent thought process)
	ResponseTypeThinking ResponseType = "thinking"
	// Tool call response type (for agent tool invocations)
	ResponseTypeToolCall ResponseType = "tool_call"
	// Tool result response type (for agent tool results)
	ResponseTypeToolResult ResponseType = "tool_result"
	// Error response type
	ResponseTypeError ResponseType = "error"
	// Reflection response type (for agent reflection)
	ResponseTypeReflection ResponseType = "reflection"
	// Session title response type
	ResponseTypeSessionTitle ResponseType = "session_title"
	// Agent query response type (query received and processing started)
	ResponseTypeAgentQuery ResponseType = "agent_query"
	// Complete response type (agent complete)
	ResponseTypeComplete ResponseType = "complete"
	// ToolApprovalRequired: MCP tool marked dangerous — UI must collect user approval before execution continues
	ResponseTypeToolApprovalRequired ResponseType = "tool_approval_required"
	// ToolApprovalResolved: user approved/rejected (or timeout); informational for UI replay
	ResponseTypeToolApprovalResolved ResponseType = "tool_approval_resolved"
	// MCPOAuthRequired: an OAuth-enabled MCP service was invoked but the user
	// has not authorized it — UI must surface an "Authorize" prompt and the
	// agent pauses until authorization completes (or the wait times out).
	ResponseTypeMCPOAuthRequired ResponseType = "mcp_oauth_required"
	// MCPOAuthResolved: authorization completed / timed out / canceled;
	// informational for UI replay.
	ResponseTypeMCPOAuthResolved ResponseType = "mcp_oauth_resolved"
)

// StreamResponse stream response
type StreamResponse struct {
	ID                  string                 `json:"id"`
	ResponseType        ResponseType           `json:"response_type"`
	Content             string                 `json:"content"`
	Done                bool                   `json:"done"`
	KnowledgeReferences References             `json:"knowledge_references,omitempty"`
	SessionID           string                 `json:"session_id,omitempty"`
	AssistantMessageID  string                 `json:"assistant_message_id,omitempty"`
	ToolCalls           []LLMToolCall          `json:"tool_calls,omitempty"`
	Data                map[string]interface{} `json:"data,omitempty"`
	Usage               *TokenUsage            `json:"usage,omitempty"`
	FinishReason        string                 `json:"finish_reason,omitempty"`
}

// References references
type References []*SearchResult

// Value implements the driver.Valuer interface, used to convert References to database values
func (c References) Value() (driver.Value, error) {
	return json.Marshal(c)
}

// Scan implements the sql.Scanner interface, used to convert database values to References
func (c *References) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	b, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(b, c)
}
