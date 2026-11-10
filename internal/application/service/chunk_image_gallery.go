package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// FR-18 文档图片画廊：service 层画廊查询。
// 从上游 WeKnora-main internal/application/service/chunk.go（ListImagesByKnowledgeBaseID）
// 与 gallery_attrs.go（buildImageAssetQuery / galleryField / sortedKeys）照抄适配：
// - Zilan 无画廊契约端点（上游的 gallery-config 策略分层未引入），故去掉
//   constrainGalleryFilter 的契约裁剪；repository 层的属性名注入防线仍在。
// - 未知判定值（"maybe"、大小写写错的 "OFF"）与未知排序字段一律忽略而非
//   报错：画廊不能因契约面板出现新字段而 500（上游语义）。
// - 来源文档名（SourceName）不在此解析：chunkService 未持有 knowledgeRepo，
//   改由 handler 层经 KnowledgeService.GetKnowledgeBatch 批量回填，避免改动
//   chunkService 构造签名与容器装配。
// - 画廊查询失败不影响既有功能（PRD §5.3）：本文件只新增读路径，不改任何写路径。

// imageAssetPageSize 上限：一页最多返回的图片数（上游 maxPageSize=1000，
// 与 types.Pagination.GetPageSize 的 1000 上限一致）。
const imageAssetPageSize = 1000

// galleryBuiltinSourceID is the id of the builtin attribute source; its
// values come from the asset's own columns, not the image_info attr map.
const galleryBuiltinSourceID = "builtin"

// galleryDefaultSearchFields is the search field set used when the request
// does not name one (older clients, API callers): caption and OCR text.
var galleryDefaultSearchFields = []string{
	types.GalleryAttrID(galleryBuiltinSourceID, "caption"),
	types.GalleryAttrID(galleryBuiltinSourceID, "ocr_text"),
}

// The two verdicts an attribute rule can carry; every other verdict is
// ignored rather than rejected.
const (
	galleryRuleOff = "off"
	galleryRuleOn  = "on"
)

// galleryField maps a namespaced gallery attribute id onto the field the
// repository query reads: builtin ids address the asset's own columns (an
// unknown builtin name passes through and the repository safely ignores it),
// every other source reads the source-local name from the image_info attrs
// map —— 当前没有这类来源时该分支自然返回空结果，不会报错（PRD 的优雅
// 降级要求）。
func galleryField(id string) (types.ImageAssetField, bool) {
	sourceID, name, ok := types.SplitGalleryAttrID(id)
	if !ok {
		return types.ImageAssetField{}, false
	}
	if sourceID == galleryBuiltinSourceID {
		return types.ImageAssetField{Builtin: name}, true
	}
	return types.ImageAssetField{Attr: name}, true
}

