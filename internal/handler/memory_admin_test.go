package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// ---------------------------------------------------------------------------
// FR-09 记忆管理控制台（handler 层）
// ---------------------------------------------------------------------------

// stubMemoryAdminService 只实现控制台用到的五个方法，其余走嵌入接口的 nil 实现。
type stubMemoryAdminService struct {
	interfaces.MemoryService

	overview    *types.MemoryAdminOverview
	summaries   []*types.MemoryUserSummary
	total       int64
	detail      *types.MemoryAdminUserDetail
	batchResult *types.MemoryAdminBatchResponse

	// 服务层错误，用于驱动错误映射。
	overviewErr error
	listErr     error
	detailErr   error
	batchErr    error

	// 调用记录。
	gotLimit, gotOffset int
	gotUserID           string
	gotIDs              []string
	archiveCalls        int
	deletePendingCalls  int
}

func (s *stubMemoryAdminService) AdminMemoryOverview(ctx context.Context) (*types.MemoryAdminOverview, error) {
	if s.overviewErr != nil {
		return nil, s.overviewErr
	}
	return s.overview, nil
}

func (s *stubMemoryAdminService) AdminListMemoryUsers(
	ctx context.Context, limit, offset int,
) ([]*types.MemoryUserSummary, int64, error) {
	s.gotLimit, s.gotOffset = limit, offset
	if s.listErr != nil {
		return nil, 0, s.listErr
	}
	return s.summaries, s.total, nil
}

func (s *stubMemoryAdminService) AdminGetMemoryUser(
	ctx context.Context, userID string,
) (*types.MemoryAdminUserDetail, error) {
	s.gotUserID = userID
	if s.detailErr != nil {
		return nil, s.detailErr
	}
	return s.detail, nil
}

func (s *stubMemoryAdminService) AdminBatchArchiveFacts(
	ctx context.Context, userID string, ids []string,
) (*types.MemoryAdminBatchResponse, error) {
	s.archiveCalls++
	s.gotUserID, s.gotIDs = userID, ids
	if s.batchErr != nil {
		return nil, s.batchErr
	}
	return s.batchResult, nil
}

func (s *stubMemoryAdminService) AdminBatchDeletePendingFacts(
	ctx context.Context, userID string, ids []string,
) (*types.MemoryAdminBatchResponse, error) {
	s.deletePendingCalls++
	s.gotUserID, s.gotIDs = userID, ids
	if s.batchErr != nil {
		return nil, s.batchErr
	}
	return s.batchResult, nil
}

// memoryAdminAuditFake 记录写进来的审计条目；fail=true 时模拟审计侧故障。
type memoryAdminAuditFake struct {
	interfaces.AuditLogService
	entries []*types.AuditLog
	fail    bool
}

func (f *memoryAdminAuditFake) Log(ctx context.Context, entry *types.AuditLog) error {
	if f.fail {
		return fmt.Errorf("audit backend unavailable")
	}
	f.entries = append(f.entries, entry)
	return nil
}

// newMemoryAdminTestRouter 挂载控制台路由，注入 (租户, 用户, 角色) 上下文与审计服务。
// 角色门本身由 router 包的 g.Admin() 负责，这里只测 handler 的行为。
func newMemoryAdminTestRouter(
	svc interfaces.MemoryService, audit interfaces.AuditLogService,
) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())
	r.Use(middleware.AuditServiceProvider(audit))
	r.Use(func(c *gin.Context) {
		ctx := c.Request.Context()
		ctx = context.WithValue(ctx, types.TenantIDContextKey, uint64(1))
		ctx = context.WithValue(ctx, types.UserIDContextKey, "admin-user")
		ctx = context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleAdmin)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	h := NewMemoryHandler(svc)
	admin := r.Group("/memory/admin")
	{
		admin.GET("/overview", h.AdminMemoryOverview)
		admin.GET("/users", h.AdminListMemoryUsers)
		admin.GET("/users/:user_id", h.AdminGetMemoryUser)
		admin.POST("/facts/batch-archive", h.AdminBatchArchiveMemoryFacts)
		admin.POST("/facts/batch-delete-pending", h.AdminBatchDeletePendingMemoryFacts)
	}
	return r
}

