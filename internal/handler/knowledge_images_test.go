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

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// FR-18 画廊 handler 测试：GET/POST 两档 knowledge_ids 上限与来源名回填。
// 从上游 WeKnora-main internal/handler/knowledge_images_test.go 照抄适配：
// Zilan 的画廊是独立 ImageGalleryHandler（复用 KnowledgeHandler 的访问校验），
// stub 也相应改为挂在 KnowledgeHandler 上。

// stubGalleryChunkService records the knowledge ids one images-listing call
// received, so tests can assert how the GET cap and the POST body cap differ.
type stubGalleryChunkService struct {
	interfaces.ChunkService
	gotIDs  []string
	gotPage *types.Pagination
}

func (s *stubGalleryChunkService) ListImagesByKnowledgeBaseID(
	_ context.Context,
	_ string,
	page *types.Pagination,
	filter *types.ImageListFilter,
) (*types.PageResult, error) {
	s.gotIDs = filter.KnowledgeIDs
	s.gotPage = page
	return types.NewPageResult(0, page, []types.ImageAsset{}), nil
}

// stubGalleryKnowledgeService feeds resolveImageAssetSourceNames with a fixed
// name for one knowledge id.
type stubGalleryKnowledgeService struct {
	interfaces.KnowledgeService
}

func (s *stubGalleryKnowledgeService) GetKnowledgeBatch(
	_ context.Context, _ uint64, ids []string,
) ([]*types.Knowledge, error) {
	items := make([]*types.Knowledge, 0, len(ids))
	for _, id := range ids {
		items = append(items, &types.Knowledge{ID: id, Title: "doc-" + strings.ToUpper(id)})
	}
	return items, nil
}

func newImagesRouter(t *testing.T, chunks interfaces.ChunkService, kg interfaces.KnowledgeService) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorCapture())
	r.Use(func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), uint64(1))
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(1))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	// KB 属于租户 1：validateKnowledgeBaseAccess 走 owner 路径直接放行。
	kh := &KnowledgeHandler{kbService: &stubKBService{
		get: func(_ context.Context, id string) (*types.KnowledgeBase, error) {
			return &types.KnowledgeBase{ID: id, TenantID: 1}, nil
		},
	}}
	h := NewImageGalleryHandler(kh, chunks, kg)
	r.GET("/knowledge-bases/:id/images", h.ListImages)
	r.POST("/knowledge-bases/:id/images", h.ListImagesPost)
	return r
}

// commaIDs builds "id-0,id-1,..." of the requested size.
func commaIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("0198c0de-0000-7000-8000-%012d", i)
	}
	return ids
}

func TestListImagesPost_KnowledgeIDBody(t *testing.T) {
	tests := []struct {
		name    string
		ids     int
		wantCap int
		wantHTT int
	}{
		{
			name:    "60 ids pass the POST body although the GET cap is 50",
			ids:     60,
			wantCap: 60,
			wantHTT: http.StatusOK,
		},
		{
			name:    "the body cap still guards abuse",
			ids:     maxKnowledgeIDFilterBody + 1,
			wantCap: 0,
			wantHTT: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &stubGalleryChunkService{}
			router := newImagesRouter(t, svc, &stubGalleryKnowledgeService{})
			ids := commaIDs(tt.ids)
			payload, _ := json.Marshal(map[string]any{"knowledge_ids": ids})
			req := httptest.NewRequest(http.MethodPost, "/knowledge-bases/kb-1/images",
				strings.NewReader(string(payload)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			router.ServeHTTP(w, req)

			if w.Code != tt.wantHTT {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tt.wantHTT, w.Body.String())
			}
			if len(svc.gotIDs) != tt.wantCap {
				t.Fatalf("got %d knowledge ids, want %d", len(svc.gotIDs), tt.wantCap)
			}
		})
	}
}

func TestListImagesPost_EmptyBody(t *testing.T) {
	svc := &stubGalleryChunkService{}
	router := newImagesRouter(t, svc, &stubGalleryKnowledgeService{})
	req := httptest.NewRequest(http.MethodPost, "/knowledge-bases/kb-1/images", nil)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(svc.gotIDs) != 0 {
		t.Fatalf("got %d knowledge ids, want none", len(svc.gotIDs))
	}
}

func TestListImagesGet_KnowledgeIDCap(t *testing.T) {
	svc := &stubGalleryChunkService{}
	router := newImagesRouter(t, svc, &stubGalleryKnowledgeService{})
	// 60 ids on the URL: the GET cap of 50 truncates them.
	url := "/knowledge-bases/kb-1/images?knowledge_ids=" + strings.Join(commaIDs(60), ",")
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(svc.gotIDs) != maxKnowledgeIDFilterGET {
		t.Fatalf("got %d knowledge ids, want the GET cap %d", len(svc.gotIDs), maxKnowledgeIDFilterGET)
	}
}

// 跨租户隔离：KB 不属于请求租户且无任何共享通道 → 403，绝不外泄图片。
func TestListImages_DeniesForeignKnowledgeBase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(errorCapture())
	r.Use(func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), uint64(2))
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(2))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	kh := &KnowledgeHandler{kbService: &stubKBService{
		get: func(_ context.Context, id string) (*types.KnowledgeBase, error) {
			return &types.KnowledgeBase{ID: id, TenantID: 1}, nil
		},
	}}
	h := NewImageGalleryHandler(kh, &stubGalleryChunkService{}, &stubGalleryKnowledgeService{})
	r.GET("/knowledge-bases/:id/images", h.ListImages)

	req := httptest.NewRequest(http.MethodGet, "/knowledge-bases/kb-1/images", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}
