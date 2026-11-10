package service

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

// ---------------------------------------------------------------------------
// FR-09 记忆管理控制台（服务层）
// ---------------------------------------------------------------------------

// memoryAdminTestCtx 在既有测试上下文（租户 1 / 用户 alice）之上叠加工作区角色。
func memoryAdminTestCtx(role types.TenantRole) context.Context {
	return context.WithValue(memoryTestCtx(), types.TenantRoleContextKey, role)
}

// seedAdminFactFor 直接落一条指定 (租户, 成员, 类型, 状态) 的记忆，绕开服务层门控。
// tag 参与三元组，避免撞上 (租户, 用户, triple_hash) 唯一索引被去重成一行。
func seedAdminFactFor(t *testing.T, svc *memoryService,
	tenantID uint64, userID, category, status, tag string) *types.MemoryFact {
	t.Helper()
	fact := &types.MemoryFact{
		TenantID: tenantID, UserID: userID, SessionID: "s-" + tag,
		Category: category, Subject: "用户", Predicate: "标记", Object: tag,
		Content: userID + " " + tag, Status: status, Origin: types.MemoryOriginExtracted,
		Confidence: 0.8, Importance: 0.5,
		NormalizedKey: types.ComputeNormalizedKey(category, "用户", "标记"),
	}
	require.NoError(t, svc.memoryRepo.CreateFact(context.Background(), fact))
	return fact
}

// seedAdminServiceWorkspace 铺一份跨两个工作区的固定数据：
//
//	租户 1  alice 3 条（active profile / pending fact / archived preference）
//	        bob   2 条（active fact / pending fact）
//	        carol 1 条（superseded todo）
//	租户 2  zoe   5 条（active fact）——绝不能出现在租户 1 的任何聚合里
func seedAdminServiceWorkspace(t *testing.T, svc *memoryService) map[string]*types.MemoryFact {
	t.Helper()
	facts := map[string]*types.MemoryFact{}
	facts["alice-active"] = seedAdminFactFor(t, svc, 1, "alice", types.MemoryCategoryProfile, types.MemoryStatusActive, "a1")
	facts["alice-pending"] = seedAdminFactFor(t, svc, 1, "alice", types.MemoryCategoryFact, types.MemoryStatusPending, "a2")
	facts["alice-archived"] = seedAdminFactFor(t, svc, 1, "alice", types.MemoryCategoryPreference, types.MemoryStatusArchived, "a3")
	facts["bob-active"] = seedAdminFactFor(t, svc, 1, "bob", types.MemoryCategoryFact, types.MemoryStatusActive, "b1")
	facts["bob-pending"] = seedAdminFactFor(t, svc, 1, "bob", types.MemoryCategoryFact, types.MemoryStatusPending, "b2")
	facts["carol-superseded"] = seedAdminFactFor(t, svc, 1, "carol", types.MemoryCategoryTodo, types.MemoryStatusSuperseded, "c1")
	for _, tag := range []string{"z1", "z2", "z3", "z4", "z5"} {
		seedAdminFactFor(t, svc, 2, "zoe", types.MemoryCategoryFact, types.MemoryStatusActive, tag)
	}
	return facts
}

// TestMemoryAdminCallerGuards 覆盖 FR-09 的权限口径：仅工作区管理员，
// 且拒绝 API Key 主体（RequireRole 对机器主体短路，控制台没有"机器管理员"概念）。
func TestMemoryAdminCallerGuards(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	seedAdminServiceWorkspace(t, svc)

	apiKeyCtx := types.WithTenantAPIKeyScope(
		memoryAdminTestCtx(types.TenantRoleOwner),
		types.TenantAPIKeyScope{KeyID: 7, FullAccess: true})

	cases := []struct {
		name    string
		ctx     context.Context
		wantErr error
	}{
		{name: "Owner 放行", ctx: memoryAdminTestCtx(types.TenantRoleOwner)},
		{name: "Admin 放行", ctx: memoryAdminTestCtx(types.TenantRoleAdmin)},
		{name: "Contributor 拒绝", ctx: memoryAdminTestCtx(types.TenantRoleContributor), wantErr: types.ErrMemoryForbidden},
		{name: "Viewer 拒绝", ctx: memoryAdminTestCtx(types.TenantRoleViewer), wantErr: types.ErrMemoryForbidden},
		{name: "无角色（缺省 Viewer）拒绝", ctx: memoryTestCtx(), wantErr: types.ErrMemoryForbidden},
		{name: "API Key 主体即使是 Owner 也拒绝", ctx: apiKeyCtx, wantErr: types.ErrMemoryForbidden},
		{name: "缺少工作区上下文拒绝", ctx: context.Background(), wantErr: errMemoryScopeUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			overview, err := svc.AdminMemoryOverview(tc.ctx)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.Nil(t, overview)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, overview)
		})
	}
}

