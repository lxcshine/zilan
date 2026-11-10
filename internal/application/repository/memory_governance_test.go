package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fencedCtx 构造一个携带清空围栏信息的写入上下文。
func fencedCtx(conversationAt time.Time, generation string) context.Context {
	return types.WithMemoryWriteFence(context.Background(), types.MemoryWriteFence{
		ConversationAt: conversationAt,
		Generation:     generation,
	})
}

// seedFact 直接落一条记忆，状态与来源由调用者指定。
func seedFact(t *testing.T, ctx context.Context, repo interfaces.MemoryRepository,
	tenantID uint64, userID, category, subject, predicate, object, status string) *types.MemoryFact {
	t.Helper()
	fact := &types.MemoryFact{
		TenantID: tenantID, UserID: userID, SessionID: "s-1",
		Category: category, Subject: subject, Predicate: predicate, Object: object,
		Content: subject + predicate + object, Status: status, Origin: types.MemoryOriginExtracted,
		NormalizedKey: types.ComputeNormalizedKey(category, subject, predicate),
	}
	require.NoError(t, repo.CreateFact(ctx, fact))
	return fact
}

// ---------------------------------------------------------------------------
// FR-01 清空时序围栏：三属性
// ---------------------------------------------------------------------------

// TestMemoryClearFenceIsMonotonic 覆盖 PRD FR-01 异常表的两行：
// "用户连续两次清空 => 第二次截止点严格晚于第一次，第一次已在途的写入同样被拦截"，
// 以及"时钟回拨 => 单调序列不回退"（同一时钟刻度用逻辑递增推进）。
func TestMemoryClearFenceIsMonotonic(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	first, err := repo.ClearAllForUser(ctx, 1, "alice")
	require.NoError(t, err)
	require.NotEmpty(t, first.Generation)
	require.False(t, first.CutoffAt.IsZero())

	// 紧接着第二次清空：即使落在同一时钟刻度，截止点也必须严格更晚。
	second, err := repo.ClearAllForUser(ctx, 1, "alice")
	require.NoError(t, err)
	require.True(t, second.CutoffAt.After(first.CutoffAt),
		"第二次清空的截止点必须严格晚于第一次")
	require.NotEqual(t, first.Generation, second.Generation, "每次清空轮换世代令牌")

	// 第一次清空后入队、第二次清空后落库的在途写入：世代已轮换，必须被拦。
	stale := fencedCtx(second.CutoffAt.Add(time.Hour), first.Generation)
	err = repo.CreateFact(stale, &types.MemoryFact{
		TenantID: 1, UserID: "alice", Category: types.MemoryCategoryFact, Content: "in-flight",
	})
	require.ErrorIs(t, err, types.ErrMemoryWriteFenced, "世代不符的在途写入必须被拦截")

	// 对话时间早于当前截止点的写入同样被拦，即使世代是当前值。
	old := fencedCtx(first.CutoffAt, second.Generation)
	err = repo.CreateFact(old, &types.MemoryFact{
		TenantID: 1, UserID: "alice", Category: types.MemoryCategoryFact, Content: "late",
	})
	require.ErrorIs(t, err, types.ErrMemoryWriteFenced, "早于截止点的写入必须被拦截")

	// 被拦截的写入只计数，不报错给用户（FR-01 规则 5）。
	gov, err := repo.GetGovernance(ctx, 1, "alice")
	require.NoError(t, err)
	require.EqualValues(t, 2, gov.BlockedWrites)

	// 当前世代 + 晚于截止点的写入正常通过。
	fresh := fencedCtx(second.CutoffAt.Add(time.Minute), second.Generation)
	require.NoError(t, repo.CreateFact(fresh, &types.MemoryFact{
		TenantID: 1, UserID: "alice", Category: types.MemoryCategoryFact, Content: "fresh",
	}))
}

