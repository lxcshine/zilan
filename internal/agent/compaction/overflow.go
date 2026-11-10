package compaction

import (
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// 精确识别上下文溢出是值得的，因为恢复手段很特定：压缩后重试一次。误判为普通失败
// 会直接结束本轮；误判为瞬时错误则会用同一个超大请求反复重试直到耗尽次数。
// 下列模式采集自真实的供应商响应。
var overflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)prompt is too long`),                                  // Anthropic
	regexp.MustCompile(`(?i)request_too_large`),                                   // Anthropic (HTTP 413)
	regexp.MustCompile(`(?i)input is too long for requested model`),               // Amazon Bedrock
	regexp.MustCompile(`(?i)exceeds the context window`),                          // OpenAI
	regexp.MustCompile(`(?i)exceeds (the )?(model'?s )?maximum context length`),   // OpenAI 兼容代理
	regexp.MustCompile(`(?i)input token count.*exceeds the maximum`),              // Google Gemini
	regexp.MustCompile(`(?i)maximum prompt length is \d+`),                        // xAI Grok
	regexp.MustCompile(`(?i)reduce the length of the messages`),                   // Groq
	regexp.MustCompile(`(?i)maximum context length is \d+ tokens`),                // OpenRouter
	regexp.MustCompile(`(?i)exceeds (the )?maximum allowed input length`),         // OpenRouter / Poolside
	regexp.MustCompile(`(?i)is longer than the model'?s context length`),          // Together AI
	regexp.MustCompile(`(?i)exceeds the limit of \d+`),                            // GitHub Copilot
	regexp.MustCompile(`(?i)exceeds the available context size`),                  // llama.cpp
	regexp.MustCompile(`(?i)greater than the context length`),                     // LM Studio
	regexp.MustCompile(`(?i)context window exceeds limit`),                        // MiniMax
	regexp.MustCompile(`(?i)exceeded model token limit`),                          // Kimi
	regexp.MustCompile(`(?i)too large for model with \d+ maximum context length`), // Mistral
	regexp.MustCompile(`(?i)but the configured context size is`),                  // DS4
	regexp.MustCompile(`(?i)model_context_window_exceeded`),                       // z.ai
	regexp.MustCompile(`(?i)prompt too long; exceeded (max )?context length`),     // Ollama
	regexp.MustCompile(`(?i)range of input length should be`),                     // DashScope / Qwen
	regexp.MustCompile(`(?i)context[_ ]length[_ ]exceeded`),                       // 通用
	regexp.MustCompile(`(?i)too many tokens`),                                     // 通用
	regexp.MustCompile(`(?i)token limit exceeded`),                                // 通用
}

// nonOverflowPatterns 是那些提到 token 数量、含义却完全不同的错误。Bedrock 的限流
// 报文是 "ThrottlingException: Too many tokens, please wait before trying again"，
// 它会命中溢出模式但并不是溢出。这里按"出现在报文任意位置"匹配而非锚定，因为看到的
// 是供应商原始错误而非归一化前缀。
var nonOverflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)throttling`),
	regexp.MustCompile(`(?i)service unavailable`),
	regexp.MustCompile(`(?i)rate limit`),
	regexp.MustCompile(`(?i)too many requests`),
}

// IsOverflowError 报告一次失败的 LLM 调用是否因为请求放不进上下文窗口而失败。
// 限流/服务不可用一律不算溢出，否则会拿压缩后的上下文去重试一个只是需要退避的请求。
func IsOverflowError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, p := range nonOverflowPatterns {
		if p.MatchString(msg) {
			return false
		}
	}
	for _, p := range overflowPatterns {
		if p.MatchString(msg) {
			return true
		}
	}
	return false
}

// ResponseHitContextLimit 报告一次**成功**的响应是否是被塞满的上下文窗口塑形的，
// 而不是被我们请求的补全预算截断的。三种形态：
//
//   - 供应商静默接受了超大请求，并报出大于窗口的 prompt（z.ai）。
//   - 供应商截断输入以恰好填满窗口，导致没有生成空间，以 length 停止且无输出（小米 MiMo）。
//   - 供应商以 length 停止但输出量少于我们允许的上限，说明拦住它的不是我们自己的 cap
//     （通用可恢复场景）。
//
// completionBudget 是请求的 max_tokens（在任何基于上下文窗口的钳制之前）；传入钳制后
// 的值恰好会掩盖本函数要检测的情况。传 0 表示未显式请求补全上限，此时第三条判据不生效。
func ResponseHitContextLimit(
	resp *types.ChatResponse, contextWindow, completionBudget int,
) bool {
	if resp == nil {
		return false
	}
	prompt := resp.Usage.PromptTokens
	completion := resp.Usage.CompletionTokens
	lengthStop := isLengthStop(resp.FinishReason)

	if contextWindow > 0 && prompt > contextWindow {
		return true
	}
	if contextWindow > 0 && lengthStop && completion == 0 && prompt > 0 &&
		prompt >= contextWindow*99/100 {
		return true
	}
	// 没有 usage 就无从比较；把它当作溢出会让每一次普通截断都触发重试。
	return lengthStop && completionBudget > 0 && completion > 0 && completion < completionBudget
}

func isLengthStop(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	// model_context_window_exceeded 是 z.ai 的 length 停止原因。
	case "length", "max_tokens", "max_output_tokens", "model_context_window_exceeded":
		return true
	default:
		return false
	}
}