// TestAdminMemoryOverviewAggregatesWorkspace 覆盖 FR-09 规则 1：
// 工作区总量 + 按状态分布 + 按类型分布 + 成员数，且严格限定在本工作区。
func TestAdminMemoryOverviewAggregatesWorkspace(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	seedAdminServiceWorkspace(t, svc)

	overview, err := svc.AdminMemoryOverview(memoryAdminTestCtx(types.TenantRoleAdmin))
	require.NoError(t, err)
	require.EqualValues(t, 1, overview.TenantID)
	require.EqualValues(t, 6, overview.Total, "租户 2 的 5 条不得计入")
	require.Equal(t, map[string]int64{
		types.MemoryStatusActive:     2,
		types.MemoryStatusPending:    2,
		types.MemoryStatusArchived:   1,
		types.MemoryStatusSuperseded: 1,
	}, overview.ByStatus)
	require.Equal(t, map[string]int64{
		types.MemoryCategoryProfile:    1,
		types.MemoryCategoryFact:       3,
		types.MemoryCategoryPreference: 1,
		types.MemoryCategoryTodo:       1,
	}, overview.ByCategory)
	require.EqualValues(t, 3, overview.UserCount)
	require.False(t, overview.GeneratedAt.IsZero())

	// 总量与按状态分布自洽：控制台两个数字不能互相矛盾。
	var sum int64
	for _, count := range overview.ByStatus {
		sum += count
	}
	require.EqualValues(t, overview.Total, sum)

	// 未配置工作区级记忆上限时回落全局默认（PRD §5.4：存量工作区无需迁移）。
	require.Equal(t, types.MemoryWriteAuto, overview.WriteMode)
	require.Equal(t, types.DefaultMemoryMaxActiveItems, overview.MaxActiveItems)
	require.Equal(t, types.DefaultMemoryMaxPendingItems, overview.MaxPendingItems)

	// 配置了上限就按配置回报，管理员据此判断某成员是否已贴着上限。
	withTenantMemoryConfig(svc, 1, &types.TenantMemoryConfig{
		WriteMode: types.MemoryWriteExplicitOnly, MaxActiveItems: 30, MaxPendingItems: 10,
	})
	configured, err := svc.AdminMemoryOverview(memoryAdminTestCtx(types.TenantRoleAdmin))
	require.NoError(t, err)
	require.Equal(t, types.MemoryWriteExplicitOnly, configured.WriteMode)
	require.Equal(t, 30, configured.MaxActiveItems)
	require.Equal(t, 10, configured.MaxPendingItems)
}

// TestAdminListMemoryUsersPaging 覆盖 FR-09 规则 2 的用户维度视图与分页钳制。
func TestAdminListMemoryUsersPaging(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	seedAdminServiceWorkspace(t, svc)
	ctx := memoryAdminTestCtx(types.TenantRoleAdmin)

	summaries, total, err := svc.AdminListMemoryUsers(ctx, 0, 0)
	require.NoError(t, err)
	require.EqualValues(t, 3, total)
	require.Len(t, summaries, 3, "limit 非正数回落默认页长 50")
	// 规模倒序：异常增长的成员浮到第一页。
	require.Equal(t, "alice", summaries[0].UserID)
	require.EqualValues(t, 3, summaries[0].ItemCount)
	require.EqualValues(t, 1, summaries[0].ActiveCount)
	require.EqualValues(t, 1, summaries[0].PendingCount)
	require.EqualValues(t, 1, summaries[0].ArchivedCount)

	paged, pagedTotal, err := svc.AdminListMemoryUsers(ctx, 2, 2)
	require.NoError(t, err)
	require.EqualValues(t, 3, pagedTotal)
	require.Len(t, paged, 1)
	require.Equal(t, "carol", paged[0].UserID)

	// 页长超上限收敛到 200，负偏移归零。
	clampedLimit, clampedOffset := types.MemoryAdminPage(100000, -3)
	require.Equal(t, types.MemoryAdminMaxPageSize, clampedLimit)
	require.Zero(t, clampedOffset)

	// 非管理员看不到用户维度视图。
	_, _, err = svc.AdminListMemoryUsers(memoryAdminTestCtx(types.TenantRoleViewer), 50, 0)
	require.ErrorIs(t, err, types.ErrMemoryForbidden)
}

