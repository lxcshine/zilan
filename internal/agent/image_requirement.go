package agent

import (
	"strings"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
)

const agentRetrievedImageRequirementMarker = "## Retrieved Image Output Requirement"

const agentRetrievedImageRequirement = `## Retrieved Image Output Requirement
Retrieved tool results for this turn contain Markdown images. Treat images attached to retrieved passages as relevant by default.
- Unless the user explicitly requests text-only output, or every retrieved image is clearly unrelated, the final answer MUST include at least one relevant Markdown image copied verbatim from the tool results.
- Preserve the complete Markdown image syntax and URL exactly; never invent, shorten, normalize, or replace the URL.
- Use ASCII half-width parentheses exactly as ![alt](url); never use full-width （ or ）.
- Place each image immediately after the paragraph it supports.
- When multiple retrieved images support different sections, distribute them across those sections instead of stopping after the first image.
- Before finishing, silently verify that the answer contains a Markdown image whenever this requirement applies.`

func stepContainsMarkdownImage(step types.AgentStep) bool {
	for _, toolCall := range step.ToolCalls {
		if toolCall.Result != nil &&
			toolCall.Result.Success &&
			searchutil.MarkdownImageRegex.MatchString(toolCall.Result.Output) {
			return true
		}
	}
	return false
}

// appendAgentRetrievedImageRequirement 把「检索图片输出要求」作为一条独立消息插入，
// 而不是改写开头的系统提示。
//
// 系统提示属于跨轮不变的稳定前缀：按本轮检索结果改写它，会让厂商侧的 prompt 缓存
// 从第一个字节起整段失效（FR-11）。改为在稳定前缀之后插入独立消息后，前缀与更早
// 历史的缓存命中范围不受影响，同时要求依然对本轮生效。
func appendAgentRetrievedImageRequirement(messages []chat.Message) []chat.Message {
	for i := range messages {
		if strings.Contains(messages[i].Content, agentRetrievedImageRequirementMarker) {
			// 本次会话已注入过：位置保持不动，避免每轮重写历史击穿缓存。
			return messages
		}
	}
	return append(messages, chat.Message{
		Role:    "user",
		Content: agentRetrievedImageRequirement,
	})
}
