package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// 本文件覆盖 PRD FR-10 的检查点持久化与失效：写入范围、最新检查点读取、
// 删除消息（撤回）时失效依赖它的检查点。

func newMessageRepositoryForCheckpointTest(t *testing.T) (*messageRepository, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.Message{}))
	return &messageRepository{db: db}, db
}

// seedCheckpointMessage 写入一行消息并返回它。types.Message 的 BeforeCreate 钩子会
// 重写主键，因此断言必须用返回值的 ID。
func seedCheckpointMessage(
	t *testing.T, db *gorm.DB, sessionID, requestID, role, content string, at time.Time,
) *types.Message {
	t.Helper()
	msg := &types.Message{
		SessionID:   sessionID,
		RequestID:   requestID,
		Role:        role,
		Content:     content,
		IsCompleted: true,
		CreatedAt:   at,
	}
	require.NoError(t, db.Create(msg).Error)
	return msg
}

// seedCheckpointTurn 写入一轮（user + assistant）；summary 非空时给 assistant 行
// 挂一份检查点。
func seedCheckpointTurn(
	t *testing.T, db *gorm.DB, sessionID, requestID string, at time.Time, summary string,
) (*types.Message, *types.Message) {
	t.Helper()
	user := seedCheckpointMessage(t, db, sessionID, requestID, "user", "问题 "+requestID, at)
	assistant := seedCheckpointMessage(t, db, sessionID, requestID, "assistant",
		"回答 "+requestID, at.Add(time.Second))
	if summary != "" {
		require.NoError(t, db.Model(&types.Message{}).Where("id = ?", assistant.ID).
			Update("context_checkpoint", &types.ContextCheckpoint{
				Summary:      summary,
				CreatedAt:    at.Add(time.Second),
				PrefixHash:   "hash-" + requestID,
				AnchorTurnID: requestID,
				CoveredTurns: 1,
			}).Error)
	}
	return user, assistant
}

func checkpointOf(t *testing.T, db *gorm.DB, id string) *types.ContextCheckpoint {
	t.Helper()
	var msg types.Message
	// Unscoped：被删除的行也要能读回，用于断言「删除时检查点被清空」。
	require.NoError(t, db.Unscoped().Where("id = ?", id).First(&msg).Error)
	return msg.ContextCheckpoint
}

