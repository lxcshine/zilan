package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/application/attachment"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

// defaultAgentAttachmentParseWait 与 handler 侧的 attachmentParseWaitTimeout 保持一致：
// read_file 等待后台解析的默认预算。
const defaultAgentAttachmentParseWait = 60 * time.Second

// maxAttachmentImageBytes 是从存储读单张附件图片的上限。
const maxAttachmentImageBytes = 32 << 20

// agentSessionAttachments 是 attachment:// 对一个会话的视图。
//
// 文档只在"应用层 reader 已经把它和会话自己的消息附件对上"之后，才按租户 + ID 查询；
// 因此分叉会话仍然能读到 temporary_documents 行还留在父会话上的文件（PRD FR-22 ⑥）。
type agentSessionAttachments struct {
	db        *gorm.DB
	files     interfaces.FileService
	tenantID  uint64
	sessionID string
}

// SessionAttachments 只投影 messages.attachments 列，按创建顺序返回。
// 必须按消息反查而不是按 temporary_documents.session_id 相等：分叉出来的会话
// 复制了用户消息（连带附件引用），但文档行的 session_id 仍指向原会话。
func (a *agentSessionAttachments) SessionAttachments(ctx context.Context) (types.MessageAttachments, error) {
	if a.sessionID == "" {
		return nil, nil
	}
	var rows []struct {
		Attachments types.MessageAttachments `gorm:"column:attachments"`
	}
	err := a.db.WithContext(ctx).
		Model(&types.Message{}).
		Select("attachments").
		Where("session_id = ? AND role = ? AND deleted_at IS NULL", a.sessionID, "user").
		Order("created_at ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(types.MessageAttachments, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Attachments...)
	}
	return out, nil
}

func (a *agentSessionAttachments) TemporaryDocument(
	ctx context.Context, documentID string,
) (*types.TemporaryDocument, error) {
	var document types.TemporaryDocument
	err := a.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", a.tenantID, documentID).
		First(&document).Error
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &document, nil
}

func (a *agentSessionAttachments) ReadImage(ctx context.Context, ref string) ([]byte, error) {
	if a.files == nil {
		return nil, fmt.Errorf("attachment image storage is unavailable")
	}
	// 共享 Agent 在模型上下文里用的是它自己的源租户，而附件存储属于会话所有者。
	ctx = context.WithValue(ctx, types.TenantIDContextKey, a.tenantID)
	file, err := a.files.GetFile(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxAttachmentImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAttachmentImageBytes {
		return nil, fmt.Errorf("attachment image exceeds %d bytes", maxAttachmentImageBytes)
	}
	return data, nil
}

func isNotFound(err error) bool {
	return err == gorm.ErrRecordNotFound || strings.Contains(err.Error(), "record not found")
}

// agentAttachmentParseWait 依次采用 WEKNORA_CHAT_ATTACHMENT_WAIT_TIMEOUT_SEC 与默认值。
// 这是 read_file 内部等待解析的预算；外层工具预算由
// ReadFileTool.ExecutionTimeout() 在此基础上再加余量（PRD FR-22 超时分层）。
func agentAttachmentParseWait() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("WEKNORA_CHAT_ATTACHMENT_WAIT_TIMEOUT_SEC")); raw != "" {
		if sec, err := strconv.Atoi(raw); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	return defaultAgentAttachmentParseWait
}

// registerAttachmentReader 在会话确实带有已存储附件时，为 read_file 挂上 attachment://。
// 注册跟随会话内容而不是工具白名单：用户发过的文件必须始终可读。
func (s *agentService) registerAttachmentReader(
	ctx context.Context, registry *tools.ToolRegistry, sessionID string,
) *attachment.Reader {
	if s.db == nil || registry == nil || sessionID == "" {
		return nil
	}
	// 附件属于会话所有者；共享 Agent 在别人的工作区里跑时，这与 Agent 的租户不同。
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return nil
	}
	store := &agentSessionAttachments{
		db: s.db, files: s.fileService, tenantID: tenantID, sessionID: sessionID,
	}
	attachments, err := store.SessionAttachments(ctx)
	if err != nil {
		logger.Warnf(ctx, "attachment:// not offered: load session attachments: %v", err)
		return nil
	}
	stored := false
	for _, item := range attachments {
		if strings.TrimSpace(item.ID) != "" {
			stored = true
			break
		}
	}
	if !stored {
		return nil
	}
	var reader *tools.ReadFileTool
	if existing, getErr := registry.GetTool(tools.ToolReadFile); getErr == nil {
		if reader, _ = existing.(*tools.ReadFileTool); reader == nil {
			logger.Warnf(ctx, "Cannot attach conversation attachments: read_file is registered by another tool")
			return nil
		}
	} else {
		reader = tools.NewReadFileTool()
		registry.RegisterTool(reader)
	}
	source := attachment.NewReader(store, agentAttachmentParseWait(), store.ReadImage)
	reader.WithAttachments(source)
	return source
}