// TestAdminGetMemoryUser 覆盖单成员明细，以及"不存在"与"外部工作区"共用同一个
// 错误（handler 据此对两者返回同一个 404，防止跨工作区探测）。
func TestAdminGetMemoryUser(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	seedAdminServiceWorkspace(t, svc)
	ctx := memoryAdminTestCtx(types.TenantRoleAdmin)

	detail, err := svc.AdminGetMemoryUser(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, "alice", detail.UserID)
	require.EqualValues(t, 3, detail.ItemCount)
	require.Equal(t, map[string]int64{
		types.MemoryCategoryProfile:    1,
		types.MemoryCategoryFact:       1,
		types.MemoryCategoryPreference: 1,
	}, detail.ByCategory)
	require.NotNil(t, detail.LastActivityAt)

	cases := []struct {
		name    string
		userID  string
		wantErr error
	}{
		{name: "本工作区无记忆成员", userID: "dave", wantErr: types.ErrMemoryAdminTargetNotFound},
		{name: "从未出现过的成员", userID: "nobody", wantErr: types.ErrMemoryAdminTargetNotFound},
		// zoe 在租户 2 有 5 条记忆，但租户 1 的管理员看到的必须是"查无此人"，
		// 与真正不存在的用户不可区分（FR-09 规则 4）。
		{name: "外部工作区成员", userID: "zoe", wantErr: types.ErrMemoryAdminTargetNotFound},
		{name: "空 user_id", userID: "", wantErr: types.ErrMemoryAdminBadRequest},
		{name: "空白 user_id", userID: "   ", wantErr: types.ErrMemoryAdminBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.AdminGetMemoryUser(ctx, tc.userID)
			require.ErrorIs(t, err, tc.wantErr)
			require.Nil(t, got)
		})
	}

	// 非管理员拿不到成员明细。
	_, err = svc.AdminGetMemoryUser(memoryAdminTestCtx(types.TenantRoleContributor), "alice")
	require.ErrorIs(t, err, types.ErrMemoryForbidden)
}

// TestAdminGetMemoryUserSurfacesGovernance 验证明细并入 FR-01/FR-04 的治理状态：
// 归档连续失败达阈值后，管理员能在控制台直接看到自动抽取已暂停。
func TestAdminGetMemoryUserSurfacesGovernance(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	seedAdminServiceWorkspace(t, svc)
	ctx := memoryAdminTestCtx(types.TenantRoleAdmin)

	before, err := svc.AdminGetMemoryUser(ctx, "alice")
	require.NoError(t, err)
	require.False(t, before.ExtractionPaused)
	require.Zero(t, before.BlockedWrites)

	for i := 0; i < types.MemoryArchiveFailureThreshold; i++ {
		_, err := svc.memoryRepo.RecordArchiveFailure(ctx, 1, "alice")
		require.NoError(t, err)
	}
	after, err := svc.AdminGetMemoryUser(ctx, "alice")
	require.NoError(t, err)
	require.True(t, after.ExtractionPaused, "达到失败阈值后控制台必须显示抽取已暂停")
}

// TestAdminBatchArchiveFacts 覆盖 FR-09 规则 3 的批量归档：可逆、影响条目数准确。
func TestAdminBatchArchiveFacts(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	facts := seedAdminServiceWorkspace(t, svc)
	ctx := memoryAdminTestCtx(types.TenantRoleAdmin)

	result, err := svc.AdminBatchArchiveFacts(ctx, "bob",
		[]string{facts["bob-active"].ID, facts["bob-pending"].ID, "no-such-id"})
	require.NoError(t, err)
	require.Equal(t, types.MemoryBatchActionArchive, result.Action)
	require.Equal(t, "bob", result.UserID)
	require.Equal(t, 3, result.Requested)
	require.EqualValues(t, 2, result.Affected, "不存在的 ID 不计入影响条目数")

	for _, key := range []string{"bob-active", "bob-pending"} {
		got, err := svc.memoryRepo.GetFactByID(ctx, 1, "bob", facts[key].ID)
		require.NoError(t, err)
		require.Equal(t, types.MemoryStatusArchived, got.Status, "归档是可逆的状态转移，不删原文")
	}

	// ids 必填：控制台不接受"什么都不指定"的归档请求。
	_, err = svc.AdminBatchArchiveFacts(ctx, "bob", nil)
	require.ErrorIs(t, err, types.ErrMemoryAdminBadRequest)
	_, err = svc.AdminBatchArchiveFacts(ctx, "bob", []string{"  ", ""})
	require.ErrorIs(t, err, types.ErrMemoryAdminBadRequest)

	// 空 user_id 同样拒绝。
	_, err = svc.AdminBatchArchiveFacts(ctx, "", []string{facts["bob-active"].ID})
	require.ErrorIs(t, err, types.ErrMemoryAdminBadRequest)

	// 非管理员不能批量归档。
	_, err = svc.AdminBatchArchiveFacts(memoryAdminTestCtx(types.TenantRoleViewer), "bob",
		[]string{facts["bob-active"].ID})
	require.ErrorIs(t, err, types.ErrMemoryForbidden)
}

