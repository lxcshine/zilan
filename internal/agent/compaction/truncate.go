package compaction

import (
	"fmt"

	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/models/chat"
)

// TruncateMarker 是规则式截断留下的占位前缀。它与 SummaryMarker 刻意不同：这不是摘要，
// 下一次压缩不应把它当作可以"更新"的先前摘要。
const TruncateMarker = "[上下文截断]"

func truncatePlaceholder(dropped int) string {
	return fmt.Sprintf("%s 早期 %d 条消息已被丢弃（无摘要可用），仅保留系统提示与最近的对话。"+
		"早期步骤已被丢弃，可能影响后续推理；如需其中的细节，请重新检索或重新执行相应工具。",
		TruncateMarker, dropped)
}

// TruncateHistory 执行无模型的规则式截断：保留系统层与最近 keepRecentTokens 预算内的
// 消息，其余整段丢弃并留下一条明确的占位说明（PRD FR-06 规则 5 与异常条款：摘要模型
// 失败时回退到规则式截断，且不得静默降级）。
//
// 切点复用 FindCutPoint，因此同样遵守 isCutPointMessage：tool_call 与其工具结果的配对
// 绝不会被拆开。返回新的消息列表与被丢弃的消息数（0 表示无可丢弃）。
func TruncateHistory(
	messages []chat.Message, counter *contextx.Counter, keepRecentTokens int,
) ([]chat.Message, int) {
	if len(messages) == 0 {
		return messages, 0
	}
	start := historyStart(messages)
	cut := FindCutPoint(messages, start, keepRecentTokens, counter)
	dropped := cut.FirstKeptIdx - start
	if dropped <= 0 {
		return messages, 0
	}

	out := make([]chat.Message, 0, 2+len(messages)-cut.FirstKeptIdx)
	if start == 1 {
		out = append(out, messages[0])
	}
	out = append(out, chat.Message{Role: "user", Content: truncatePlaceholder(dropped)})
	return append(out, messages[cut.FirstKeptIdx:]...), dropped
}
