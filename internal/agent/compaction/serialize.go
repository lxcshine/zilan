package compaction

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/models/chat"
)

const (
	// toolResultMaxChars 限制摘要请求中单条工具结果的长度。工具输出是上下文体积的最大
	// 贡献者，而摘要需要的是其要旨，不是它的字节。
	toolResultMaxChars = 2000
	// textMaxChars 限制 user / assistant 散文，它通常不是问题所在，但也不该无界。
	textMaxChars = 4000
	// toolArgsMaxChars 限制渲染后的工具调用参数。写页面的调用会把整篇正文塞进参数里；
	// 摘要需要的是标识与"写过"这个事实，从来不是内容本身。
	toolArgsMaxChars = 400
)

// serializeConversation 把消息渲染为对话文本（transcript）而非直接作为对话传入。
// 拿到真实消息的模型会试图接着往下写；拿到 transcript 的模型则会去总结它。
func serializeConversation(messages []chat.Message) string {
	var parts []string
	for i := range messages {
		msg := &messages[i]
		text := messageText(*msg)
		switch msg.Role {
		case "system":
			continue
		case "user":
			if content := truncate(text, textMaxChars); content != "" {
				parts = append(parts, "[User]: "+content)
			}
		case "assistant":
			if msg.ReasoningContent != "" {
				parts = append(parts,
					"[Assistant thinking]: "+truncate(msg.ReasoningContent, textMaxChars))
			}
			if text != "" {
				parts = append(parts, "[Assistant]: "+truncate(text, textMaxChars))
			}
			if calls := serializeToolCalls(msg.ToolCalls); calls != "" {
				parts = append(parts, "[Assistant tool calls]: "+calls)
			}
		case "tool":
			if content := truncate(text, toolResultMaxChars); content != "" {
				parts = append(parts, fmt.Sprintf("[Tool result %s]: %s", msg.Name, content))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func serializeToolCalls(calls []chat.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	rendered := make([]string, 0, len(calls))
	for _, tc := range calls {
		rendered = append(rendered,
			fmt.Sprintf("%s(%s)", tc.Function.Name, renderToolArgs(tc.Function.Arguments)))
	}
	return strings.Join(rendered, "; ")
}

// renderToolArgs 把参数 JSON 渲染为 `key=value` 序列，丢弃超大取值。键排序，使同一次
// 调用总是渲染成同样的样子——跨压缩比较 transcript 时这一点很重要。
func renderToolArgs(arguments string) string {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(arguments), &parsed); err != nil {
		return truncate(arguments, toolArgsMaxChars)
	}
	keys := make([]string, 0, len(parsed))
	for k := range parsed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		encoded, err := json.Marshal(parsed[k])
		if err != nil {
			continue
		}
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, truncate(string(encoded), toolArgsMaxChars)))
	}
	return strings.Join(pairs, ", ")
}

func truncate(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	return fmt.Sprintf("%s\n\n[... %d more characters truncated]",
		string(runes[:maxChars]), len(runes)-maxChars)
}

// rawArchive 是摘要器不可用时的兜底。它有损且无结构，但保留了下一轮避免重做已完成
// 工作所需的工具名与标识。
//
// 它顶替摘要的位置，因此像摘要一样受 budget 约束，保留最新的消息。不设上限时，一份
// 填满窗口的历史的归档可能几乎和历史本身一样大，而释放量为零的压缩会让请求继续超窗。
func rawArchive(messages []chat.Message, budget int, counter *contextx.Counter) string {
	lines := make([]string, 0, len(messages))
	for i := range messages {
		if line := archiveLine(&messages[i]); line != "" {
			lines = append(lines, line)
		}
	}
	kept := len(lines)
	if budget > 0 {
		used := 0
		for i := len(lines) - 1; i >= 0; i-- {
			used += countString(counter, lines[i])
			if used > budget && i < len(lines)-1 {
				kept = len(lines) - 1 - i
				break
			}
		}
	}

	var sb strings.Builder
	sb.WriteString("Raw conversation archive (LLM summarization unavailable):\n\n")
	if omitted := len(lines) - kept; omitted > 0 {
		fmt.Fprintf(&sb, "[%d earlier messages omitted]\n", omitted)
	}
	for _, line := range lines[len(lines)-kept:] {
		sb.WriteString(line)
	}
	return sb.String()
}

func archiveLine(msg *chat.Message) string {
	text := messageText(*msg)
	switch msg.Role {
	case "user":
		return fmt.Sprintf("- User: %s\n", truncate(text, 500))
	case "assistant":
		if calls := serializeToolCalls(msg.ToolCalls); calls != "" {
			return fmt.Sprintf("- Assistant [%s]: %s\n", calls, truncate(text, 500))
		}
		return fmt.Sprintf("- Assistant: %s\n", truncate(text, 500))
	case "tool":
		return fmt.Sprintf("- Tool[%s]: %s\n", msg.Name, truncate(text, 500))
	}
	return ""
}

// messageText 遵循供应商对多段内容的偏好：有 MultiContent 时以其文本段为准，避免与
// 遗留的标量 Content 重复计入。图片段不参与文本渲染。
func messageText(msg chat.Message) string {
	if len(msg.MultiContent) == 0 {
		return msg.Content
	}
	var text strings.Builder
	for _, part := range msg.MultiContent {
		if part.Type == "text" {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}
