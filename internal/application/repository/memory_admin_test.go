package repository

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// ---------------------------------------------------------------------------
// FR-09 记忆管理控制台：工作区级聚合
// ---------------------------------------------------------------------------

// seedAdminFact 直接落一条指定 (租户, 用户, 类型, 状态) 的记忆。
// tag 参与三元组，保证 triple_hash 唯一——(租户, 用户, triple_hash) 上有唯一索引，
// 完全相同的三元组会被去重成一行，计数就不准了。
func seedAdminFact(t *testing.T, ctx context.Context, repo interfaces.MemoryRepository,
	tenantID uint64, userID, category, status, tag string) *types.MemoryFact {
	t.Helper()
	fact := &types.MemoryFact{
		TenantID: tenantID, UserID: userID, SessionID: "s-" + tag,
		Category: category, Subject: "用户", Predicate: "标记", Object: tag,
		Content: userID + " " + tag, Status: status, Origin: types.MemoryOriginExtracted,
		NormalizedKey: types.ComputeNormalizedKey(category, "用户", "标记"),
	}
	require.NoError(t, repo.CreateFact(ctx, fact))
	require.NotEmpty(t, fact.ID)
	return fact
}

// seedAdminWorkspace 铺一份固定的工作区数据，供本文件的各个用例共用：
//
//	租户 1  alice 3 条（active profile / pending fact / archived preference）
//	        bob   2 条（active fact ×2）
//	        carol 1 条（superseded todo）
//	        dave  0 条
//	租户 2  alice 5 条（active fact）——必须完全不出现在租户 1 的聚合里
func seedAdminWorkspace(t *testing.T, ctx context.Context, repo interfaces.MemoryRepository) map[string]*types.MemoryFact {
	t.Helper()
	facts := map[string]*types.MemoryFact{}
	facts["alice-active"] = seedAdminFact(t, ctx, repo, 1, "alice", types.MemoryCategoryProfile, types.MemoryStatusActive, "a1")
	facts["alice-pending"] = seedAdminFact(t, ctx, repo, 1, "alice", types.MemoryCategoryFact, types.MemoryStatusPending, "a2")
	facts["alice-archived"] = seedAdminFact(t, ctx, repo, 1, "alice", types.MemoryCategoryPreference, types.MemoryStatusArchived, "a3")
	facts["bob-1"] = seedAdminFact(t, ctx, repo, 1, "bob", types.MemoryCategoryFact, types.MemoryStatusActive, "b1")
	facts["bob-2"] = seedAdminFact(t, ctx, repo, 1, "bob", types.MemoryCategoryFact, types.MemoryStatusActive, "b2")
	facts["carol-1"] = seedAdminFact(t, ctx, repo, 1, "carol", types.MemoryCategoryTodo, types.MemoryStatusSuperseded, "c1")
	for i := 0; i < 5; i++ {
		seedAdminFact(t, ctx, repo, 2, "alice", types.MemoryCategoryFact, types.MemoryStatusActive, "t2-"+string(rune('a'+i)))
	}
	return facts
}

// TestCountTenantFactsByStatus 覆盖 FR-09 规则 1 的"按状态分布"，并验证聚合
// 严格限定在工作区内（FR-09 规则 4）。
func TestCountTenantFactsByStatus(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()
	seedAdminWorkspace(t, ctx, repo)

	byStatus, err := repo.CountTenantFactsByStatus(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{
		types.MemoryStatusActive:     3, // alice 1 + bob 2
		types.MemoryStatusPending:    1,
		types.MemoryStatusArchived:   1,
		types.MemoryStatusSuperseded: 1,
	}, byStatus)

	// 租户 2 的 5 条与租户 1 互不干扰。
	other, err := repo.CountTenantFactsByStatus(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{types.MemoryStatusActive: 5}, other)

	// 空工作区返回空 map 而不是 nil，调用方可以直接取值。
	empty, err := repo.CountTenantFactsByStatus(ctx, 99)
	require.NoError(t, err)
	require.Empty(t, empty)
	require.NotNil(t, empty)
}

// TestCountTenantFactsByCategory 覆盖"按类型分布"，并确认工作区级与用户级两个
// 口径不会互相污染。
func TestCountTenantFactsByCategory(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()
	seedAdminWorkspace(t, ctx, repo)

	byCategory, err := repo.CountTenantFactsByCategory(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{
		types.MemoryCategoryProfile:    1,
		types.MemoryCategoryFact:       3, // alice pending 1 + bob active 2
		types.MemoryCategoryPreference: 1,
		types.MemoryCategoryTodo:       1,
	}, byCategory)

	// 用户级口径（全状态）：alice 三类各一条。
	alice, err := repo.CountUserFactsByCategory(ctx, 1, "alice")
	require.NoError(t, err)
	require.Equal(t, map[string]int64{
		types.MemoryCategoryProfile:    1,
		types.MemoryCategoryFact:       1,
		types.MemoryCategoryPreference: 1,
	}, alice)

	// 同租户其他成员的类型分布不受影响。
	bob, err := repo.CountUserFactsByCategory(ctx, 1, "bob")
	require.NoError(t, err)
	require.Equal(t, map[string]int64{types.MemoryCategoryFact: 2}, bob)
}