// TestMemoryClearFenceIsTenantLocal 覆盖"租户本地"：租户 1 的清空不得影响租户 2，
// 同一租户内不同用户也互不影响。
func TestMemoryClearFenceIsTenantLocal(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	cleared, err := repo.ClearAllForUser(ctx, 1, "alice")
	require.NoError(t, err)

	// 其他租户的同名用户：没有围栏行，早于截止点的对话时间也照常写入。
	other := fencedCtx(cleared.CutoffAt.Add(-24*time.Hour), "")
	require.NoError(t, repo.CreateFact(other, &types.MemoryFact{
		TenantID: 2, UserID: "alice", Category: types.MemoryCategoryFact, Content: "tenant-2 fact",
	}))
	// 同租户的其他用户同样不受影响。
	require.NoError(t, repo.CreateFact(other, &types.MemoryFact{
		TenantID: 1, UserID: "bob", Category: types.MemoryCategoryFact, Content: "bob fact",
	}))

	gov, err := repo.GetGovernance(ctx, 2, "alice")
	require.NoError(t, err)
	require.Nil(t, gov, "未被清空的主体不应产生围栏状态")

	// 被清空的主体确实空了。
	_, total, err := repo.ListFacts(ctx, 1, "alice", &types.MemoryFactQuery{Status: "all"})
	require.NoError(t, err)
	require.EqualValues(t, 0, total)
	_, total, err = repo.ListFacts(ctx, 2, "alice", &types.MemoryFactQuery{Status: "all"})
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
}

// TestMemoryClearFenceAcrossTimezones 覆盖"跨时区"：截止点以 UTC 存储，
// 比较只依赖瞬时，不依赖调用方时区——同一瞬时在不同时区表示下判定结果一致。
func TestMemoryClearFenceAcrossTimezones(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	cleared, err := repo.ClearAllForUser(ctx, 1, "alice")
	require.NoError(t, err)
	require.Equal(t, time.UTC, cleared.CutoffAt.Location(), "截止点必须以 UTC 基准存储")

	east := time.FixedZone("east", 14*60*60)  // UTC+14
	west := time.FixedZone("west", -11*60*60) // UTC-11

	after := cleared.CutoffAt.Add(time.Hour)
	before := cleared.CutoffAt.Add(-time.Hour)
	cases := []struct {
		name    string
		at      time.Time
		blocked bool
	}{
		{"晚于截止点（东十四区表示）", after.In(east), false},
		{"晚于截止点（西十一区表示）", after.In(west), false},
		{"晚于截止点（UTC 表示）", after.UTC(), false},
		{"早于截止点（东十四区表示）", before.In(east), true},
		{"早于截止点（西十一区表示）", before.In(west), true},
		{"早于截止点（本地时区表示）", before.In(time.Local), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocked, err := repo.FenceBlocked(ctx, 1, "alice", tc.at, "")
			require.NoError(t, err)
			require.Equal(t, tc.blocked, blocked)
		})
	}

	// 写入路径与时区无关：同一瞬时的两种表示，一种通过、一种被拦。
	require.NoError(t, repo.CreateFact(fencedCtx(after.In(west), ""), &types.MemoryFact{
		TenantID: 1, UserID: "alice", Category: types.MemoryCategoryFact, Content: "west ok",
	}))
	require.ErrorIs(t, repo.CreateFact(fencedCtx(before.In(east), ""), &types.MemoryFact{
		TenantID: 1, UserID: "alice", Category: types.MemoryCategoryFact, Content: "east blocked",
	}), types.ErrMemoryWriteFenced)
}

// TestMemoryClearFenceCoversEveryWritePath 确认所有记忆写路径都经过围栏收口。
func TestMemoryClearFenceCoversEveryWritePath(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	fact := seedFact(t, ctx, repo, 1, "alice", types.MemoryCategoryFact, "用户", "负责", "Project X",
		types.MemoryStatusActive)
	cleared, err := repo.ClearAllForUser(ctx, 1, "alice")
	require.NoError(t, err)
	stale := fencedCtx(cleared.CutoffAt.Add(-time.Minute), cleared.Generation)

	require.ErrorIs(t, repo.CreateFact(stale, &types.MemoryFact{
		TenantID: 1, UserID: "alice", Category: types.MemoryCategoryFact, Content: "new",
	}), types.ErrMemoryWriteFenced, "CreateFact 必须被围栏拦截")

	_, err = repo.UpdateFact(stale, &types.MemoryFact{
		TenantID: 1, UserID: "alice", ID: fact.ID, Content: "edited", Status: types.MemoryStatusActive,
	})
	require.ErrorIs(t, err, types.ErrMemoryWriteFenced, "UpdateFact 必须被围栏拦截")

	require.ErrorIs(t, repo.UpsertSessionSummary(stale, &types.MemorySessionSummary{
		TenantID: 1, UserID: "alice", SessionID: "s-1", Summary: "sum",
		LastMessageAt: func() *time.Time { t := cleared.CutoffAt.Add(-time.Minute); return &t }(),
	}), types.ErrMemoryWriteFenced, "UpsertSessionSummary 必须被围栏拦截")
}