// decodeAdminBody 解出 {"success":..,"data":{...}} 里的 data。
func decodeAdminBody(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, true, body["success"])
	data, ok := body["data"].(map[string]interface{})
	require.True(t, ok, "data 必须是对象: %s", w.Body.String())
	return data
}

func TestAdminMemoryOverviewReturnsAggregates(t *testing.T) {
	svc := &stubMemoryAdminService{overview: &types.MemoryAdminOverview{
		TenantID: 1, Total: 6, UserCount: 3,
		ByStatus:       map[string]int64{"active": 2, "pending": 2, "archived": 1, "superseded": 1},
		ByCategory:     map[string]int64{"fact": 3, "profile": 1},
		WriteMode:      types.MemoryWriteAuto,
		MaxActiveItems: 200, MaxPendingItems: 50,
	}}
	audit := &memoryAdminAuditFake{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/memory/admin/overview", nil)
	newMemoryAdminTestRouter(svc, audit).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	data := decodeAdminBody(t, w)
	require.EqualValues(t, 6, data["total"])
	require.EqualValues(t, 3, data["user_count"])
	byStatus := data["by_status"].(map[string]interface{})
	require.EqualValues(t, 2, byStatus["active"])
	require.EqualValues(t, 2, byStatus["pending"])
	require.EqualValues(t, 1, byStatus["superseded"])
	require.EqualValues(t, 1, byStatus["archived"])
	byCategory := data["by_category"].(map[string]interface{})
	require.EqualValues(t, 3, byCategory["fact"])

	// FR-09 规则 5 / PRD §5.2：读操作也留痕，且记录操作者、操作类型、影响范围。
	require.Len(t, audit.entries, 1)
	entry := audit.entries[0]
	require.Equal(t, types.AuditActionMemoryAdminOverviewViewed, entry.Action)
	require.EqualValues(t, 1, entry.TenantID)
	require.Equal(t, "admin-user", entry.ActorUserID)
	require.Equal(t, string(types.TenantRoleAdmin), entry.ActorRole)
	require.Equal(t, types.AuditOutcomeSuccess, entry.Outcome)
	// ScopeType 必须留空：租户审计 feed 用 UnscopedOnly 过滤，带 scope 的行不可见。
	require.Empty(t, entry.ScopeType)
	require.Empty(t, entry.ScopeID)
	require.Equal(t, "memory_workspace", entry.TargetType)
	require.Equal(t, "/memory/admin/overview", entry.RequestPath)
	var details map[string]interface{}
	require.NoError(t, json.Unmarshal(entry.Details, &details))
	require.EqualValues(t, 6, details["total"])
	require.EqualValues(t, 3, details["user_count"])
	// 审计明细绝不能夹带记忆正文。
	require.NotContains(t, strings.ToLower(string(entry.Details)), "content")
}

func TestAdminMemoryOverviewMapsForbidden(t *testing.T) {
	svc := &stubMemoryAdminService{overviewErr: types.ErrMemoryForbidden}
	audit := &memoryAdminAuditFake{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/memory/admin/overview", nil)
	newMemoryAdminTestRouter(svc, audit).ServeHTTP(w, req)

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Empty(t, audit.entries, "被拒绝的请求不写成功审计")
}

func TestAdminListMemoryUsersPassesPaging(t *testing.T) {
	svc := &stubMemoryAdminService{
		summaries: []*types.MemoryUserSummary{{UserID: "alice", ItemCount: 3, ActiveCount: 1}},
		total:     3,
	}
	audit := &memoryAdminAuditFake{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/memory/admin/users?limit=2&offset=1", nil)
	newMemoryAdminTestRouter(svc, audit).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 2, svc.gotLimit)
	require.Equal(t, 1, svc.gotOffset)
	data := decodeAdminBody(t, w)
	require.EqualValues(t, 3, data["total"])
	items := data["items"].([]interface{})
	require.Len(t, items, 1)
	require.Equal(t, "alice", items[0].(map[string]interface{})["user_id"])

	require.Len(t, audit.entries, 1)
	require.Equal(t, types.AuditActionMemoryAdminUsersListed, audit.entries[0].Action)
}

func TestAdminListMemoryUsersClampsPaging(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
	}{
		{name: "缺省回落默认页长", query: "", wantLimit: types.MemoryAdminDefaultPageSize, wantOffset: 0},
		{name: "非法值回落默认页长", query: "?limit=abc&offset=xyz", wantLimit: types.MemoryAdminDefaultPageSize, wantOffset: 0},
		{name: "超上限收敛", query: "?limit=100000", wantLimit: types.MemoryAdminMaxPageSize, wantOffset: 0},
		{name: "负偏移归零", query: "?limit=10&offset=-5", wantLimit: 10, wantOffset: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &stubMemoryAdminService{}
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/memory/admin/users"+tc.query, nil)
			newMemoryAdminTestRouter(svc, &memoryAdminAuditFake{}).ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, tc.wantLimit, svc.gotLimit)
			require.Equal(t, tc.wantOffset, svc.gotOffset)
		})
	}
}