// TestListTenantMemoryUsers 覆盖 FR-09 规则 2 的用户维度分页：规模倒序、
// 同规模按 user_id 稳定排序、总数与翻页不重不漏。
func TestListTenantMemoryUsers(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()
	seedAdminWorkspace(t, ctx, repo)

	total, err := repo.CountTenantMemoryUsers(ctx, 1)
	require.NoError(t, err)
	require.EqualValues(t, 3, total, "dave 没有记忆，不应计入成员数")

	cases := []struct {
		name        string
		limit       int
		offset      int
		wantUsers   []string
		wantTotal   int64
		wantItems   []int64
		wantLastAct bool
	}{
		{
			name: "首页按规模倒序", limit: 2, offset: 0,
			wantUsers: []string{"alice", "bob"}, wantTotal: 3, wantItems: []int64{3, 2}, wantLastAct: true,
		},
		{
			name: "翻页取到剩余成员", limit: 2, offset: 2,
			wantUsers: []string{"carol"}, wantTotal: 3, wantItems: []int64{1}, wantLastAct: true,
		},
		{
			name: "偏移越过末尾返回空页但总数不变", limit: 50, offset: 100,
			wantUsers: nil, wantTotal: 3, wantItems: nil,
		},
		{
			name: "limit 非正数回落默认页长", limit: 0, offset: 0,
			wantUsers: []string{"alice", "bob", "carol"}, wantTotal: 3, wantItems: []int64{3, 2, 1}, wantLastAct: true,
		},
		{
			name: "负偏移归零", limit: 1, offset: -5,
			wantUsers: []string{"alice"}, wantTotal: 3, wantItems: []int64{3}, wantLastAct: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			summaries, total, err := repo.ListTenantMemoryUsers(ctx, 1, tc.limit, tc.offset)
			require.NoError(t, err)
			require.EqualValues(t, tc.wantTotal, total)
			require.Len(t, summaries, len(tc.wantUsers))
			for i, want := range tc.wantUsers {
				require.Equal(t, want, summaries[i].UserID)
				require.EqualValues(t, tc.wantItems[i], summaries[i].ItemCount)
				if tc.wantLastAct {
					require.NotNil(t, summaries[i].LastActivityAt, "最近活动时间用于识别异常增长")
				}
			}
		})
	}

	// 状态计数与总量自洽：alice 三状态各一条。
	first, _, err := repo.ListTenantMemoryUsers(ctx, 1, 1, 0)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.EqualValues(t, 1, first[0].ActiveCount)
	require.EqualValues(t, 1, first[0].PendingCount)
	require.EqualValues(t, 1, first[0].ArchivedCount)
	require.EqualValues(t, 0, first[0].SupersededCount)
	require.EqualValues(t, 3, first[0].ItemCount)

	// 空工作区返回空切片 + 0，而不是 nil / 错误。
	empty, emptyTotal, err := repo.ListTenantMemoryUsers(ctx, 99, 50, 0)
	require.NoError(t, err)
	require.EqualValues(t, 0, emptyTotal)
	require.Empty(t, empty)
	require.NotNil(t, empty)
}

// TestListTenantMemoryUsersNeverCrossesTenant 是 FR-09 规则 4 的正向断言：
// 租户 2 的 alice 有 5 条记忆，绝不能出现在租户 1 的用户维度列表里。
func TestListTenantMemoryUsersNeverCrossesTenant(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()
	seedAdminWorkspace(t, ctx, repo)

	summaries, total, err := repo.ListTenantMemoryUsers(ctx, 1, 200, 0)
	require.NoError(t, err)
	require.EqualValues(t, 3, total)
	for _, summary := range summaries {
		require.LessOrEqual(t, summary.ItemCount, int64(3),
			"租户 1 的聚合不得混入租户 2 的条目")
		if summary.UserID == "alice" {
			require.EqualValues(t, 3, summary.ItemCount, "租户 2 的 5 条不得计入")
		}
	}

	otherTotal, err := repo.CountTenantMemoryUsers(ctx, 2)
	require.NoError(t, err)
	require.EqualValues(t, 1, otherTotal)
}

