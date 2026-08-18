package chatpipeline

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func mkChunks(contents ...string) []*types.Chunk {
	out := make([]*types.Chunk, 0, len(contents))
	for _, c := range contents {
		out = append(out, &types.Chunk{Content: c})
	}
	return out
}

func TestComputeKBProfileStats(t *testing.T) {
	chunks := mkChunks(
		"# 第一章 总则\n第一条 本法所称术语如下……"+strings.Repeat("内容", 100),
		"Q: 如何重置密码?\nA: 进入设置页。",
	)
	stats := computeKBProfileStats(chunks)
	if stats.SampleCount != 2 {
		t.Fatalf("SampleCount = %d, want 2", stats.SampleCount)
	}
	if stats.HeadingDensity <= 0 {
		t.Fatalf("HeadingDensity = %v, want > 0", stats.HeadingDensity)
	}
	if stats.RegulationRatio <= 0 {
		t.Fatalf("RegulationRatio = %v, want > 0", stats.RegulationRatio)
	}
	if stats.FAQRatio <= 0 {
		t.Fatalf("FAQRatio = %v, want > 0", stats.FAQRatio)
	}
}

func TestComputeKBProfileStatsEmpty(t *testing.T) {
	stats := computeKBProfileStats(nil)
	if stats.SampleCount != 0 || stats.AvgContentLen != 0 {
		t.Fatalf("empty sample stats = %+v, want zero value", stats)
	}
}

func TestClassifyKB(t *testing.T) {
	cases := []struct {
		name  string
		stats kbProfileStats
		want  string
	}{
		{
			"regulation",
			kbProfileStats{RegulationRatio: 0.20, HeadingDensity: 0.30, AvgContentLen: 400},
			types.KBClassRegulation,
		},
		{
			"faq",
			kbProfileStats{FAQRatio: 0.50, AvgContentLen: 120},
			types.KBClassFAQ,
		},
		{
			"paper",
			kbProfileStats{HeadingDensity: 0.35, AvgContentLen: 500, TableRatio: 0.10},
			types.KBClassPaper,
		},
		{
			"manual",
			kbProfileStats{HeadingDensity: 0.20, AvgContentLen: 300},
			types.KBClassManual,
		},
		{
			"general",
			kbProfileStats{HeadingDensity: 0.05, AvgContentLen: 100},
			types.KBClassGeneral,
		},
	}
	for _, c := range cases {
		if got := classifyKB(c.stats); got != c.want {
			t.Fatalf("%s: classifyKB() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestIsFAQChunk(t *testing.T) {
	if !isFAQChunk("如何重置密码？") {
		t.Fatalf("short question chunk not detected as FAQ")
	}
	if !isFAQChunk("Q: 如何重置密码?\nA: 打开设置页面，点击重置。" + strings.Repeat("补充说明。", 20)) {
		t.Fatalf("Q/A marker chunk not detected as FAQ")
	}
	if isFAQChunk(strings.Repeat("这是一段普通的产品手册正文，没有任何问答结构。", 10)) {
		t.Fatalf("plain prose chunk misdetected as FAQ")
	}
}
