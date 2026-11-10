package types

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// 附件内容模式：完整内联文本 vs 仅清单（按需读取）（PRD FR-22 ③④）。
const (
	AttachmentContentModeFull     = "full"
	AttachmentContentModeOnDemand = "on_demand"
	// AgentAttachmentInlineTokens 是 Agent 提示词里允许整篇内联的最大附件；
	// 更大的文档只给引用，由 Agent 通过 read_file 按需读取。
	AgentAttachmentInlineTokens = 2000
	// AttachmentPathScheme 是附件在 read_file 里的地址前缀。
	AttachmentPathScheme = "attachment://"
)

// AttachmentHandle 是附件面向模型的稳定名字。它由文档 ID 派生，
// 因此每一轮、每条回放的历史消息都用同一个名字指代同一个文件。
// 暴露 handle 而不是原始 ID，避免把可跨会话使用的内部标识交给模型。
func AttachmentHandle(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("att_%x", sum[:4])
}

// AttachmentPath 是附件在 read_file 里的完整地址。
func AttachmentPath(id string) string {
	handle := AttachmentHandle(id)
	if handle == "" {
		return ""
	}
	return AttachmentPathScheme + handle
}