// TestAdminGetMemoryUserNotFoundIsIndistinguishable 覆盖 FR-09 规则 4 的防探测要求：
// "成员不存在"与"成员属于其他工作区"必须返回完全一致的 404，
// 调用方无法用状态码或文案差异判断某用户是否存在于别的工作区。
func TestAdminGetMemoryUserNotFoundIsIndistinguishable(t *testing.T) {
	responses := make([]string, 0, 2)
	for _, svc := range []*stubMemoryAdminService{
		{detailErr: types.ErrMemoryAdminTargetNotFound}, // 本工作区查无此人
		{detailErr: types.ErrMemoryAdminTargetNotFound}, // 外部工作区成员（服务层同一个错误）
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/memory/admin/users/zoe", nil)
		newMemoryAdminTestRouter(svc, &memoryAdminAuditFake{}).ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
		responses = append(responses, w.Body.String())
	}
	require.Equal(t, responses[0], responses[1], "两种情况的响应体必须逐字节一致")
}

func TestAdminGetMemoryUserReturnsDetail(t *testing.T) {
	svc := &stubMemoryAdminService{detail: &types.MemoryAdminUserDetail{
		MemoryUserSummary: types.MemoryUserSummary{
			UserID: "alice", ItemCount: 3, ActiveCount: 1, PendingCount: 1, ArchivedCount: 1,
		},
		ByCategory:      map[string]int64{"fact": 1, "profile": 1, "preference": 1},
		WriteMode:       types.MemoryWriteAuto,
		MaxActiveItems:  200,
		MaxPendingItems: 50,
	}}
	audit := &memoryAdminAuditFake{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/memory/admin/users/alice", nil)
	newMemoryAdminTestRouter(svc, audit).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "alice", svc.gotUserID)
	data := decodeAdminBody(t, w)
	require.Equal(t, "alice", data["user_id"])
	require.EqualValues(t, 3, data["item_count"])
	require.EqualValues(t, 1, data["pending_count"])

	require.Len(t, audit.entries, 1)
	entry := audit.entries[0]
	require.Equal(t, types.AuditActionMemoryAdminUserInspected, entry.Action)
	require.Equal(t, "alice", entry.TargetUserID, "审计必须记录被查看的成员")
	require.Equal(t, "memory_user", entry.TargetType)
	require.Equal(t, "alice", entry.TargetID)
}

func TestAdminBatchArchiveValidation(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
		wantCall int
	}{
		{name: "缺少 user_id", body: `{"ids":["f-1"]}`, wantCode: http.StatusBadRequest},
		{name: "请求体不是合法 JSON", body: `{`, wantCode: http.StatusBadRequest},
		{name: "ids 为空", body: `{"user_id":"bob"}`, wantCode: http.StatusBadRequest},
		{name: "ids 全是空白串", body: `{"user_id":"bob","ids":["","  "]}`, wantCode: http.StatusBadRequest},
		{name: "正常请求", body: `{"user_id":"bob","ids":["f-1","f-2"]}`, wantCode: http.StatusOK, wantCall: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &stubMemoryAdminService{batchResult: &types.MemoryAdminBatchResponse{
				Action: types.MemoryBatchActionArchive, UserID: "bob", Requested: 2, Affected: 2,
			}}
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/memory/admin/facts/batch-archive",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			newMemoryAdminTestRouter(svc, &memoryAdminAuditFake{}).ServeHTTP(w, req)
			require.Equal(t, tc.wantCode, w.Code, w.Body.String())
			require.Equal(t, tc.wantCall, svc.archiveCalls, "非法请求不得触达服务层")
		})
	}
}

