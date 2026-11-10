package compaction

import (
	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/models/chat"
)

// estimatedImageTokens 是单张图片在上下文中的保守 token 估值。供应商按图块计费，
// 而 contextx.Counter 只统计文本，不计图片，因此这里补齐一个固定成本：低估会让
// 压缩器认为图片"免费"，从而在图片导致的溢出上反复压缩文本历史（PRD FR-06 规则 8）。
const estimatedImageTokens = 1024

// genericCounter 是 counter 为 nil 时的兜底，保证包内所有函数都能独立调用。
func genericCounter(counter *contextx.Counter) *contextx.Counter {
	if counter != nil {
		return counter
	}
	return contextx.NewCounter(contextx.VendorGeneric)
}

// countString 用精确 tokenizer 统计单段文本。
func countString(counter *contextx.Counter, s string) int {
	return genericCounter(counter).Count(s)
}

// CountMessage 统计单条消息，含框架开销与图片的保守估值。
func CountMessage(counter *contextx.Counter, msg *chat.Message) int {
	tokens := genericCounter(counter).CountMessage(msg)
	tokens += imageCount(msg) * estimatedImageTokens
	return tokens
}

// CountMessages 统计整段消息列表（含图片保守估值），供 engine 侧做阈值判断。
func CountMessages(counter *contextx.Counter, messages []chat.Message) int {
	total := 0
	for i := range messages {
		total += CountMessage(counter, &messages[i])
	}
	return total
}

// imageCount 返回一条消息携带的图片数量，同时覆盖 Images 与 MultiContent 两种形态。
func imageCount(msg *chat.Message) int {
	n := len(msg.Images)
	for i := range msg.MultiContent {
		part := &msg.MultiContent[i]
		if part.Type == "image_url" && part.ImageURL != nil {
			n++
		}
	}
	return n
}

// CountImages 返回整段消息列表携带的图片总数。
func CountImages(messages []chat.Message) int {
	total := 0
	for i := range messages {
		total += imageCount(&messages[i])
	}
	return total
}
