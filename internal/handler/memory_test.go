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

// stubMemoryService records calls and returns canned results.
type stubMemoryService struct {
	interfaces.MemoryService
	facts       []*types.MemoryFact
	total       int64
	lastQuery   *types.MemoryFactQuery
	updateErr   error
	deleteErr   error
	deleteAllN  int64
	enabled     bool
	updatedFact *types.MemoryFact
	deletedID   string
}

func (s *stubMemoryService) ListFacts(ctx context.Context, q *types.MemoryFactQuery) ([]*types.MemoryFact, int64, error) {
	s.lastQuery = q
	return s.facts, s.total, nil
}

func (s *stubMemoryService) UpdateFact(ctx context.Context, fact *types.MemoryFact) error {
	s.updatedFact = fact
	return s.updateErr
}

func (s *stubMemoryService) DeleteFact(ctx context.Context, id string) error {
	s.deletedID = id
	return s.deleteErr
}

func (s *stubMemoryService) DeleteAllForUser(ctx context.Context) (int64, error) {
	return s.deleteAllN, nil
}

func (s *stubMemoryService) IsEnabled(ctx context.Context, userID string) bool {
	return s.enabled
}

// newMemoryTestRouter mounts the memory routes behind a middleware that
// injects the caller's (tenant, user) scope, mirroring middleware.Auth.
func newMemoryTestRouter(svc interfaces.MemoryService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())
	r.Use(func(c *gin.Context) {
		ctx := c.Request.Context()
		ctx = context.WithValue(ctx, types.TenantIDContextKey, uint64(1))
		ctx = context.WithValue(ctx, types.UserIDContextKey, "alice")
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	h := NewMemoryHandler(svc)
	r.GET("/memory/status", h.GetMemoryStatus)
	r.GET("/memory/facts", h.ListMemoryFacts)
	r.PUT("/memory/facts/:id", h.UpdateMemoryFact)
	r.DELETE("/memory/facts/:id", h.DeleteMemoryFact)
	r.DELETE("/memory", h.DeleteAllMemories)
	return r
}

func TestListMemoryFactsPassesFilters(t *testing.T) {
	svc := &stubMemoryService{
		facts: []*types.MemoryFact{{ID: "f-1", Content: "用户偏好 Python", Category: types.MemoryCategoryPreference}},
		total: 1,
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/memory/facts?category=preference&keyword=Python&page=2&page_size=5", nil)
	newMemoryTestRouter(svc).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotNil(t, svc.lastQuery)
	require.Equal(t, "preference", svc.lastQuery.Category)
	require.Equal(t, "Python", svc.lastQuery.Keyword)
	require.Equal(t, 2, svc.lastQuery.Page)
	require.Equal(t, 5, svc.lastQuery.PageSize)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	data := body["data"].(map[string]interface{})
	require.EqualValues(t, 1, data["total"])
	require.Len(t, data["items"], 1)
}

func TestListMemoryFactsClampsPageSize(t *testing.T) {
	svc := &stubMemoryService{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/memory/facts?page_size=500", nil)
	newMemoryTestRouter(svc).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 20, svc.lastQuery.PageSize, "oversized page_size must fall back to the default")
}

func TestUpdateMemoryFactValidatesStatus(t *testing.T) {
	svc := &stubMemoryService{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/memory/facts/f-1", strings.NewReader(`{"status":"bogus"}`))
	req.Header.Set("Content-Type", "application/json")
	newMemoryTestRouter(svc).ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Nil(t, svc.updatedFact, "invalid status must not reach the service")
}

func TestUpdateMemoryFactValidatesDueAt(t *testing.T) {
	svc := &stubMemoryService{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/memory/facts/f-1", strings.NewReader(`{"due_at":"next friday"}`))
	req.Header.Set("Content-Type", "application/json")
	newMemoryTestRouter(svc).ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Nil(t, svc.updatedFact)
}

func TestUpdateMemoryFactMapsNotFound(t *testing.T) {
	svc := &stubMemoryService{updateErr: fmt.Errorf("memory fact not found")}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/memory/facts/f-1", strings.NewReader(`{"content":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	newMemoryTestRouter(svc).ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdateMemoryFactSuccess(t *testing.T) {
	svc := &stubMemoryService{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/memory/facts/f-1",
		strings.NewReader(`{"content":"用户偏好 Golang","status":"active","importance":0.9,"due_at":"2026-08-20"}`))
	req.Header.Set("Content-Type", "application/json")
	newMemoryTestRouter(svc).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotNil(t, svc.updatedFact)
	require.Equal(t, "f-1", svc.updatedFact.ID)
	require.Equal(t, "用户偏好 Golang", svc.updatedFact.Content)
	require.NotNil(t, svc.updatedFact.DueAt)
}

func TestDeleteMemoryFactMapsNotFound(t *testing.T) {
	svc := &stubMemoryService{deleteErr: fmt.Errorf("memory fact not found")}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/memory/facts/f-1", nil)
	newMemoryTestRouter(svc).ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteAllMemoriesReturnsCount(t *testing.T) {
	svc := &stubMemoryService{deleteAllN: 7}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/memory", nil)
	newMemoryTestRouter(svc).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.EqualValues(t, 7, body["data"].(map[string]interface{})["deleted"])
}

func TestGetMemoryStatus(t *testing.T) {
	svc := &stubMemoryService{enabled: true, total: 3}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/memory/status", nil)
	newMemoryTestRouter(svc).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	data := body["data"].(map[string]interface{})
	require.Equal(t, true, data["enabled"])
	require.EqualValues(t, 3, data["fact_count"])
}