func TestAdminBatchArchiveRejectsOversizedRequest(t *testing.T) {
	ids := make([]string, types.MemoryAdminBatchLimit+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("f-%d", i)
	}
	payload, err := json.Marshal(map[string]interface{}{"user_id": "bob", "ids": ids})
	require.NoError(t, err)

	svc := &stubMemoryAdminService{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/memory/admin/facts/batch-archive",
		strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	newMemoryAdminTestRouter(svc, &memoryAdminAuditFake{}).ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Zero(t, svc.archiveCalls, "超上限的请求必须在 handler 层就被拦下")
}

func TestAdminBatchArchiveWritesAudit(t *testing.T) {
	svc := &stubMemoryAdminService{batchResult: &types.MemoryAdminBatchResponse{
		Action: types.MemoryBatchActionArchive, UserID: "bob", Requested: 3, Affected: 2,
	}}
	audit := &memoryAdminAuditFake{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/memory/admin/facts/batch-archive",
		strings.NewReader(`{"user_id":"bob","ids":["f-1","f-2","nope"]}`))
	req.Header.Set("Content-Type", "application/json")
	newMemoryAdminTestRouter(svc, audit).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	data := decodeAdminBody(t, w)
	require.EqualValues(t, 2, data["affected"])
	require.EqualValues(t, 3, data["requested"])
	require.Equal(t, []string{"f-1", "f-2", "nope"}, svc.gotIDs)

	require.Len(t, audit.entries, 1)
	entry := audit.entries[0]
	require.Equal(t, types.AuditActionMemoryAdminBatchArchived, entry.Action)
	require.Equal(t, "admin-user", entry.ActorUserID, "审计记录操作者")
	require.Equal(t, "bob", entry.TargetUserID, "审计记录目标用户")
	require.Empty(t, entry.ScopeType)
	var details map[string]interface{}
	require.NoError(t, json.Unmarshal(entry.Details, &details))
	require.EqualValues(t, 2, details["affected"], "审计记录影响条目数")
	require.EqualValues(t, 3, details["requested"])
	require.NotContains(t, strings.ToLower(string(entry.Details)), "content")
}

// TestAdminBatchDeletePendingReportsSkipped 覆盖 FR-09 规则 3：
// 批量删除只作用于待确认项，被拒绝的条目必须在响应与审计里都可见。
func TestAdminBatchDeletePendingReportsSkipped(t *testing.T) {
	svc := &stubMemoryAdminService{batchResult: &types.MemoryAdminBatchResponse{
		Action: types.MemoryBatchActionDelete, UserID: "bob",
		Requested: 3, Affected: 1, Skipped: []string{"active-1", "archived-1"},
	}}
	audit := &memoryAdminAuditFake{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/memory/admin/facts/batch-delete-pending",
		strings.NewReader(`{"user_id":"bob","ids":["p-1","active-1","archived-1"]}`))
	req.Header.Set("Content-Type", "application/json")
	newMemoryAdminTestRouter(svc, audit).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 1, svc.deletePendingCalls)
	require.Zero(t, svc.archiveCalls, "两个批量端点不得互相串用")
	data := decodeAdminBody(t, w)
	require.EqualValues(t, 1, data["affected"])
	require.ElementsMatch(t, []interface{}{"active-1", "archived-1"}, data["skipped"].([]interface{}))

	require.Len(t, audit.entries, 1)
	entry := audit.entries[0]
	require.Equal(t, types.AuditActionMemoryAdminBatchDeleted, entry.Action)
	require.Equal(t, "bob", entry.TargetUserID)
	var details map[string]interface{}
	require.NoError(t, json.Unmarshal(entry.Details, &details))
	require.EqualValues(t, 1, details["affected"])
	require.Len(t, details["skipped"].([]interface{}), 2)
}