func TestUpdateMessageContextCheckpointIsScopedToSessionAssistantRows(t *testing.T) {
	repo, db := newMessageRepositoryForCheckpointTest(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	user := seedCheckpointMessage(t, db, "s1", "r1", "user", "问题", base)
	assistant := seedCheckpointMessage(t, db, "s1", "r1", "assistant", "回答", base.Add(time.Second))
	otherSession := seedCheckpointMessage(t, db, "s2", "r1", "assistant", "别的会话", base)

	cp := &types.ContextCheckpoint{Summary: "摘要", PrefixHash: "hash"}
	require.Error(t, repo.UpdateMessageContextCheckpoint(ctx, "s2", assistant.ID, cp),
		"跨会话写入必须失败")
	require.Error(t, repo.UpdateMessageContextCheckpoint(ctx, "s1", user.ID, cp),
		"检查点只能落在 assistant 行上")
	require.Error(t, repo.UpdateMessageContextCheckpoint(ctx, "s1", "不存在的消息", cp),
		"没有匹配到行就不算保存成功")
	require.NoError(t, repo.UpdateMessageContextCheckpoint(ctx, "s1", assistant.ID, cp))

	got, err := repo.GetLatestContextCheckpoint(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, assistant.ID, got.ID)
	require.NotNil(t, got.ContextCheckpoint)
	require.Equal(t, "摘要", got.ContextCheckpoint.Summary)
	require.Equal(t, "hash", got.ContextCheckpoint.PrefixHash)

	none, err := repo.GetLatestContextCheckpoint(ctx, "s3")
	require.NoError(t, err)
	require.Nil(t, none, "没有检查点的会话返回 nil 而不是错误")
	require.Nil(t, checkpointOf(t, db, otherSession.ID), "别的会话不受影响")
}

func TestGetLatestContextCheckpointReturnsTheNewestOne(t *testing.T) {
	repo, db := newMessageRepositoryForCheckpointTest(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	seedCheckpointTurn(t, db, "s1", "r1", base, "覆盖到第一轮")
	_, newest := seedCheckpointTurn(t, db, "s1", "r2", base.Add(time.Hour), "覆盖到第二轮")

	got, err := repo.GetLatestContextCheckpoint(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, newest.ID, got.ID)
	require.Equal(t, "覆盖到第二轮", got.ContextCheckpoint.Summary)
}

func TestDeleteMessageInvalidatesDependentCheckpoints(t *testing.T) {
	repo, db := newMessageRepositoryForCheckpointTest(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	_, first := seedCheckpointTurn(t, db, "s1", "r1", base, "覆盖到第一轮")
	rewoundUser, second := seedCheckpointTurn(t, db, "s1", "r2", base.Add(time.Hour), "覆盖到第二轮")
	_, third := seedCheckpointTurn(t, db, "s1", "r3", base.Add(2*time.Hour), "覆盖到第三轮")
	// 另一个会话复用了同样的 request_id：边界必须按会话隔离。
	_, other := seedCheckpointTurn(t, db, "other", "r2", base.Add(3*time.Hour), "别的会话")

	// 撤回第二轮的用户消息：覆盖它及其之后的检查点全部失效，更早的那份保留。
	require.NoError(t, repo.DeleteMessage(ctx, "s1", rewoundUser.ID))

	require.Nil(t, checkpointOf(t, db, second.ID), "被撤回那一轮的检查点必须清空")
	require.Nil(t, checkpointOf(t, db, third.ID), "更晚的检查点描述的历史已不完整，同样清空")
	require.NotNil(t, checkpointOf(t, db, first.ID), "更早的检查点不受影响")

	got, err := repo.GetLatestContextCheckpoint(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, first.ID, got.ID, "只剩第一轮的检查点可用")

	otherCheckpoint, err := repo.GetLatestContextCheckpoint(ctx, "other")
	require.NoError(t, err)
	require.NotNil(t, otherCheckpoint)
	require.Equal(t, other.ID, otherCheckpoint.ID, "别的会话即使复用同一个 request_id 也不受影响")
}

func TestDeleteAssistantRowInvalidatesItsOwnCheckpoint(t *testing.T) {
	repo, db := newMessageRepositoryForCheckpointTest(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	_, first := seedCheckpointTurn(t, db, "s1", "r1", base, "覆盖到第一轮")
	_, second := seedCheckpointTurn(t, db, "s1", "r2", base.Add(time.Hour), "覆盖到第二轮")

	require.NoError(t, repo.DeleteMessage(ctx, "s1", second.ID))
	require.Nil(t, checkpointOf(t, db, second.ID))

	got, err := repo.GetLatestContextCheckpoint(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, first.ID, got.ID)
}

func TestDeleteMissingMessageIsIdempotent(t *testing.T) {
	repo, _ := newMessageRepositoryForCheckpointTest(t)
	require.NoError(t, repo.DeleteMessage(context.Background(), "s1", "不存在"))
}

func TestUpdateMessageKeepsAnExistingContextCheckpoint(t *testing.T) {
	repo, db := newMessageRepositoryForCheckpointTest(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	_, assistant := seedCheckpointTurn(t, db, "s1", "r1", base, "保留我")

	// 常规更新（写入最终答案等）不得顺手清掉检查点：字段是 nil 指针，
	// gorm 的 Updates 会跳过它。
	require.NoError(t, repo.UpdateMessage(ctx, &types.Message{
		ID: assistant.ID, SessionID: "s1", Content: "改写后的回答",
	}))

	require.Equal(t, "保留我", checkpointOf(t, db, assistant.ID).Summary)
}

func TestContextCheckpointRoundTripsThroughTheColumn(t *testing.T) {
	repo, db := newMessageRepositoryForCheckpointTest(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	assistant := seedCheckpointMessage(t, db, "s1", "r1", "assistant", "回答", base)

	want := &types.ContextCheckpoint{
		Summary:      "## Goal\n继续任务",
		CreatedAt:    base,
		Degraded:     true,
		PrefixHash:   "chain-hash",
		AnchorTurnID: "r0",
		BaseHash:     "base-hash",
		CoveredTurns: 5,
		Source:       types.ContextCheckpointSourceAgent,
	}
	require.NoError(t, repo.UpdateMessageContextCheckpoint(ctx, "s1", assistant.ID, want))

	got, err := repo.GetLatestContextCheckpoint(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, got.ContextCheckpoint)
	require.Equal(t, want.Summary, got.ContextCheckpoint.Summary)
	require.Equal(t, want.PrefixHash, got.ContextCheckpoint.PrefixHash)
	require.Equal(t, want.AnchorTurnID, got.ContextCheckpoint.AnchorTurnID)
	require.Equal(t, want.BaseHash, got.ContextCheckpoint.BaseHash)
	require.Equal(t, want.CoveredTurns, got.ContextCheckpoint.CoveredTurns)
	require.Equal(t, want.Source, got.ContextCheckpoint.Source)
	require.True(t, got.ContextCheckpoint.Degraded)
	require.True(t, got.ContextCheckpoint.CreatedAt.Equal(want.CreatedAt))

	// 空摘要的检查点必须落成 NULL，而不是一个空对象。
	require.NoError(t, repo.UpdateMessageContextCheckpoint(ctx, "s1", assistant.ID,
		&types.ContextCheckpoint{Summary: "  "}))
	none, err := repo.GetLatestContextCheckpoint(ctx, "s1")
	require.NoError(t, err)
	require.Nil(t, none, "空检查点写成 NULL，因此不再被当作最新检查点")
}