// TestMemoryClearRollsBackWhenFenceFails 覆盖 PRD FR-01 异常表第一行：
// 截止点生成失败 => 整个清空失败并回滚，不得出现"删了但没围栏"的中间态。
func TestMemoryClearRollsBackWhenFenceFails(t *testing.T) {
	repo, db := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	seedFact(t, ctx, repo, 1, "alice", types.MemoryCategoryFact, "用户", "负责", "Project X",
		types.MemoryStatusActive)
	require.NoError(t, repo.UpsertSessionSummary(ctx, &types.MemorySessionSummary{
		TenantID: 1, UserID: "alice", SessionID: "s-1", Summary: "sum",
	}))

	// 模拟围栏不可写（表缺失）：清空必须整体失败，记忆一行都不能少。
	require.NoError(t, db.Exec(`DROP TABLE memory_governance`).Error)
	_, err := repo.ClearAllForUser(ctx, 1, "alice")
	require.Error(t, err, "围栏生成失败时清空必须失败")

	_, total, listErr := repo.ListFacts(ctx, 1, "alice", &types.MemoryFactQuery{Status: "all"})
	require.NoError(t, listErr)
	require.EqualValues(t, 1, total, "围栏失败时不得删除记忆（无中间态）")
	summaries, err := repo.CountSessionSummaries(ctx, 1, "alice")
	require.NoError(t, err)
	require.EqualValues(t, 1, summaries)
}

// ---------------------------------------------------------------------------
// FR-02 / FR-03：状态机转移原语
// ---------------------------------------------------------------------------

func TestMemoryRepositoryConfirmPendingSupersedesConflicts(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	old := seedFact(t, ctx, repo, 1, "alice", types.MemoryCategoryFact, "用户", "岗位", "运维工程师",
		types.MemoryStatusActive)
	proposal := seedFact(t, ctx, repo, 1, "alice", types.MemoryCategoryFact, "用户", "岗位", "平台架构师",
		types.MemoryStatusPending)

	require.NoError(t, repo.ConfirmFact(ctx, 1, "alice", proposal.ID, []string{old.ID}))

	got, err := repo.GetFactByID(ctx, 1, "alice", proposal.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusActive, got.Status)

	replaced, err := repo.GetFactByID(ctx, 1, "alice", old.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusSuperseded, replaced.Status, "旧值转 superseded 而非删除")
	require.Equal(t, proposal.ID, replaced.SupersededBy)
	require.NotNil(t, replaced.SupersededAt, "必须保留取代时间")
	require.Equal(t, "用户岗位运维工程师", replaced.Content, "原文必须保留")

	// 重复确认同一条：状态机拒绝（已不是 pending）。
	require.ErrorIs(t, repo.ConfirmFact(ctx, 1, "alice", proposal.ID, nil), types.ErrMemoryConflict)
	// 确认不存在的记忆：映射为 not found。
	require.ErrorIs(t, repo.ConfirmFact(ctx, 1, "alice", "missing", nil), gorm.ErrRecordNotFound)
}

