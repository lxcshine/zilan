package types

import "testing"

// FR-19①：RRF 融合常数必须是工作区级可配项，未配置时回落到历史默认值 60。
func TestGetEffectiveRRFK(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *RetrievalConfig
		want int
	}{
		{"nil config falls back to 60", nil, 60},
		{"unset falls back to 60", &RetrievalConfig{}, 60},
		{"non-positive falls back to 60", &RetrievalConfig{RRFK: -5}, 60},
		{"configured value wins", &RetrievalConfig{RRFK: 30}, 30},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.GetEffectiveRRFK(); got != tc.want {
				t.Fatalf("GetEffectiveRRFK() = %d, want %d", got, tc.want)
			}
		})
	}
}

// FR-19②：FAQ 列表展开轮数可配，带默认值与上限钳制。
func TestGetEffectiveFAQExpandCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *RetrievalConfig
		want int
	}{
		{"nil config uses the default", nil, DefaultFAQExpandCount},
		{"unset uses the default", &RetrievalConfig{}, DefaultFAQExpandCount},
		{"non-positive uses the default", &RetrievalConfig{FAQExpandCount: -1}, DefaultFAQExpandCount},
		{"configured value wins", &RetrievalConfig{FAQExpandCount: 3}, 3},
		{"above the cap is clamped", &RetrievalConfig{FAQExpandCount: 999}, MaxFAQExpandCount},
		{"exactly the cap is kept", &RetrievalConfig{FAQExpandCount: MaxFAQExpandCount}, MaxFAQExpandCount},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.GetEffectiveFAQExpandCount(); got != tc.want {
				t.Fatalf("GetEffectiveFAQExpandCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

// 扩池默认值必须来自共享常量，避免两处 50 各自漂移。
func TestGetEffectiveEmbeddingTopKUsesSharedDefault(t *testing.T) {
	t.Parallel()
	if got := (*RetrievalConfig)(nil).GetEffectiveEmbeddingTopK(); got != DefaultRetrievalTopK {
		t.Fatalf("GetEffectiveEmbeddingTopK() = %d, want DefaultRetrievalTopK(%d)", got, DefaultRetrievalTopK)
	}
	if got := (&RetrievalConfig{EmbeddingTopK: 7}).GetEffectiveEmbeddingTopK(); got != 7 {
		t.Fatalf("configured EmbeddingTopK = %d, want 7", got)
	}
}

// 展开轮数与 RRF 常数都要能安全地跟着 Clone 走（深拷贝不能丢字段）。
func TestCloneKeepsFR19Fields(t *testing.T) {
	t.Parallel()
	src := &RetrievalConfig{RRFK: 25, FAQExpandCount: 8}
	dst := src.Clone()
	if dst.RRFK != 25 || dst.FAQExpandCount != 8 {
		t.Fatalf("Clone lost FR-19 fields: %+v", dst)
	}
	dst.RRFK = 99
	if src.RRFK != 25 {
		t.Fatal("Clone must not alias the source config")
	}
}