// TestAdminBatchOperationsRespectLimit 覆盖单次批量上限。
func TestAdminBatchOperationsRespectLimit(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	seedAdminServiceWorkspace(t, svc)
	ctx := memoryAdminTestCtx(types.TenantRoleAdmin)

	ids := make([]string, types.MemoryAdminBatchLimit+1)
	for i := range ids {
		ids[i] = "id-" + strings.Repeat("x", i%5+1) + string(rune('a'+i%26))
	}
	_, err := svc.AdminBatchArchiveFacts(ctx, "bob", ids)
	require.ErrorIs(t, err, types.ErrMemoryAdminBadRequest)

	_, err = svc.AdminBatchDeletePendingFacts(ctx, "bob", ids)
	require.ErrorIs(t, err, types.ErrMemoryAdminBadRequest)
}

// TestAdminBatchDeletePendingFactsNeverTouchesActive 是 FR-09 规则 3 与
// FR-02 规则 6 的核心不变量：批量删除待确认项只作用于 pending，
// 即使调用方把 active / archived / superseded 的 ID 混进请求也不会被删。
func TestAdminBatchDeletePendingFactsNeverTouchesActive(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	facts := seedAdminServiceWorkspace(t, svc)
	ctx := memoryAdminTestCtx(types.TenantRoleAdmin)

	mixed := []string{
		facts["alice-pending"].ID,    // 唯一应被删除的
		facts["alice-active"].ID,     // active：必须留下
		facts["alice-archived"].ID,   // archived：必须留下
		facts["carol-superseded"].ID, // 别人的记忆：跨用户不生效
		"no-such-id",
	}
	result, err := svc.AdminBatchDeletePendingFacts(ctx, "alice", mixed)
	require.NoError(t, err)
	require.Equal(t, types.MemoryBatchActionDelete, result.Action)
	require.Equal(t, "alice", result.UserID)
	require.Equal(t, len(mixed), result.Requested)
	require.EqualValues(t, 1, result.Affected, "只删掉了那一条 pending")
	require.ElementsMatch(t, []string{
		facts["alice-active"].ID,
		facts["alice-archived"].ID,
		facts["carol-superseded"].ID,
		"no-such-id",
	}, result.Skipped, "被拒绝的条目必须回报，不能静默忽略")

	// 被拒绝的条目确实还在，且状态没变。
	stillActive, err := svc.memoryRepo.GetFactByID(ctx, 1, "alice", facts["alice-active"].ID)
	require.NoError(t, err)
	require.NotNil(t, stillActive)
	require.Equal(t, types.MemoryStatusActive, stillActive.Status)

	stillArchived, err := svc.memoryRepo.GetFactByID(ctx, 1, "alice", facts["alice-archived"].ID)
	require.NoError(t, err)
	require.NotNil(t, stillArchived)
	require.Equal(t, types.MemoryStatusArchived, stillArchived.Status)

	// 目标那条 pending 已被物理移除（不可逆）。
	gone, err := svc.memoryRepo.GetFactByID(ctx, 1, "alice", facts["alice-pending"].ID)
	require.NoError(t, err)
	require.Nil(t, gone)

	// carol 的 superseded 记忆不受 alice 的批量操作影响。
	carolFact, err := svc.memoryRepo.GetFactByID(ctx, 1, "carol", facts["carol-superseded"].ID)
	require.NoError(t, err)
	require.NotNil(t, carolFact)
	require.Equal(t, types.MemoryStatusSuperseded, carolFact.Status)
}