// TestAdminBatchDeletePendingAllowsEmptyIDs 覆盖"清空该成员全部待确认项"：
// ids 为空是合法请求（与批量归档不同），由服务层解析成全部 pending。
func TestAdminBatchDeletePendingAllowsEmptyIDs(t *testing.T) {
	svc := &stubMemoryAdminService{batchResult: &types.MemoryAdminBatchResponse{
		Action: types.MemoryBatchActionDelete, UserID: "bob", Requested: 4, Affected: 4,
	}}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/memory/admin/facts/batch-delete-pending",
		strings.NewReader(`{"user_id":"bob"}`))
	req.Header.Set("Content-Type", "application/json")
	newMemoryAdminTestRouter(svc, &memoryAdminAuditFake{}).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 1, svc.deletePendingCalls)
	require.Empty(t, svc.gotIDs)

	// user_id 仍然必填。
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/memory/admin/facts/batch-delete-pending",
		strings.NewReader(`{"ids":["p-1"]}`))
	req.Header.Set("Content-Type", "application/json")
	newMemoryAdminTestRouter(svc, &memoryAdminAuditFake{}).ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Equal(t, 1, svc.deletePendingCalls, "缺少 user_id 不得触达服务层")
}

// TestAdminAuditFailureDoesNotBreakResponse 覆盖 PRD §5.3 规则 1：
// 审计属于增强能力，审计侧故障不得让管理员的治理操作失败。
func TestAdminAuditFailureDoesNotBreakResponse(t *testing.T) {
	svc := &stubMemoryAdminService{
		overview: &types.MemoryAdminOverview{TenantID: 1, Total: 6, UserCount: 3},
		batchResult: &types.MemoryAdminBatchResponse{
			Action: types.MemoryBatchActionArchive, UserID: "bob", Requested: 1, Affected: 1,
		},
	}
	audit := &memoryAdminAuditFake{fail: true}
	router := newMemoryAdminTestRouter(svc, audit)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/memory/admin/overview", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/memory/admin/facts/batch-archive",
		strings.NewReader(`{"user_id":"bob","ids":["f-1"]}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Empty(t, audit.entries)
}

// TestAdminConsoleWithoutAuditProviderStillWorks 覆盖 lite 模式：
// 审计服务未注入时控制台照常工作。
func TestAdminConsoleWithoutAuditProviderStillWorks(t *testing.T) {
	svc := &stubMemoryAdminService{
		overview: &types.MemoryAdminOverview{TenantID: 1, Total: 0, UserCount: 0},
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/memory/admin/overview", nil)
	newMemoryAdminTestRouter(svc, nil).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestAdminBatchMapsServiceErrors 覆盖错误映射表。
func TestAdminBatchMapsServiceErrors(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
	}{
		{name: "非管理员", err: types.ErrMemoryForbidden, wantCode: http.StatusForbidden},
		{name: "请求非法", err: fmt.Errorf("%w: ids is required", types.ErrMemoryAdminBadRequest), wantCode: http.StatusBadRequest},
		{name: "目标不在本工作区", err: types.ErrMemoryAdminTargetNotFound, wantCode: http.StatusNotFound},
		{name: "状态机冲突", err: types.ErrMemoryConflict, wantCode: http.StatusConflict},
		{name: "底层故障", err: fmt.Errorf("connection reset"), wantCode: http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &stubMemoryAdminService{batchErr: tc.err}
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/memory/admin/facts/batch-archive",
				strings.NewReader(`{"user_id":"bob","ids":["f-1"]}`))
			req.Header.Set("Content-Type", "application/json")
			newMemoryAdminTestRouter(svc, &memoryAdminAuditFake{}).ServeHTTP(w, req)
			require.Equal(t, tc.wantCode, w.Code, w.Body.String())
		})
	}
}
