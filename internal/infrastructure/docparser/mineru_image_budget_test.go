package docparser

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

// FR-14 ② 图像预算：单文档图像数、单图像素、单图长边，以及重复图像
// （页眉页脚 logo）识别与去重。字节类上界由 zip_limits_test.go 覆盖。

func TestImageBudgetEnvOverrides(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		t.Setenv(maxImagesPerDocEnv, "")
		t.Setenv(maxImagePixelsEnv, "")
		t.Setenv(maxImageLongEdgeEnv, "")
		assert.Equal(t, defaultMaxImagesPerDoc, maxImagesPerDoc())
		assert.Equal(t, defaultMaxImagePixels, maxImagePixels())
		assert.Equal(t, defaultMaxImageLongEdge, maxImageLongEdge())
	})
	t.Run("valid", func(t *testing.T) {
		t.Setenv(maxImagesPerDocEnv, "12")
		t.Setenv(maxImagePixelsEnv, "34")
		t.Setenv(maxImageLongEdgeEnv, "56")
		assert.Equal(t, 12, maxImagesPerDoc())
		assert.Equal(t, int64(34), maxImagePixels())
		assert.Equal(t, 56, maxImageLongEdge())
	})
	// 非法值必须回落到默认值，配置错误不能被静默放大成"没有限制"。
	t.Run("invalid", func(t *testing.T) {
		for _, value := range []string{"abc", "0", "-1", "1.5"} {
			t.Setenv(maxImagesPerDocEnv, value)
			assert.Equal(t, defaultMaxImagesPerDoc, maxImagesPerDoc(), value)
		}
	})
}

