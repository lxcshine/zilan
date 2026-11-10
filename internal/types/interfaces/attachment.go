package interfaces

import (
	"context"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// ErrAttachmentNotFound 表示引用不在本会话已授权的附件清单内。
// 调用方（Agent 工具）据此把错误降级为一次可读的失败结果，而不是中断整轮。
var ErrAttachmentNotFound = errors.New("attachment is not in this conversation")

// AttachmentReader 是 attachment:// 协议使用的应用层边界。
// 实现方必须先按会话清单完成鉴权，再打开任何内容。
type AttachmentReader interface {
	List(context.Context) ([]AttachmentDocument, error)
	Resolve(ctx context.Context, ref string, wait bool) (*AttachmentDocument, error)
	// ReadImages 对文档鉴权一次，然后并发读取编号图片。
	// 返回的 error 覆盖整批；单张失败记录在对齐的结果里。
	ReadImages(
		ctx context.Context, documentID string, numbers []int, detail, focus string,
	) ([]AttachmentImageRead, error)
	WaitTimeout() time.Duration
}

// AttachmentImageRead 是单个请求编号的读取结果。
type AttachmentImageRead struct {
	Number int
	Image  *AttachmentImage
	Err    error
}

// AttachmentDocument 同时携带会话内的附件元数据与持久化的解析文档。
type AttachmentDocument struct {
	Attachment types.MessageAttachment
	Document   *types.TemporaryDocument
	Warning    string
}

// AttachmentImage 是准备就绪的视觉证据，Description 非 nil 表示已尝试过图片理解。
type AttachmentImage struct {
	DataURI string
	// 非 nil 表示已经尝试过描述；空字符串也不应再触发一次 VLM 调用。
	Description *string
	Warning     string
}
