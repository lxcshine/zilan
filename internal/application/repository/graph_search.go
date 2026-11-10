package repository

import (
	"context"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

// graphSearchEvidenceRepository 把“图谱实体引用的 chunk”投影成公开 API 可以
// 返回的有界结构：只取需要的列，且在同一条 SQL 里完成租户/知识库/软删/启用
// 状态校验，避免先取 ID 再回表带来的竞态与越权面。
type graphSearchEvidenceRepository struct {
	db *gorm.DB
}

// NewGraphSearchEvidenceRepository 构造上述有界投影仓储。
func NewGraphSearchEvidenceRepository(db *gorm.DB) interfaces.GraphSearchEvidenceRepository {
	return &graphSearchEvidenceRepository{db: db}
}

// GetGraphSourceChunks 返回 ids 中确实属于 (tenantID, kbID)、且文档与分块都
// 仍然有效的来源分块。内容与标题在 SQL 层就被 SUBSTR 截断，多取一个字符用于
// 判断“是否被裁掉”，从而如实上报 ContentTruncated / TitleTruncated。
func (r *graphSearchEvidenceRepository) GetGraphSourceChunks(
	ctx context.Context, tenantID uint64, kbID string, ids []string,
) ([]types.GraphSearchSourceChunk, error) {
	if tenantID == 0 || kbID == "" || len(ids) > types.GraphSearchMaxEvidence {
		return nil, fmt.Errorf("invalid graph evidence scope or budget")
	}
	rows := []types.GraphSearchSourceChunk{}
	if len(ids) == 0 {
		return rows, nil
	}
	// Table/Joins 不会套用两个模型的 GORM 默认作用域，因此软删条件必须显式写。
	// SUBSTR 各家 SQL 后端都支持；多取一个字符即可判断裁剪，既不会把无界正文
	// 读进内存，也不需要第二次“metadata → content”的竞态查询。
	err := r.db.WithContext(ctx).Table("chunks AS c").
		Select(`c.id, c.tenant_id, c.knowledge_base_id, c.knowledge_id,
			c.chunk_index, c.start_at, c.end_at,
			SUBSTR(c.content, 1, ?) AS content, SUBSTR(k.title, 1, 513) AS title`,
			types.GraphSearchMaxContentRunes+1).
		Joins(`JOIN knowledges AS k ON k.id = c.knowledge_id
			AND k.tenant_id = c.tenant_id AND k.knowledge_base_id = c.knowledge_base_id`).
		Where("c.tenant_id = ? AND c.knowledge_base_id = ? AND c.id IN ?", tenantID, kbID, ids).
		Where("c.deleted_at IS NULL AND k.deleted_at IS NULL AND c.is_enabled = ?", true).
		Where("k.enable_status = ? AND k.parse_status <> ?", "enabled", types.ParseStatusDeleting).
		Order("c.id").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].Content, rows[i].ContentTruncated = graphSearchClip(rows[i].Content, types.GraphSearchMaxContentRunes)
		rows[i].Title, rows[i].TitleTruncated = graphSearchClip(rows[i].Title, 512)
	}
	return rows, nil
}

// graphSearchClip 按 rune 裁剪并报告是否发生裁剪。入参长度已被 SQL 限住，
// 这里的 []rune 转换不会引入无界分配。
func graphSearchClip(text string, limit int) (string, bool) {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]), true
	}
	return text, false
}