// TestAdminBatchDeletePendingFactsWithoutIDs 覆盖 ids 为空的语义：
// 清空该成员的全部待确认项，且只影响该成员。
func TestAdminBatchDeletePendingFactsWithoutIDs(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	facts := seedAdminServiceWorkspace(t, svc)
	// bob 再补一条 pending，验证"全部待确认项"确实按成员收敛。
	extra := seedAdminFactFor(t, svc, 1, "bob", types.MemoryCategoryFeedback, types.MemoryStatusPending, "b3")
	ctx := memoryAdminTestCtx(types.TenantRoleAdmin)

	result, err := svc.AdminBatchDeletePendingFacts(ctx, "bob", nil)
	require.NoError(t, err)
	require.EqualValues(t, 2, result.Affected)
	require.Equal(t, 2, result.Requested, "不指定 ID 时请求范围等于解析出的待确认项数")
	require.Empty(t, result.Skipped)

	gone, err := svc.memoryRepo.GetFactByID(ctx, 1, "bob", facts["bob-pending"].ID)
	require.NoError(t, err)
	require.Nil(t, gone)
	gone, err = svc.memoryRepo.GetFactByID(ctx, 1, "bob", extra.ID)
	require.NoError(t, err)
	require.Nil(t, gone)

	// bob 的 active 记忆毫发无损。
	still, err := svc.memoryRepo.GetFactByID(ctx, 1, "bob", facts["bob-active"].ID)
	require.NoError(t, err)
	require.NotNil(t, still)
	require.Equal(t, types.MemoryStatusActive, still.Status)

	// alice 的待确认项没被牵连。
	alicePending, err := svc.memoryRepo.GetFactByID(ctx, 1, "alice", facts["alice-pending"].ID)
	require.NoError(t, err)
	require.NotNil(t, alicePending)

	// 没有待确认项的成员：0 影响，不报错（PRD §5.5 空结果场景）。
	empty, err := svc.AdminBatchDeletePendingFacts(ctx, "carol", nil)
	require.NoError(t, err)
	require.EqualValues(t, 0, empty.Affected)

	// 非管理员与空 user_id 一律拒绝。
	_, err = svc.AdminBatchDeletePendingFacts(memoryAdminTestCtx(types.TenantRoleContributor), "bob", nil)
	require.ErrorIs(t, err, types.ErrMemoryForbidden)
	_, err = svc.AdminBatchDeletePendingFacts(ctx, "", nil)
	require.ErrorIs(t, err, types.ErrMemoryAdminBadRequest)
}

// TestAdminConsoleNeverLeaksOtherWorkspace 是 FR-09 规则 4 的端到端断言：
// 租户 1 的管理员无论走哪个入口，都拿不到租户 2 的任何计数。
func TestAdminConsoleNeverLeaksOtherWorkspace(t *testing.T) {
	svc, _, _, _, _, _ := newMemoryServiceForTest(t)
	seedAdminServiceWorkspace(t, svc)
	ctx := memoryAdminTestCtx(types.TenantRoleAdmin)

	overview, err := svc.AdminMemoryOverview(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 6, overview.Total)
	require.EqualValues(t, 3, overview.UserCount)

	summaries, total, err := svc.AdminListMemoryUsers(ctx, 200, 0)
	require.NoError(t, err)
	require.EqualValues(t, 3, total)
	for _, summary := range summaries {
		require.NotEqual(t, "zoe", summary.UserID, "外部工作区成员不得出现在列表里")
		require.LessOrEqual(t, summary.ItemCount, int64(3),
			"任何成员的规模都不得超过租户 1 内的实际条数")
	}

	// 租户 2 的 zoe 在租户 1 的控制台里等同"查无此人"。
	_, err = svc.AdminGetMemoryUser(ctx, "zoe")
	require.ErrorIs(t, err, types.ErrMemoryAdminTargetNotFound)

	// 换成租户 2 的管理员上下文，看到的才是那 5 条。
	tenant2Ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(2))
	tenant2Ctx = context.WithValue(tenant2Ctx, types.UserIDContextKey, "root")
	tenant2Ctx = context.WithValue(tenant2Ctx, types.TenantRoleContextKey, types.TenantRoleOwner)
	other, err := svc.AdminMemoryOverview(tenant2Ctx)
	require.NoError(t, err)
	require.EqualValues(t, 5, other.Total)
	require.EqualValues(t, 1, other.UserCount)

	otherDetail, err := svc.AdminGetMemoryUser(tenant2Ctx, "zoe")
	require.NoError(t, err)
	require.EqualValues(t, 5, otherDetail.ItemCount)
}
