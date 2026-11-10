package types

import (
	"reflect"
	"testing"
)

// TestMemoryAdminPage 覆盖控制台分页参数的钳制：查询串是外部输入，
// 必须在进入 SQL 之前收敛到 [1, MemoryAdminMaxPageSize] × [0, +∞)。
func TestMemoryAdminPage(t *testing.T) {
	cases := []struct {
		name       string
		limit      int
		offset     int
		wantLimit  int
		wantOffset int
	}{
		{name: "合法值原样通过", limit: 20, offset: 40, wantLimit: 20, wantOffset: 40},
		{name: "边界页长", limit: MemoryAdminMaxPageSize, offset: 0, wantLimit: MemoryAdminMaxPageSize, wantOffset: 0},
		{name: "limit 为 0 回落默认", limit: 0, offset: 0, wantLimit: MemoryAdminDefaultPageSize, wantOffset: 0},
		{name: "limit 为负回落默认", limit: -10, offset: 5, wantLimit: MemoryAdminDefaultPageSize, wantOffset: 5},
		{name: "limit 超上限收敛", limit: 100000, offset: 0, wantLimit: MemoryAdminMaxPageSize, wantOffset: 0},
		{name: "offset 为负归零", limit: 10, offset: -1, wantLimit: 10, wantOffset: 0},
		{name: "两个参数同时非法", limit: 0, offset: -100, wantLimit: MemoryAdminDefaultPageSize, wantOffset: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limit, offset := MemoryAdminPage(tc.limit, tc.offset)
			if limit != tc.wantLimit || offset != tc.wantOffset {
				t.Fatalf("MemoryAdminPage(%d, %d) = (%d, %d), want (%d, %d)",
					tc.limit, tc.offset, limit, offset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

// TestMemoryAdminPageInvariants 守住常量之间的关系：默认页长必须落在硬顶之内，
// 否则"回落默认"这一步本身就会突破上限。
func TestMemoryAdminPageInvariants(t *testing.T) {
	if MemoryAdminDefaultPageSize > MemoryAdminMaxPageSize {
		t.Fatalf("默认页长 %d 不得超过硬顶 %d",
			MemoryAdminDefaultPageSize, MemoryAdminMaxPageSize)
	}
	if MemoryAdminBatchLimit <= 0 {
		t.Fatalf("批量上限必须为正数，当前 %d", MemoryAdminBatchLimit)
	}
}

// jsonFieldNames 递归收集结构体（含匿名嵌入）的字段名。
func jsonFieldNames(value interface{}) []string {
	out := make([]string, 0)
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if field.Anonymous && field.Type.Kind() == reflect.Struct {
				walk(field.Type)
				continue
			}
			out = append(out, field.Name)
		}
	}
	walk(reflect.TypeOf(value))
	return out
}

// TestMemoryUserSummaryCarriesNoContent 是 FR-09 规则 4 的结构级断言：
// 管理员侧的聚合类型不得出现承载记忆正文的字段。新增字段时若不小心带上
// Content / Object / Embedding，这个测试会失败。
func TestMemoryUserSummaryCarriesNoContent(t *testing.T) {
	forbidden := []string{"Content", "Object", "Subject", "Predicate", "Embedding"}
	targets := map[string][]string{
		"MemoryUserSummary":        jsonFieldNames(MemoryUserSummary{}),
		"MemoryAdminOverview":      jsonFieldNames(MemoryAdminOverview{}),
		"MemoryAdminUserDetail":    jsonFieldNames(MemoryAdminUserDetail{}),
		"MemoryAdminBatchRequest":  jsonFieldNames(MemoryAdminBatchRequest{}),
		"MemoryAdminBatchResponse": jsonFieldNames(MemoryAdminBatchResponse{}),
	}
	for name, fields := range targets {
		for _, field := range fields {
			for _, banned := range forbidden {
				if field == banned {
					t.Fatalf("%s 暴露了 %s 字段：控制台只能输出聚合计数，不得输出记忆正文", name, field)
				}
			}
		}
	}
}
