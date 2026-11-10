package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

// FR-18 画廊：查询组装（buildImageAssetQuery / galleryField）单测。
// 从上游 WeKnora-main 的 service 画廊测试（gallery_attrs_test.go）精神照抄：
// 未知属性 ID 与未知判定值一律忽略而非报错（契约面板的优雅降级），
// map 迭代按键排序使同一请求恒产出同一查询。

func TestBuildImageAssetQueryDefaults(t *testing.T) {
	q := buildImageAssetQuery(&types.ImageListFilter{})
	// 默认：created_at 降序 + 默认搜索域（caption/ocr_text）。
	require.Equal(t, types.ImageAssetField{Builtin: "created_at"}, q.SortField)
	require.True(t, q.SortDesc)
	require.Nil(t, q.IsEnabled)
	require.Empty(t, q.KnowledgeIDs)
	require.Equal(t, []types.ImageAssetField{{Builtin: "caption"}, {Builtin: "ocr_text"}}, q.SearchFields)

	q = buildImageAssetQuery(&types.ImageListFilter{SortBy: "updated_at", SortOrder: "asc"})
	require.Equal(t, types.ImageAssetField{Builtin: "updated_at"}, q.SortField)
	require.False(t, q.SortDesc)

	// 不可解析的排序字段静默保持默认（不报错）；可解析但未知的 builtin
	// 排序名透传，由 repository 的 sortKey 对未知列回落 created_at。
	q = buildImageAssetQuery(&types.ImageListFilter{SortBy: "nosource"})
	require.Equal(t, types.ImageAssetField{Builtin: "created_at"}, q.SortField)

	q = buildImageAssetQuery(&types.ImageListFilter{SortBy: "builtin:whatever"})
	require.Equal(t, types.ImageAssetField{Builtin: "whatever"}, q.SortField, "可解析的 id 透传，repository 负责未知列回落")
}

func TestBuildImageAssetQueryKeywordSearchFields(t *testing.T) {
	q := buildImageAssetQuery(&types.ImageListFilter{Keyword: "revenue"})
	// 未指定搜索域时默认 caption + ocr_text（PRD 需求 2）。
	require.Equal(t, []types.ImageAssetField{{Builtin: "caption"}, {Builtin: "ocr_text"}}, q.SearchFields)

	q = buildImageAssetQuery(&types.ImageListFilter{Keyword: "revenue", SearchIn: []string{"builtin:caption", "builtin:unknown", "nosource", ""}})
	// 无分隔符与空串不解析；builtin:unknown 透传（repository 方言层安全空转）。
	require.Equal(t, []types.ImageAssetField{{Builtin: "caption"}, {Builtin: "unknown"}}, q.SearchFields)
}

func TestBuildImageAssetQueryAttrFiltersAndRules(t *testing.T) {
	on := true
	q := buildImageAssetQuery(&types.ImageListFilter{
		AttrFilters: map[string][]string{
			"builtin:caption":    {"a"},
			"builtin:is_enabled": {"true"},
			"builtin:nope":       {"x"}, // 未知 builtin 字段：透传，由 repository 空转
			"":                   {"y"}, // 空 ID：不解析
			"obs:text":           {"t"}, // 通用观测来源：透传为 Attr 字段
		},
		AttrRules: map[string]map[string]string{
			"obs:contain.text": {"block": "off", "chart": "on", "": ""},
			"builtin:ocr_text": {"spam": "OFF"}, // 大小写不同的判定：忽略而非报错
		},
		IsEnabled: &on,
	})
	// map 迭代按键排序；空 ID 不解析。
	require.Equal(t, []types.ImageAssetValueSet{
		{Field: types.ImageAssetField{Builtin: "caption"}, Values: []string{"a"}},
		{Field: types.ImageAssetField{Builtin: "is_enabled"}, Values: []string{"true"}},
		{Field: types.ImageAssetField{Builtin: "nope"}, Values: []string{"x"}},
		{Field: types.ImageAssetField{Attr: "text"}, Values: []string{"t"}},
	}, q.AttrFilters)
	// 判定值也排序；未知判定（空串、"OFF"）被忽略，不进任何一组。
	require.Equal(t, []types.ImageAssetValueSet{
		{Field: types.ImageAssetField{Attr: "contain.text"}, Values: []string{"chart"}},
	}, q.OnRules)
	require.Equal(t, []types.ImageAssetValueSet{
		{Field: types.ImageAssetField{Attr: "contain.text"}, Values: []string{"block"}},
	}, q.OffRules)
	require.True(t, *q.IsEnabled)
}

func TestGalleryField(t *testing.T) {
	f, ok := galleryField("builtin:caption")
	require.True(t, ok)
	require.Equal(t, "caption", f.Builtin)

	// 未知 builtin 名透传：repository 的方言层负责安全空转。
	f, ok = galleryField("builtin:nope")
	require.True(t, ok)
	require.Equal(t, "nope", f.Builtin)

	// Zilan 无属性观测来源时，通用来源仍解析为 Attr 字段（查询安全地空转）。
	f, ok = galleryField("obs:contain.text")
	require.True(t, ok)
	require.Equal(t, "contain.text", f.Attr)

	_, ok = galleryField("builtin")
	require.False(t, ok, "无分隔符的 ID 不解析")
	_, ok = galleryField("builtin:")
	require.False(t, ok, "空属性名不解析")
	_, ok = galleryField(":x")
	require.False(t, ok, "空来源不解析")
}

func TestSplitGalleryAttrIDRoundTrip(t *testing.T) {
	require.Equal(t, "builtin:caption", types.GalleryAttrID("builtin", "caption"))
	source, name, ok := types.SplitGalleryAttrID("obs:contain.text")
	require.True(t, ok)
	require.Equal(t, "obs", source)
	require.Equal(t, "contain.text", name)
}
