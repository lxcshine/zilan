// Package attachment 承载"会话内已授权附件"的读取逻辑（PRD FR-22 ③④⑤）。
// 本包刻意不依赖 agent、tool、database 或 sandbox：存储与投影都由适配层注入，
// 否则会与 application/service 形成循环依赖。
package attachment

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// pollInterval 是等待后台解析的轮询间隔。这里用轮询而不是事件订阅：
// 解析在 asynq worker 进程里完成，同进程内没有可靠的完成信号可等。
const pollInterval = 500 * time.Millisecond

// Store 提供会话附件清单与租户内的文档查询。
type Store interface {
	SessionAttachments(context.Context) (types.MessageAttachments, error)
	TemporaryDocument(context.Context, string) (*types.TemporaryDocument, error)
}

// Reader 独立于工具与沙箱，协调一次会话内已授权的附件读取。
type Reader struct {
	store     Store
	parseWait time.Duration
	readImage func(context.Context, string) ([]byte, error)
	// Images 为 nil 表示对话模型自己就能看图，不需要额外的图片理解调用。
	Images *ImageReader
}

// NewReader 把持久化存储绑定到一个会话及其解析等待预算。
func NewReader(store Store, wait time.Duration, readImage func(context.Context, string) ([]byte, error)) *Reader {
	return &Reader{store: store, parseWait: wait, readImage: readImage}
}

// WaitTimeout 覆盖"解析 + 一批图片"：图片批次内部是并发描述的，
// 额外留出排在其他读取之后的一轮队列时间。
// 外层（read_file 工具）必须再留出余量，否则工具被引擎杀掉而底层 VLM 还在跑，
// 既浪费调用又写不进缓存。
func (r *Reader) WaitTimeout() time.Duration {
	wait := r.parseWait
	if r.Images != nil && r.Images.Describe != nil {
		wait += imageDescribeFullTimeout + imageDescribeTimeout
	}
	return wait
}

// List 只加载一次已授权清单，然后逐个解析文档，不等待、不投影文件。
// 列清单不能为每一项都重新加载一遍消息历史。
func (r *Reader) List(ctx context.Context) ([]interfaces.AttachmentDocument, error) {
	inventory, err := r.inventory(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]interfaces.AttachmentDocument, 0, len(inventory))
	for _, item := range inventory {
		doc, err := r.store.TemporaryDocument(ctx, item.ID)
		if err != nil {
			return nil, fmt.Errorf("load attachment %s: %w", item.ID, err)
		}
		out = append(out, interfaces.AttachmentDocument{Attachment: item, Document: doc})
	}
	return out, nil
}

// Resolve 打开一个会话附件，可选择等待解析完成。
func (r *Reader) Resolve(ctx context.Context, ref string, wait bool) (*interfaces.AttachmentDocument, error) {
	inventory, err := r.inventory(ctx)
	if err != nil {
		return nil, err
	}
	item, err := findAttachment(inventory, ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", interfaces.ErrAttachmentNotFound, err)
	}
	var doc *types.TemporaryDocument
	if wait {
		doc, err = r.awaitDocument(ctx, item.ID)
	} else {
		doc, err = r.store.TemporaryDocument(ctx, item.ID)
	}
	if err != nil {
		return nil, err
	}
	return &interfaces.AttachmentDocument{Attachment: item, Document: doc}, nil
}

// inventory 返回会话已存储的附件，按 ID 去重。没有存储文档的内联上传
// 无内容可读，直接排除。
func (r *Reader) inventory(ctx context.Context) (types.MessageAttachments, error) {
	if r.store == nil {
		return nil, fmt.Errorf("attachments are unavailable")
	}
	all, err := r.store.SessionAttachments(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load conversation attachments: %w", err)
	}
	seen := make(map[string]int, len(all))
	out := make(types.MessageAttachments, 0, len(all))
	for _, att := range all {
		id := strings.TrimSpace(att.ID)
		if id == "" {
			continue
		}
		if i, ok := seen[id]; ok {
			// 后出现的行携带解析完成后回写的内容。
			if out[i].Content == "" {
				out[i] = att
			}
			continue
		}
		seen[id] = len(out)
		out = append(out, att)
	}
	return out, nil
}

// findAttachment 依次按 handle、原始 ID、文件名匹配。匹配不到时错误信息里
// 必须列出可用清单，否则模型会反复猜错路径白烧 token。
func findAttachment(inventory types.MessageAttachments, ref string) (types.MessageAttachment, error) {
	ref = strings.TrimSpace(ref)
	var byName []types.MessageAttachment
	for _, att := range inventory {
		if types.AttachmentHandle(att.ID) == ref || att.ID == ref {
			return att, nil
		}
		if strings.EqualFold(att.FileName, ref) {
			byName = append(byName, att)
		}
	}
	if len(byName) > 0 {
		return byName[len(byName)-1], nil
	}
	paths := make([]string, 0, len(inventory))
	for _, att := range inventory {
		paths = append(paths, fmt.Sprintf("%s (%s)", types.AttachmentPath(att.ID), att.FileName))
	}
	if len(paths) == 0 {
		return types.MessageAttachment{}, fmt.Errorf("this conversation has no readable attachments")
	}
	return types.MessageAttachment{}, fmt.Errorf(
		"unknown attachment %q; available: %s", ref, strings.Join(paths, ", "))
}

// awaitDocument 加载文档，只要还在解析就轮询，直到 parseWait 用尽或 ctx 取消。
// 到点返回的是"当前状态"而不是 error：调用方据此告诉模型"还在解析，稍后再试"，
// 而不是让整轮失败（PRD §5.3 增强能力失败不得阻断主流程）。
func (r *Reader) awaitDocument(ctx context.Context, id string) (*types.TemporaryDocument, error) {
	deadline := time.Now().Add(r.parseWait)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		document, err := r.store.TemporaryDocument(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("failed to load attachment: %w", err)
		}
		if document == nil || !attachmentPending(document.Status) || !time.Now().Before(deadline) {
			return document, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func attachmentPending(status string) bool {
	return status == types.TemporaryDocumentStatusUploaded || status == types.TemporaryDocumentStatusProcessing
}
