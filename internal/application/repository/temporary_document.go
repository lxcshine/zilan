package repository

import (
	"context"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

type temporaryDocumentRepository struct{ db *gorm.DB }

func NewTemporaryDocumentRepository(db *gorm.DB) interfaces.TemporaryDocumentRepository {
	return &temporaryDocumentRepository{db: db}
}

func (r *temporaryDocumentRepository) Create(ctx context.Context, document *types.TemporaryDocument) error {
	return r.db.WithContext(ctx).Create(document).Error
}

func (r *temporaryDocumentRepository) GetByID(ctx context.Context, tenantID uint64, documentID string) (*types.TemporaryDocument, error) {
	var document types.TemporaryDocument
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, documentID).First(&document).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &document, err
}

func (r *temporaryDocumentRepository) GetScoped(ctx context.Context, tenantID uint64, sessionID, documentID string) (*types.TemporaryDocument, error) {
	var document types.TemporaryDocument
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND session_id = ? AND id = ?", tenantID, sessionID, documentID).First(&document).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &document, err
}

func (r *temporaryDocumentRepository) ListScoped(ctx context.Context, tenantID uint64, sessionID string) ([]*types.TemporaryDocument, error) {
	var documents []*types.TemporaryDocument
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND session_id = ?", tenantID, sessionID).Order("created_at ASC").Find(&documents).Error
	return documents, err
}

func (r *temporaryDocumentRepository) MarkProcessing(ctx context.Context, tenantID uint64, documentID string, startedAt time.Time) error {
	return r.db.WithContext(ctx).Model(&types.TemporaryDocument{}).
		Where("tenant_id = ? AND id = ?", tenantID, documentID).
		Updates(map[string]interface{}{"status": types.TemporaryDocumentStatusProcessing, "started_at": startedAt, "error_message": ""}).Error
}

func (r *temporaryDocumentRepository) MarkReady(ctx context.Context, tenantID uint64, documentID, content string, chunks, imageRefs, metadata types.JSON, tokenCount, chunkCount int, readyAt time.Time) error {
	return r.db.WithContext(ctx).Model(&types.TemporaryDocument{}).
		Where("tenant_id = ? AND id = ?", tenantID, documentID).
		Updates(map[string]interface{}{
			"status": types.TemporaryDocumentStatusReady, "content": content, "chunks": chunks,
			"image_refs": imageRefs, "metadata": metadata, "token_count": tokenCount,
			"chunk_count": chunkCount, "ready_at": readyAt, "error_message": "",
		}).Error
}

func (r *temporaryDocumentRepository) MarkFailed(ctx context.Context, tenantID uint64, documentID, message string) error {
	return r.db.WithContext(ctx).Model(&types.TemporaryDocument{}).
		Where("tenant_id = ? AND id = ?", tenantID, documentID).
		Updates(map[string]interface{}{"status": types.TemporaryDocumentStatusFailed, "error_message": message}).Error
}

func (r *temporaryDocumentRepository) DeleteScoped(ctx context.Context, tenantID uint64, sessionID, documentID string) error {
	return r.db.WithContext(ctx).Where("tenant_id = ? AND session_id = ? AND id = ?", tenantID, sessionID, documentID).Delete(&types.TemporaryDocument{}).Error
}

func (r *temporaryDocumentRepository) ListExpired(ctx context.Context, before time.Time, limit int) ([]*types.TemporaryDocument, error) {
	var documents []*types.TemporaryDocument
	err := r.expiredQuery(ctx, before, limit).Find(&documents).Error
	return documents, err
}

// expiredQuery 挑出可以清理的过期附件（PRD FR-22 ⑥）。
//
// 附件一旦被发出就成为会话历史的一部分，因此只要还有存活的会话引用它，
// 源文件与解析结果都要保留 —— 包括分叉会话：分叉会话复制了用户消息（连带附件引用），
// 但 temporary_documents.session_id 仍指向原会话，所以豁免必须靠 messages.attachments
// 反查，而不是靠 session_id 相等。
//
// 过滤必须发生在 LIMIT 之前，否则被保留的上传会一直占满清理队列，
// 让真正该删的行永远轮不到。
func (r *temporaryDocumentRepository) expiredQuery(ctx context.Context, before time.Time, limit int) *gorm.DB {
	query := r.db.WithContext(ctx).Model(&types.TemporaryDocument{}).
		Where("expires_at <= ?", before).Order("expires_at ASC").Limit(limit)
	if r.db.Name() == "sqlite" {
		// SQLite 无法为 JSON 数组成员建索引。这里把"存活引用"的
		// 租户/文档键一次性物化出来，而不是为每个过期文档再展开一遍所有消息。
		// 标量键可以走单次索引 IN 查找；IS NOT NULL 用来避开 NOT IN 的 NULL 陷阱
		// （子查询里出现 NULL 会让整个 NOT IN 恒为假）。租户 ID 是数字，
		// 所以 ':' 分隔符不会跨租户碰撞。
		referenced := r.db.Table("messages m").
			Select("CAST(s.tenant_id AS TEXT) || ':' || json_extract(a.value, '$.id')").
			Joins("JOIN sessions s ON s.id = m.session_id").
			Joins("JOIN json_each(CAST(m.attachments AS TEXT)) a").
			Where("m.role = 'user' AND m.deleted_at IS NULL AND s.deleted_at IS NULL").
			Where("json_extract(a.value, '$.id') IS NOT NULL")
		return query.Where(liveAttachmentKeyExpr("temporary_documents")+" NOT IN (?)", referenced)
	}
	// PostgreSQL 用 attachments 上的部分 GIN 索引服务这个包含谓词；
	// 索引谓词必须与这里的存活用户消息谓词逐字一致，否则不走索引会退化成全表扫。
	referenced := r.db.Table("messages m").Select("1").
		Joins("JOIN sessions s ON s.id = m.session_id").
		Where("m.role = 'user'").
		Where("s.tenant_id = temporary_documents.tenant_id AND s.deleted_at IS NULL AND m.deleted_at IS NULL").
		Where(containsAttachmentPredicate(r.db.Name()))
	return query.Where("NOT EXISTS (?)", referenced)
}

// containsAttachmentPredicate 返回"messages.attachments 里含有该文档 ID"的方言写法。
// 抽成独立函数是为了让 PostgreSQL / MySQL 两种无法在内存库里真跑的方言也能被测试钉住：
// 写法一旦与迁移里建的部分索引谓词漂移，清理查询就会静默退化成全表扫。
func containsAttachmentPredicate(dialect string) string {
	if dialect == "mysql" {
		return "JSON_CONTAINS(m.attachments, JSON_OBJECT('id', temporary_documents.id))"
	}
	return "m.attachments @> jsonb_build_array(jsonb_build_object('id', temporary_documents.id))"
}

// liveAttachmentKeyExpr 是 SQLite 方言下用于 NOT IN 判断的标量键表达式。
// SQLite 无法为 JSON 数组成员建索引，因此把存活引用物化成 "tenant_id:document_id"
// 字符串键，再走一次索引 IN 查找。
func liveAttachmentKeyExpr(table string) string {
	return "(CAST(" + table + ".tenant_id AS TEXT) || ':' || " + table + ".id)"
}