// chatModelSupportsVision 判断本轮对话模型自己能不能看图。
// 能看图时不必再挂 attachment.ImageReader：read_file 直接把图片交给模型即可。
func (s *agentService) chatModelSupportsVision(ctx context.Context, chatModel interface{ GetModelID() string }) bool {
	if s.modelService == nil || chatModel == nil {
		return false
	}
	modelID := chatModel.GetModelID()
	if modelID == "" {
		return false
	}
	info, err := s.modelService.GetModelByID(ctx, modelID)
	if err != nil || info == nil {
		return false
	}
	return info.Parameters.SupportsVision
}

// 图片理解缓存的容量上限：描述与文档同生命周期，因此不需要单独的清理策略。
const (
	maxDocumentDescriptions  = 64
	maxDescriptionCacheBytes = 1 << 20
)

// agentAttachmentDescriptionCache 把视觉模型的图片理解结果存在
// temporary_documents.metadata 的 attachment_image_descriptions 键下（PRD FR-22 ⑤）。
// 与文档同生共死，不引入第二套文档生命周期，也不需要数据库迁移。
type agentAttachmentDescriptionCache struct {
	db       *gorm.DB
	tenantID uint64
}

func newAgentAttachmentDescriptionCache(db *gorm.DB, tenantID uint64) attachment.DescriptionCache {
	if db == nil {
		return nil
	}
	return &agentAttachmentDescriptionCache{db: db, tenantID: tenantID}
}

func (c *agentAttachmentDescriptionCache) loadMetadata(
	ctx context.Context, id string,
) (*types.TemporaryDocument, error) {
	var doc types.TemporaryDocument
	err := c.db.WithContext(ctx).
		Select("metadata").
		Where("tenant_id = ? AND id = ?", c.tenantID, id).
		First(&doc).Error
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &doc, nil
}

func (c *agentAttachmentDescriptionCache) Get(
	ctx context.Context, id, key string,
) (*attachment.Description, error) {
	doc, err := c.loadMetadata(ctx, id)
	if err != nil || doc == nil {
		return nil, err
	}
	_, entries, err := decodeAttachmentDescriptions(doc.Metadata)
	if err != nil {
		return nil, err
	}
	value, ok := entries[key]
	if !ok {
		return nil, nil
	}
	return &value, nil
}

func (c *agentAttachmentDescriptionCache) Put(
	ctx context.Context, id, key string, value attachment.Description,
) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded) > maxDescriptionCacheBytes/4 {
		return fmt.Errorf("image observation exceeds cache size limit")
	}
	// 用比较并交换写入：这样在 SQLite / PostgreSQL / MySQL 上都能保留解析器写入的
	// metadata 与并发的图片写入，不需要各方言各自的 JSON 修改 SQL。
	for attempt := 0; attempt < 8; attempt++ {
		doc, err := c.loadMetadata(ctx, id)
		if err != nil {
			return err
		}
		if doc == nil {
			return fmt.Errorf("attachment no longer exists")
		}
		metadata, entries, err := decodeAttachmentDescriptions(doc.Metadata)
		if err != nil {
			return err
		}
		entries[key] = value
		keys := make([]string, 0, len(entries))
		for k := range entries {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			return entries[keys[i]].CreatedAt.Before(entries[keys[j]].CreatedAt)
		})
		for {
			encoded, err = json.Marshal(entries)
			if err != nil {
				return err
			}
			if len(entries) <= maxDocumentDescriptions && len(encoded) <= maxDescriptionCacheBytes {
				break
			}
			delete(entries, keys[0])
			keys = keys[1:]
		}
		metadata[types.TemporaryDocumentImageDescriptionsKey] = encoded
		updated, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		cast := "CAST(metadata AS TEXT)"
		if c.db.Name() == "mysql" {
			cast = "CAST(metadata AS CHAR)"
		}
		query := c.db.WithContext(ctx).Model(&types.TemporaryDocument{}).
			Where("tenant_id = ? AND id = ?", c.tenantID, id)
		if len(doc.Metadata) == 0 {
			query = query.Where("metadata IS NULL")
		} else {
			query = query.Where(cast+" = ?", string(doc.Metadata))
		}
		result := query.UpdateColumn("metadata", types.JSON(updated))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			return nil
		}
	}
	return fmt.Errorf("attachment description changed concurrently; retry the read")
}

func decodeAttachmentDescriptions(
	raw types.JSON,
) (map[string]json.RawMessage, map[string]attachment.Description, error) {
	metadata := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return nil, nil, err
		}
	}
	if metadata == nil {
		metadata = map[string]json.RawMessage{}
	}
	entries := map[string]attachment.Description{}
	if value := metadata[types.TemporaryDocumentImageDescriptionsKey]; len(value) > 0 {
		if err := json.Unmarshal(value, &entries); err != nil {
			return nil, nil, err
		}
	}
	if entries == nil {
		entries = map[string]attachment.Description{}
	}
	return metadata, entries, nil
}