func TestMemoryRepositoryRejectAndRestore(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	proposal := seedFact(t, ctx, repo, 1, "alice", types.MemoryCategoryPreference, "用户", "偏好", "Vim",
		types.MemoryStatusPending)
	require.NoError(t, repo.RejectFact(ctx, 1, "alice", proposal.ID))
	got, err := repo.GetFactByID(ctx, 1, "alice", proposal.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusArchived, got.Status, "拒绝 => archived（可恢复）")

	// 恢复归档项 => active。
	require.NoError(t, repo.RestoreFact(ctx, 1, "alice", proposal.ID, nil))
	got, err = repo.GetFactByID(ctx, 1, "alice", proposal.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusActive, got.Status)

	// 恢复被取代的旧值时，当前 active 项转为 superseded（FR-03 规则 5）。
	current := seedFact(t, ctx, repo, 1, "alice", types.MemoryCategoryFact, "用户", "岗位", "架构师",
		types.MemoryStatusActive)
	historic := seedFact(t, ctx, repo, 1, "alice", types.MemoryCategoryFact, "用户", "岗位", "工程师",
		types.MemoryStatusSuperseded)
	require.NoError(t, repo.RestoreFact(ctx, 1, "alice", historic.ID, []string{current.ID}))

	got, err = repo.GetFactByID(ctx, 1, "alice", historic.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusActive, got.Status)
	got, err = repo.GetFactByID(ctx, 1, "alice", current.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusSuperseded, got.Status)
	require.Equal(t, historic.ID, got.SupersededBy)

	// active 项不可"恢复"（状态机不允许）。
	require.ErrorIs(t, repo.RestoreFact(ctx, 1, "alice", historic.ID, nil), types.ErrMemoryConflict)
	// 拒绝一条非 pending 记忆同样冲突。
	require.ErrorIs(t, repo.RejectFact(ctx, 1, "alice", historic.ID), types.ErrMemoryConflict)
}

func TestMemoryRepositoryArchiveAndHardDelete(t *testing.T) {
	repo, db := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	active := seedFact(t, ctx, repo, 1, "alice", types.MemoryCategoryFact, "用户", "负责", "A",
		types.MemoryStatusActive)
	pending := seedFact(t, ctx, repo, 1, "alice", types.MemoryCategoryFact, "用户", "负责", "B",
		types.MemoryStatusPending)

	// fromStatuses=[active] 时不得触及 pending（两类容量互相独立）。
	n, err := repo.ArchiveFactsByIDs(ctx, 1, "alice", []string{active.ID, pending.ID},
		[]string{types.MemoryStatusActive})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	got, err := repo.GetFactByID(ctx, 1, "alice", active.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusArchived, got.Status, "归档是可逆操作，不是删除")
	got, err = repo.GetFactByID(ctx, 1, "alice", pending.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusPending, got.Status)

	// 物理删除后连软删行都不留。
	n, err = repo.HardDeleteFacts(ctx, 1, "alice", []string{active.ID})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	var count int64
	require.NoError(t, db.Model(&types.MemoryFact{}).Unscoped().
		Where("id = ?", active.ID).Count(&count).Error)
	require.EqualValues(t, 0, count, "物理删除不可恢复")
}

func TestMemoryRepositoryTrimPendingNeverTouchesActive(t *testing.T) {
	repo, db := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		fact := seedFact(t, ctx, repo, 1, "alice", types.MemoryCategoryFact, "用户", "提及",
			"pending-"+string(rune('a'+i)), types.MemoryStatusPending)
		// 显式错开创建时间，保证"最旧者优先归档"可判定。
		createdAt := time.Now().Add(-time.Duration(5-i) * time.Hour)
		require.NoError(t, db.Model(&types.MemoryFact{}).Where("id = ?", fact.ID).
			Update("created_at", createdAt).Error)
	}
	active := seedFact(t, ctx, repo, 1, "alice", types.MemoryCategoryFact, "用户", "负责", "active",
		types.MemoryStatusActive)

	trimmed, err := repo.TrimPendingFacts(ctx, 1, "alice", 2)
	require.NoError(t, err)
	require.Len(t, trimmed, 3, "只保留最新 2 条 pending")
	require.Equal(t, "pending-a", trimmed[0].Object, "最旧的 pending 先归档")

	counts, err := repo.CountFactsByStatus(ctx, 1, "alice")
	require.NoError(t, err)
	require.EqualValues(t, 2, counts[types.MemoryStatusPending])
	require.EqualValues(t, 3, counts[types.MemoryStatusArchived])
	require.EqualValues(t, 1, counts[types.MemoryStatusActive], "待确认超限绝不得触及 active")

	got, err := repo.GetFactByID(ctx, 1, "alice", active.ID)
	require.NoError(t, err)
	require.Equal(t, types.MemoryStatusActive, got.Status)

	// 未超限时不做任何归档。
	trimmed, err = repo.TrimPendingFacts(ctx, 1, "alice", 2)
	require.NoError(t, err)
	require.Empty(t, trimmed)
}

