package interfaces

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// StreamEvent represents a single event in the stream
type StreamEvent struct {
	ID        string                 `json:"id"`             // Unique event ID
	Type      types.ResponseType     `json:"type"`           // Event type (thinking, tool_call, tool_result, references, complete, etc.)
	Content   string                 `json:"content"`        // Event content (chunk for streaming events)
	Done      bool                   `json:"done"`           // Whether this event is done
	Timestamp time.Time              `json:"timestamp"`      // When this event occurred
	Data      map[string]interface{} `json:"data,omitempty"` // Additional event data (references, metadata, etc.)
	// Usage 是本轮的模型 token 用量，只在 complete 事件上出现（PRD FR-12）。
	// 单独给一个强类型字段而不是只塞进 Data：事件会经 Redis 序列化再读回，Data 里的
	// 用量回来时已经退化成 map[string]interface{}，前端与 buildStreamResponse 都得
	// 再解析一次。带 json tag 的字段则原样往返。
	Usage *types.TokenUsage `json:"usage,omitempty"`
}

// StreamManager stream manager interface - minimal append-only design
// All stream state is managed through events: metadata, references, completion, etc.
type StreamManager interface {
	// AppendEvent appends a single event to the stream
	// Uses Redis RPush for O(1) append performance
	// All event types (thinking, tool_call, references, complete) use this method
	AppendEvent(ctx context.Context, sessionID, messageID string, event StreamEvent) error

	// GetEvents gets events starting from offset
	// Uses Redis LRange for incremental reads
	// Returns: events slice, next offset for subsequent reads, error
	GetEvents(ctx context.Context, sessionID, messageID string, fromOffset int) ([]StreamEvent, int, error)
}