// sortedKeys returns the map's keys in order, so the same request always
// produces the same query.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// buildImageAssetQuery compiles the UI filter into the storage-neutral query.
// Map iteration is sorted so the same request always produces the same query.
func buildImageAssetQuery(filter *types.ImageListFilter) *types.ImageAssetQuery {
	q := &types.ImageAssetQuery{
		SortField: types.ImageAssetField{Builtin: "created_at"},
		SortDesc:  true,
	}
	if filter == nil {
		return q
	}
	q.Keyword = strings.TrimSpace(filter.Keyword)
	q.IsEnabled = filter.IsEnabled
	if len(filter.KnowledgeIDs) > 0 {
		q.KnowledgeIDs = filter.KnowledgeIDs
	}

	// The search fields the filter names, or the gallery default: description
	// and OCR text. Ids that do not resolve are dropped: a UI passing an
	// unknown id (an observed source that was later removed) must not 500
	// the gallery.
	searchIn := filter.SearchIn
	if len(searchIn) == 0 {
		searchIn = galleryDefaultSearchFields
	}
	for _, id := range searchIn {
		if f, ok := galleryField(id); ok {
			q.SearchFields = append(q.SearchFields, f)
		}
	}

	for _, id := range sortedKeys(filter.AttrFilters) {
		f, ok := galleryField(id)
		if !ok {
			continue
		}
		var values []string
		for _, v := range filter.AttrFilters[id] {
			values = append(values, strings.TrimSpace(v))
		}
		if len(values) > 0 {
			q.AttrFilters = append(q.AttrFilters, types.ImageAssetValueSet{Field: f, Values: values})
		}
	}

	for _, id := range sortedKeys(filter.AttrRules) {
		f, ok := galleryField(id)
		if !ok {
			continue
		}
		var on, off []string
		verdicts := filter.AttrRules[id]
		for _, value := range sortedKeys(verdicts) {
			switch verdicts[value] {
			case galleryRuleOn:
				on = append(on, value)
			case galleryRuleOff:
				off = append(off, value)
			}
		}
		if len(on) > 0 {
			q.OnRules = append(q.OnRules, types.ImageAssetValueSet{Field: f, Values: on})
		}
		if len(off) > 0 {
			q.OffRules = append(q.OffRules, types.ImageAssetValueSet{Field: f, Values: off})
		}
	}

	// The gallery default is the upload order, newest first. The bare legacy
	// values ("created_at", ...) keep the old semantics: images of one chunk
	// share its timestamps, so the listing follows the time the images were
	// added to the document (the "All" list the panel opens on).
	switch filter.SortBy {
	case "":
	case "created_at", "updated_at", "caption":
		// Legacy bare field names map onto their builtin ids.
		q.SortField = types.ImageAssetField{Builtin: filter.SortBy}
	default:
		if f, ok := galleryField(filter.SortBy); ok {
			q.SortField = f
		}
	}
	q.SortDesc = filter.SortOrder != "asc"
	return q
}

// ListImagesByKnowledgeBaseID lists one page of a KB's image assets for the
// gallery (FR-18), de-duplicated per image and filtered per the caller's
// constraints. SourceName is left empty here; the gallery handler fills it
// with a batch knowledge lookup.
func (s *chunkService) ListImagesByKnowledgeBaseID(
	ctx context.Context, kbID string, page *types.Pagination, filter *types.ImageListFilter,
) (*types.PageResult, error) {
	tenantID := types.MustTenantIDFromContext(ctx)
	query := buildImageAssetQuery(filter)

	if page == nil {
		page = &types.Pagination{}
	}
	// cap both ends：页大小至少 1，至多 imageAssetPageSize。
	pageSize := max(page.GetPageSize(), 1)
	pageSize = min(pageSize, imageAssetPageSize)
	offset := max((page.GetPage()-1)*pageSize, 0)
	query.Offset = offset
	query.Limit = pageSize

	rows, total, err := s.chunkRepository.ListImageAssets(ctx, tenantID, kbID, query)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": kbID,
			"tenant_id":         tenantID,
		})
		return nil, err
	}

	assets := make([]types.ImageAsset, len(rows))
	for i, r := range rows {
		assets[i] = types.ImageAsset{
			ID:          fmt.Sprintf("%s#%d", r.ChunkID, r.ImageIndex),
			ChunkID:     r.ChunkID,
			KnowledgeID: r.KnowledgeID,
			ChunkType:   r.ChunkType,
			URL:         r.URL,
			OriginalURL: r.OriginalURL,
			Caption:     r.Caption,
			OCRText:     r.OCRText,
			IsEnabled:   r.IsEnabled,
			Status:      r.Status,
			CreatedAt:   r.CreatedAt,
			UpdatedAt:   r.UpdatedAt,
		}
		if r.AttrsJSON != "" {
			if err := json.Unmarshal([]byte(r.AttrsJSON), &assets[i].Attrs); err != nil {
				assets[i].Attrs = nil
			}
		}
	}

	return &types.PageResult{
		Total:    total,
		Page:     page.GetPage(),
		PageSize: pageSize,
		Data:     assets,
	}, nil
}