func TestMemoryRepositoryConflictKeyChain(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	key := types.ComputeNormalizedKey(types.MemoryCategoryFact, "用户", "岗位")
	require.NotEmpty(t, key)
	// 同 (category, subject, predicate) 不同 object => 同键。
	require.Equal(t, key, types.ComputeNormalizedKey(types.MemoryCategoryFact, " 用户 ", "岗位"))
	// object 不参与键：三元组完全相同属去重而非矛盾。
	require.NotEqual(t, key, types.ComputeTripleHash(types.MemoryCategoryFact, "用户", "岗位", "A"))
	// 无主语无谓语 => 空键，不参与矛盾判定。
	require.Empty(t, types.ComputeNormalizedKey(types.MemoryCategoryFact, "", ""))

	first := seedFact(t, ctx, repo, 1, "alice", types.MemoryCategoryFact, "用户", "岗位", "工程师",
		types.MemoryStatusSuperseded)
	second := seedFact(t, ctx, repo, 1, "alice", types.MemoryCategoryFact, "用户", "岗位", "架构师",
		types.MemoryStatusActive)
	// 其他用户的同键记忆不得混入。
	seedFact(t, ctx, repo, 1, "bob", types.MemoryCategoryFact, "用户", "岗位", "设计师",
		types.MemoryStatusActive)

	chain, err := repo.ListFactsByConflictKey(ctx, 1, "alice", key, 10)
	require.NoError(t, err)
	require.Len(t, chain, 2)
	ids := map[string]bool{chain[0].ID: true, chain[1].ID: true}
	require.True(t, ids[first.ID] && ids[second.ID])

	empty, err := repo.ListFactsByConflictKey(ctx, 1, "alice", "", 10)
	require.NoError(t, err)
	require.Empty(t, empty)
}

// ---------------------------------------------------------------------------
// FR-04 容量治理：归档失败计数与治理事件
// ---------------------------------------------------------------------------

func TestMemoryRepositoryArchiveFailurePausesExtraction(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	for i := 0; i < types.MemoryArchiveFailureThreshold-1; i++ {
		gov, err := repo.RecordArchiveFailure(ctx, 1, "alice")
		require.NoError(t, err)
		require.False(t, gov.ExtractionPaused(), "未达阈值不暂停抽取")
	}
	gov, err := repo.RecordArchiveFailure(ctx, 1, "alice")
	require.NoError(t, err)
	require.EqualValues(t, types.MemoryArchiveFailureThreshold, gov.ArchiveFailures)
	require.True(t, gov.ExtractionPaused(), "连续失败达阈值 => 暂停自动抽取")

	require.NoError(t, repo.ResetArchiveFailures(ctx, 1, "alice"))
	gov, err = repo.GetGovernance(ctx, 1, "alice")
	require.NoError(t, err)
	require.Zero(t, gov.ArchiveFailures)
	require.False(t, gov.ExtractionPaused(), "归档恢复后解除暂停")
}

func TestMemoryRepositoryGovernanceEventsAreOneShot(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		require.NoError(t, repo.CreateGovernanceEvent(ctx, &types.MemoryGovernanceEvent{
			TenantID: 1, UserID: "alice", Kind: types.MemoryEventCapacityArchived,
			Reason: "超过上限", ItemCount: 2,
			ItemIDs:      types.StringArray{"f-1", "f-2"},
			ItemContents: types.StringArray{"旧偏好 A", "旧偏好 B"},
		}))
	}
	events, err := repo.ListUnacknowledgedGovernanceEvents(ctx, 1, "alice", 20)
	require.NoError(t, err)
	require.Len(t, events, 3)
	require.Equal(t, types.StringArray{"旧偏好 A", "旧偏好 B"}, events[0].ItemContents)

	acked, err := repo.AcknowledgeGovernanceEvents(ctx, 1, "alice", []int64{events[0].ID})
	require.NoError(t, err)
	require.EqualValues(t, 1, acked)

	events, err = repo.ListUnacknowledgedGovernanceEvents(ctx, 1, "alice", 20)
	require.NoError(t, err)
	require.Len(t, events, 2, "已提示的事件不再重复提示")

	// 其他用户看不到 alice 的事件。
	other, err := repo.ListUnacknowledgedGovernanceEvents(ctx, 1, "bob", 20)
	require.NoError(t, err)
	require.Empty(t, other)
}
