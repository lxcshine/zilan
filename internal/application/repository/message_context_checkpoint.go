package repository

import (
	"context"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
)

// UpdateMessageContextCheckpoint 只写 messages.context_checkpoint 一列（PRD FR-10）。
//
// 限定 role='assistant' 且限定会话：检查点身份是「被覆盖的最后一轮的 assistant 行」，
// 写到别的行会让下一轮从错误的边界起读历史。没有匹配到行时报错——静默丢掉一次写入
// 会让「摘要已经存好」这个前提变成假的。
func (r *messageRepository) UpdateMessageContextCheckpoint(
	ctx context.Context, sessionID, messageID string, checkpoint *types.ContextCheckpoint,
) error {
	result := r.db.WithContext(ctx).
		Model(&types.Message{}).
		Where("id = ? AND session_id = ? AND role = 'assistant'", messageID, sessionID).
		Update("context_checkpoint", checkpoint)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("no assistant message %s in session %s", messageID, sessionID)
	}
	return nil
}

// GetLatestContextCheckpoint 返回会话中最新一份带压缩检查点的 assistant 消息。
// 只取身份、排序列与检查点本身：调用方拿它去和自己已经装载的轮次对齐，不需要整行。
// 没有检查点时返回 (nil, nil)。
func (r *messageRepository) GetLatestContextCheckpoint(
	ctx context.Context, sessionID string,
) (*types.Message, error) {
	var messages []*types.Message
	if err := r.db.WithContext(ctx).
		Select("id", "session_id", "request_id", "role", "created_at", "context_checkpoint").
		Where("session_id = ? AND role = 'assistant' AND context_checkpoint IS NOT NULL", sessionID).
		Order("created_at DESC, id DESC").
		Limit(1).
		Find(&messages).Error; err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, nil
	}
	return messages[0], nil
}
