package compaction

import (
	"fmt"

	"github.com/Tencent/WeKnora/internal/models/chat"
)

// imagePlaceholder 是图片被移除后留下的占位说明（PRD FR-06 规则 3：让模型知道此处曾有
// 内容而非凭空缺失）。
const imagePlaceholder = "[图片已移除以适配上下文窗口：此处原有 %d 张图片，内容不再可用。]"

// DropOldestImages 从最旧的消息开始移除图片载荷，只保留最新的 keep 张，返回新的消息
// 切片与被移除的图片数。
//
// 这是多模态溢出的首选恢复手段（PRD FR-06 规则 8）：图片按图块计费，单张就可能上千
// token，而压缩文本历史既昂贵（一次摘要调用）又治不了图片本身的体积。
//
// 最新一条携带图片的 user 消息被视为必要图片（用户本轮刚上传），永不移除。
func DropOldestImages(messages []chat.Message, keep int) ([]chat.Message, int) {
	if keep < 0 {
		keep = 0
	}
	total := CountImages(messages)
	if total <= keep {
		return messages, 0
	}

	// 当前轮次用户刚上传的图片是必要图片。
	protected := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && imageCount(&messages[i]) > 0 {
			protected = i
			break
		}
	}

	droppable := 0
	for i := range messages {
		if i != protected {
			droppable += imageCount(&messages[i])
		}
	}
	toDrop := total - keep
	if toDrop > droppable {
		toDrop = droppable
	}
	if toDrop <= 0 {
		return messages, 0
	}

	out := append([]chat.Message(nil), messages...)
	dropped := 0
	for i := range out {
		if dropped >= toDrop || i == protected {
			continue
		}
		removed := stripImages(&out[i], toDrop-dropped)
		dropped += removed
	}
	if dropped == 0 {
		return messages, 0
	}
	return out, dropped
}

// stripImages 从单条消息上移除至多 max 张图片，并在原位置留下占位说明。
func stripImages(msg *chat.Message, max int) int {
	removed := 0
	if len(msg.Images) > 0 {
		n := len(msg.Images)
		if n > max {
			n = max
		}
		msg.Images = msg.Images[n:]
		removed += n
		if len(msg.Images) == 0 {
			msg.Images = nil
		}
	}

	if len(msg.MultiContent) > 0 {
		kept := make([]chat.MessageContentPart, 0, len(msg.MultiContent))
		for i := range msg.MultiContent {
			part := msg.MultiContent[i]
			isImage := part.Type == "image_url" && part.ImageURL != nil
			if !isImage || removed >= max {
				kept = append(kept, part)
				continue
			}
			removed++
		}
		if removed > 0 {
			msg.MultiContent = kept
		}
	}

	if removed > 0 {
		note := fmt.Sprintf(imagePlaceholder, removed)
		if len(msg.MultiContent) > 0 {
			msg.MultiContent = append(msg.MultiContent,
				chat.MessageContentPart{Type: "text", Text: note})
		} else if msg.Content == "" {
			msg.Content = note
		} else {
			msg.Content += "\n\n" + note
		}
	}
	return removed
}
