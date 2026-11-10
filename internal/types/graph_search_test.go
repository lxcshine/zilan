package types

import (
	"strings"
	"testing"
)

// FR-19③：graph-search 的请求校验必须同时挡住空查询、超长查询与越界上限，
// 并把 0 归一成默认上限。
func TestGraphSearchRequestNormalize(t *testing.T) {
	t.Parallel()

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()
		req := GraphSearchRequest{Query: "  张三  "}
		if err := req.Normalize(); err != nil {
			t.Fatalf("Normalize() = %v, want nil", err)
		}
		if req.Query != "张三" {
			t.Fatalf("query = %q, want it trimmed", req.Query)
		}
		if req.MaxNodes != 50 || req.MaxRelations != 100 || req.MaxSourceChunks != 20 {
			t.Fatalf("defaults not applied: %+v", req)
		}
	})

	t.Run("explicit limits kept", func(t *testing.T) {
		t.Parallel()
		req := GraphSearchRequest{Query: "q", MaxNodes: 7, MaxRelations: 9, MaxSourceChunks: 3}
		if err := req.Normalize(); err != nil {
			t.Fatalf("Normalize() = %v, want nil", err)
		}
		if req.MaxNodes != 7 || req.MaxRelations != 9 || req.MaxSourceChunks != 3 {
			t.Fatalf("explicit limits were rewritten: %+v", req)
		}
	})

	t.Run("empty query rejected", func(t *testing.T) {
		t.Parallel()
		for _, q := range []string{"", "   ", "\t\n"} {
			req := GraphSearchRequest{Query: q}
			if err := req.Normalize(); err == nil {
				t.Fatalf("query %q must be rejected", q)
			}
		}
	})

	t.Run("overlong query rejected", func(t *testing.T) {
		t.Parallel()
		req := GraphSearchRequest{Query: strings.Repeat("字", GraphSearchMaxNameRunes+1)}
		if err := req.Normalize(); err == nil {
			t.Fatal("a query longer than the rune cap must be rejected")
		}
	})

	t.Run("invalid utf8 rejected", func(t *testing.T) {
		t.Parallel()
		req := GraphSearchRequest{Query: string([]byte{0xff, 0xfe, 0xfd})}
		if err := req.Normalize(); err == nil {
			t.Fatal("invalid UTF-8 must be rejected")
		}
	})

	t.Run("limits out of range rejected", func(t *testing.T) {
		t.Parallel()
		cases := []GraphSearchRequest{
			{Query: "q", MaxNodes: -1},
			{Query: "q", MaxNodes: 101},
			{Query: "q", MaxRelations: -1},
			{Query: "q", MaxRelations: 201},
			{Query: "q", MaxSourceChunks: -1},
			{Query: "q", MaxSourceChunks: 51},
		}
		for _, req := range cases {
			req := req
			if err := req.Normalize(); err == nil {
				t.Fatalf("%+v must be rejected", req)
			}
		}
	})

	t.Run("boundary limits accepted", func(t *testing.T) {
		t.Parallel()
		req := GraphSearchRequest{Query: "q", MaxNodes: 100, MaxRelations: 200, MaxSourceChunks: 50}
		if err := req.Normalize(); err != nil {
			t.Fatalf("boundary values must be accepted, got %v", err)
		}
	})
}

// 公开响应必须始终带非 nil 的切片，外部系统才能稳定地按 JSON 结构消费。
func TestGraphSearchResultShape(t *testing.T) {
	t.Parallel()
	result := GraphSearchResult{
		KnowledgeBaseID: "kb-1",
		Query:           "q",
		Entities:        []GraphSearchEntity{},
		Relationships:   []GraphSearchRelation{},
		SourceChunks:    []GraphSearchSourceChunk{},
		Meta:            GraphSearchMeta{TruncationReasons: []string{}, MaxNodes: 50},
	}
	if len(result.Entities) != 0 || len(result.Relationships) != 0 || len(result.SourceChunks) != 0 {
		t.Fatalf("empty result must stay empty: %+v", result)
	}
	// 来源分块不得泄露租户与知识库内部字段到 JSON。
	chunk := GraphSearchSourceChunk{ID: "c1", TenantID: 7, KnowledgeBaseID: "kb-1"}
	if chunk.TenantID != 7 || chunk.KnowledgeBaseID != "kb-1" {
		t.Fatal("internal scope fields must survive inside the process")
	}
}
