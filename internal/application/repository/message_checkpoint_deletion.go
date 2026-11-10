package repository

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// invalidateDependentCheckpoints 清空覆盖了 message 所在轮次及其之后所有轮次的压缩
// 检查点（PRD FR-10 规则 3：对已压缩区间内消息的任何修改都使检查点失效）。
//
// 检查点记录的是「该轮及其之前的全部内容已被摘要替代」，因此一旦其中某轮被删除、
// 撤回或改写，从那一轮起的所有摘要都在描述不再存在的历史。置 NULL 让下一轮退回
// 全量装载与重算，代价是一次摘要调用，而不是一段错误的记忆。
//
// 必须在调用方的事务内执行：失效与删除要么一起成功，要么一起回滚。
func invalidateDependentCheckpoints(tx *gorm.DB, message *types.Message) error {
	if message == nil || message.SessionID == "" {
		return nil
	}
	boundary := *message
	if message.RequestID != "" {
		// 一轮可能有多条消息（例如中途插入的用户消息比它的 assistant 行更新）。
		// 检查点覆盖的是整轮，因此边界回溯到该轮最早的那一行。
		boundary = types.Message{}
		if err := tx.Where("session_id = ? AND request_id = ?", message.SessionID, message.RequestID).
			Order("created_at ASC, id ASC").First(&boundary).Error; err != nil {
			return err
		}
	}
	return tx.Model(&types.Message{}).
		Where("session_id = ? AND role = 'assistant' AND context_checkpoint IS NOT NULL", message.SessionID).
		Where("created_at > ? OR (created_at = ? AND id >= ?)", boundary.CreatedAt, boundary.CreatedAt, boundary.ID).
		UpdateColumn("context_checkpoint", nil).Error
}

// deleteMessageInvalidatingCheckpoints 在同一事务里删除消息并失效依赖它的检查点。
// Zilan 的消息删除（撤回）是唯一会改写已压缩区间的持久化路径，因此失效逻辑挂在这里。
func deleteMessageInvalidatingCheckpoints(
	ctx context.Context, db *gorm.DB, sessionID, messageID string,
) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var message types.Message
		if err := tx.Where("id = ? AND session_id = ?", messageID, sessionID).First(&message).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				// 没有这一行也就没有依赖它的检查点：与既有语义一致，删除是幂等的。
				return nil
			}
			return err
		}
		if err := invalidateDependentCheckpoints(tx, &message); err != nil {
			return err
		}
		return tx.Where("id = ? AND session_id = ?", messageID, sessionID).Delete(&types.Message{}).Error
	})
}