// TestGetTenantMemoryUser 覆盖单用户明细，以及"不存在"与"外部工作区"共用同一个
// (nil, nil) 结果——handler 据此对两者返回同一个 404，不泄露工作区外成员是否存在。
func TestGetTenantMemoryUser(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()
	seedAdminWorkspace(t, ctx, repo)

	cases := []struct {
		name     string
		tenantID uint64
		userID   string
		wantNil  bool
		wantItem int64
	}{
		{name: "本工作区成员", tenantID: 1, userID: "alice", wantItem: 3},
		{name: "本工作区单条成员", tenantID: 1, userID: "carol", wantItem: 1},
		{name: "本工作区无记忆成员", tenantID: 1, userID: "dave", wantNil: true},
		{name: "从未出现过的成员", tenantID: 1, userID: "nobody", wantNil: true},
		{name: "外部工作区的同名成员", tenantID: 1, userID: "tenant2-alice", wantNil: true},
		{name: "空 user_id", tenantID: 1, userID: "", wantNil: true},
		{name: "空白 user_id", tenantID: 1, userID: "   ", wantNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			summary, err := repo.GetTenantMemoryUser(ctx, tc.tenantID, tc.userID)
			require.NoError(t, err)
			if tc.wantNil {
				require.Nil(t, summary, "不存在与外部工作区必须返回同一个空结果")
				return
			}
			require.NotNil(t, summary)
			require.Equal(t, tc.userID, summary.UserID)
			require.EqualValues(t, tc.wantItem, summary.ItemCount)
			require.NotNil(t, summary.LastActivityAt)
		})
	}

	// 租户 2 的 alice 在自己的工作区里看得到全部 5 条。
	other, err := repo.GetTenantMemoryUser(ctx, 2, "alice")
	require.NoError(t, err)
	require.NotNil(t, other)
	require.EqualValues(t, 5, other.ItemCount)
	require.EqualValues(t, 5, other.ActiveCount)
}

// TestListFactIDsByStatus 覆盖批量删除待确认项的作用范围收口：
// 只返回目标状态的 ID，混入的 active 条目一律被过滤掉（FR-02 规则 6）。
func TestListFactIDsByStatus(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()
	facts := seedAdminWorkspace(t, ctx, repo)

	cases := []struct {
		name   string
		userID string
		status string
		ids    []string
		limit  int
		want   []string
	}{
		{
			name: "全部待确认项", userID: "alice", status: types.MemoryStatusPending,
			want: []string{facts["alice-pending"].ID},
		},
		{
			name: "指定 ID 时收敛到交集", userID: "alice", status: types.MemoryStatusPending,
			ids:  []string{facts["alice-active"].ID, facts["alice-pending"].ID, facts["alice-archived"].ID},
			want: []string{facts["alice-pending"].ID},
		},
		{
			name: "全部是 active 时返回空", userID: "alice", status: types.MemoryStatusPending,
			ids:  []string{facts["alice-active"].ID},
			want: nil,
		},
		{
			name: "bob 没有待确认项", userID: "bob", status: types.MemoryStatusPending,
			want: nil,
		},
		{name: "空 user_id 直接返回空", userID: "", status: types.MemoryStatusPending, want: nil},
		{name: "空状态直接返回空", userID: "alice", status: "", want: nil},
		{name: "未知 ID 不匹配", userID: "alice", status: types.MemoryStatusPending,
			ids: []string{"no-such-id"}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.ListFactIDsByStatus(ctx, 1, tc.userID, tc.status, tc.ids, tc.limit)
			require.NoError(t, err)
			if len(tc.want) == 0 {
				require.Empty(t, got)
				return
			}
			require.Equal(t, tc.want, got)
		})
	}

	// limit 生效：bob 有两条 active，limit=1 只返回一条。
	// 不断言具体是哪一条——两条的 created_at 可能落在同一时钟刻度，
	// 此时次序由随机 UUID 决定，断言具体 ID 会造成偶发失败。
	limited, err := repo.ListFactIDsByStatus(ctx, 1, "bob", types.MemoryStatusActive, nil, 1)
	require.NoError(t, err)
	require.Len(t, limited, 1)
	require.Contains(t, []string{facts["bob-1"].ID, facts["bob-2"].ID}, limited[0])

	// 关键不变量：以 pending 为条件时，任何 active 条目的 ID 都不可能出现在结果里，
	// 即使调用方明确把它列进 ids。
	got, err := repo.ListFactIDsByStatus(ctx, 1, "alice", types.MemoryStatusPending,
		[]string{facts["alice-active"].ID, facts["bob-1"].ID, facts["alice-pending"].ID}, 0)
	require.NoError(t, err)
	require.Equal(t, []string{facts["alice-pending"].ID}, got)
	require.NotContains(t, got, facts["alice-active"].ID, "active 记忆绝不能被批量删除待确认项命中")

	// limit 超上限时收敛到 MemoryAdminBatchLimit，不报错。
	clamped, err := repo.ListFactIDsByStatus(ctx, 1, "alice", types.MemoryStatusPending, nil, 100000)
	require.NoError(t, err)
	require.Len(t, clamped, 1)
}

// TestListFactIDsByStatusIgnoresSoftDeleted 确认已软删除的行不参与批量删除的
// 候选集：与用户侧列表口径一致，也不会让 Affected 计数虚高。
func TestListFactIDsByStatusIgnoresSoftDeleted(t *testing.T) {
	repo, _ := newMemoryRepositoryForTest(t)
	ctx := context.Background()
	facts := seedAdminWorkspace(t, ctx, repo)

	deleted, err := repo.DeleteFact(ctx, 1, "alice", facts["alice-pending"].ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)

	got, err := repo.ListFactIDsByStatus(ctx, 1, "alice", types.MemoryStatusPending, nil, 0)
	require.NoError(t, err)
	require.Empty(t, got)

	summary, err := repo.GetTenantMemoryUser(ctx, 1, "alice")
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.EqualValues(t, 2, summary.ItemCount, "软删除行不计入规模")
}