func TestExtractMarkdownZipRejectsTooManyImages(t *testing.T) {
	t.Setenv(maxImagesPerDocEnv, "1")
	zipData := buildMinerUZip(t, map[string]string{
		"full.md":           "![](images/a.png)\n![](images/b.png)\n",
		"images/a.png":      "aaa",
		"images/b.png":      "bbb",
		"content_list.json": "[]",
	})

	_, _, err := extractMarkdownZipWithLayout(zipData, "test", maxZipImagesTotalBytes(), nil)
	require.True(t, errors.Is(err, errResultPackageTooLarge), "err = %v", err)
	assert.True(t, IsMinerUResultTooLarge(err), "budget violations must be distinguishable from network failures")
	for _, want := range []string{"more than 1 images", maxImagesPerDocEnv} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestExtractMarkdownZipRejectsOversizedImages(t *testing.T) {
	big := createTestPNG(100, 100) // 10_000 像素，长边 100
	wide := createTestPNG(300, 10) // 长边 300

	t.Run("pixels", func(t *testing.T) {
		t.Setenv(maxImagePixelsEnv, "9999")
		t.Setenv(maxImageLongEdgeEnv, "")
		zipData := buildMinerUZip(t, map[string]string{
			"full.md":      "![](images/a.png)\n",
			"images/a.png": string(big),
		})
		_, _, err := extractMarkdownZip(zipData, "test")
		require.True(t, errors.Is(err, errResultPackageTooLarge), "err = %v", err)
		for _, want := range []string{"10000 pixels", "9999 pixels", maxImagePixelsEnv} {
			assert.Contains(t, err.Error(), want)
		}
	})

	t.Run("long edge", func(t *testing.T) {
		t.Setenv(maxImagePixelsEnv, "")
		t.Setenv(maxImageLongEdgeEnv, "200")
		zipData := buildMinerUZip(t, map[string]string{
			"full.md":      "![](images/w.png)\n",
			"images/w.png": string(wide),
		})
		_, _, err := extractMarkdownZip(zipData, "test")
		require.True(t, errors.Is(err, errResultPackageTooLarge), "err = %v", err)
		for _, want := range []string{"300px long edge", "200px", maxImageLongEdgeEnv} {
			assert.Contains(t, err.Error(), want)
		}
	})

	// 量不出尺寸的格式（例如 svg）放行：不能因为读不到大小就丢正文插图。
	t.Run("unknown format passes", func(t *testing.T) {
		t.Setenv(maxImagePixelsEnv, "1")
		t.Setenv(maxImageLongEdgeEnv, "1")
		zipData := buildMinerUZip(t, map[string]string{
			"full.md":      "![](images/a.svg)\n",
			"images/a.svg": "<svg/>",
		})
		_, refs, err := extractMarkdownZip(zipData, "test")
		require.NoError(t, err)
		assert.Len(t, refs, 1)
	})
}

// 同一份图像字节出现在半数及以上页面时判定为页眉页脚 logo，整组丢弃；
// 规则与 docreader/parser/pdf_parser.py 的 _select_embedded_images 一致。
func TestExtractMarkdownZipDropsRepeatedHeaderFooterLogos(t *testing.T) {
	logo := "identical-logo-bytes"
	zipData := buildMinerUZip(t, map[string]string{
		"full.md": strings.Join([]string{
			"![](images/logo_left.png)",
			"![](images/logo_right.png)",
			"![](images/figure.png)",
			"正文内容",
		}, "\n"),
		"images/logo_left.png":  logo,
		"images/logo_right.png": logo,
		"images/figure.png":     "unique-figure-bytes",
	})
	layout := []byte(`[{"page_idx":0},{"page_idx":1},{"page_idx":2},{"page_idx":3}]`)
	require.Equal(t, 4, minerULayoutPageCount(layout))

	_, refs, err := extractMarkdownZipWithLayout(zipData, "test", maxZipImagesTotalBytes(), layout)
	require.NoError(t, err)
	require.Len(t, refs, 1, "the two identical logos must be dropped, the figure kept")
	assert.Equal(t, "images/figure.png", refs[0].OriginalRef)

	// 页数未知（layout 缺失）时跳过该规则：按猜测丢图比多留几张更糟。
	_, refs, err = extractMarkdownZipWithLayout(zipData, "test", maxZipImagesTotalBytes(), nil)
	require.NoError(t, err)
	assert.Len(t, refs, 3)
}

// 同一行里的多种写法（markdown 语法 + 内联 HTML src）只代表一页，
// 否则一张正文插图会被误判成页眉页脚 logo 而丢掉。
func TestMinerUImageLineOccurrences(t *testing.T) {
	md := "![](images/a.png) <img src=\"images/a.png\">\n![](images/a.png)\n![](images/b.png)\n"
	counts := minerUImageLineOccurrences(md)
	assert.Equal(t, 2, counts["images/a.png"])
	assert.Equal(t, 1, counts["images/b.png"])
	assert.NotContains(t, counts, "images/missing.png")
}

func TestFilterMinerURepeatedImages(t *testing.T) {
	keep := func(name string) minerUImageCandidate {
		return minerUImageCandidate{
			Refs:        []types.ImageRef{{OriginalRef: name}},
			Occurrences: 1,
			Hash:        name,
		}
	}
	// pages <= 0：页数未知，全部保留。
	assert.Len(t, filterMinerURepeatedImages([]minerUImageCandidate{keep("a"), keep("a")}, "t", 0), 2)

	// 10 页文档：阈值 = ceil(10*0.5) = 5，出现 5 次的丢弃，4 次的保留。
	candidates := make([]minerUImageCandidate, 0, 9)
	for i := 0; i < 5; i++ {
		candidates = append(candidates, keep("logo"))
	}
	for i := 0; i < 4; i++ {
		candidates = append(candidates, keep("figure"))
	}
	refs := filterMinerURepeatedImages(candidates, "t", 10)
	require.Len(t, refs, 4)
	assert.Equal(t, "figure", refs[0].OriginalRef)

	// 短文档不会误杀：2 页文档的阈值下限是 2，出现 1 次的正文插图保留。
	assert.Len(t, filterMinerURepeatedImages([]minerUImageCandidate{keep("a")}, "t", 2), 1)
}

// legacy /file_parse 的内联 base64 图片走同一套预算与去重规则。
func TestProcessImagesEnforcesBudgetAndDedup(t *testing.T) {
	reader := &MinerUReader{}
	png := createTestPNG(20, 20)
	b64 := base64.StdEncoding.EncodeToString(png)
	md := "![](images/a.png)\n![](images/b.png)\n"

	t.Run("count limit", func(t *testing.T) {
		t.Setenv(maxImagesPerDocEnv, "1")
		_, _, err := reader.processImages(md, map[string]string{
			"a.png": b64, "b.png": b64,
		}, 0)
		require.True(t, errors.Is(err, errResultPackageTooLarge), "err = %v", err)
		assert.True(t, IsMinerUResultTooLarge(err))
	})

	t.Run("pixel limit", func(t *testing.T) {
		t.Setenv(maxImagePixelsEnv, "10")
		_, _, err := reader.processImages(md, map[string]string{"a.png": b64}, 0)
		require.True(t, errors.Is(err, errResultPackageTooLarge), "err = %v", err)
		assert.Contains(t, err.Error(), maxImagePixelsEnv)
	})

	t.Run("total bytes limit", func(t *testing.T) {
		t.Setenv(zipImagesTotalLimitEnv, "1")
		_, _, err := reader.processImages(md, map[string]string{"a.png": b64}, 0)
		require.True(t, errors.Is(err, errResultPackageTooLarge), "err = %v", err)
		assert.Contains(t, err.Error(), zipImagesTotalLimitEnv)
	})

	t.Run("repeated logo dropped", func(t *testing.T) {
		t.Setenv(maxImagesPerDocEnv, "")
		t.Setenv(maxImagePixelsEnv, "")
		// 4 页文档，阈值 2：两份同字节的图各出现一次即达到阈值。
		refs, _, err := reader.processImages(md, map[string]string{"a.png": b64, "b.png": b64}, 4)
		require.NoError(t, err)
		assert.Empty(t, refs, "identical bytes on half the pages are header/footer logos")
	})
}
