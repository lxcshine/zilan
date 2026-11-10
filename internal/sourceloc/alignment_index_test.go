package sourceloc

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

// 长单元键（>= alignIndexPrefixBytes 字节）走前缀分桶路径。
func TestAlignIndexLongKeyBucket(t *testing.T) {
	long := strings.Repeat("数据", 20) // 40 字节 >= 32
	other := strings.Repeat("其他", 20)
	text := long + " 分隔 " + other
	counts := map[string]int{normalizeKey(long): 1}
	matches := indexNormalizedMatches(normalizeMarkdown(text).text, counts)
	require.Len(t, matches[normalizeKey(long)], 1)
	assert.Equal(t, 0, matches[normalizeKey(long)][0])

	blocks := Align(text, []Unit{{Text: long, Locator: types.SourceLocator{Type: "docx", Block: 1}}})
	require.Len(t, blocks, 1)
	assert.Equal(t, 1, blocks[0].Locator.Block)
	assert.Equal(t, 0, blocks[0].Start)
}

// 数字紧贴时不得把两个不同的数值粘成一个键（边界检查）。
func TestNumericBoundaryRejected(t *testing.T) {
	text := "版本 1.23 和 1.234"
	matches := indexNormalizedMatches(normalizeMarkdown(text).text, map[string]int{"123": 1})
	// "1.23" 中含 "123" 吗？不会：'.' 在数字之间被保留，"1.23" 归一化为 "1.23"，
	// 因此 "123" 不匹配 "1.23"；"1.234" 中的 "234" 亦不匹配 "123"。
	require.Empty(t, matches["123"])
}

func TestFoldIntoSlideAndSection(t *testing.T) {
	list := types.SourceLocators{{Type: "slide", Slide: 1}}
	list = AppendLocator(list, types.SourceLocator{Type: "slide", Slide: 1, Quote: "x"})
	require.Len(t, list, 1)
	assert.Equal(t, "x", list[0].Quote)

	list = AppendLocator(list, types.SourceLocator{Type: "slide", Slide: 2})
	require.Len(t, list, 2)

	secs := types.SourceLocators{{Type: "section", Section: 3}}
	secs = AppendLocator(secs, types.SourceLocator{Type: "section", Section: 4})
	require.Len(t, secs, 2)
}

func TestFoldIntoSheetRejectsFarRows(t *testing.T) {
	list := types.SourceLocators{{Type: "sheet", Sheet: "S", RowStart: 1, RowEnd: 2}}
	list = AppendLocator(list, types.SourceLocator{Type: "sheet", Sheet: "S", RowStart: 5, RowEnd: 6})
	require.Len(t, list, 2)
}

func TestFoldIntoRejectsMixedTypes(t *testing.T) {
	list := types.SourceLocators{{Type: "pdf", Page: 1}}
	list = AppendLocator(list, types.SourceLocator{Type: "docx", Block: 1})
	require.Len(t, list, 2)

	bboxes := types.SourceLocators{{Type: "pdf", Page: 1, BBox: []float64{0, 0, 1, 1}}}
	bboxes = AppendLocator(bboxes, types.SourceLocator{Type: "pdf", Page: 1})
	require.Len(t, bboxes, 2, "已有 bbox 的定位不再折叠")
}

// 图片引用重写为本地 URL 后，RemapBlocks 依据块内文本重新对齐。
func TestRemapBlocksImageURLRewrite(t *testing.T) {
	oldText := "前文\n\n![图](images/a.png)\n\n后文"
	newText := "前文\n\n![图](local://1/images/0123456789abcdef.png)\n\n后文"
	blocks := []types.SourceBlock{
		{Start: 0, End: runeIndex(oldText, "![图]"), Locator: types.SourceLocator{Type: "pdf", Page: 1}},
		{Start: runeIndex(oldText, "![图]"), End: len([]rune(oldText)), Locator: types.SourceLocator{Type: "pdf", Page: 2}},
	}
	out := RemapBlocks(blocks, oldText, newText)
	require.Len(t, out, 2)
	assert.Equal(t, 1, out[0].Locator.Page)
	assert.Equal(t, 2, out[1].Locator.Page)
	assert.Equal(t, runeIndex(newText, "![图]"), out[1].Start)
}

func TestRemapBlocksSameTextPassthrough(t *testing.T) {
	blocks := []types.SourceBlock{{Start: 1, End: 4, Locator: types.SourceLocator{Type: "text", Start: 1, End: 4}}}
	out := RemapBlocks(blocks, "abcdef", "abcdef")
	require.Len(t, out, 1)
	assert.Equal(t, 1, out[0].Start)
}

// 行数相等的改写区间按行一一对应，避免整段插值漂移。
func TestRemapperLineForLineGap(t *testing.T) {
	oldText := "A\n旧1\n旧2\nB"
	newText := "A\n新1\n新2\nB"
	r := NewRemapper(oldText, newText)
	// 锚点 A、B 唯一；中间两行从两端 fillGap 配不上，行数相等按行映射。
	assert.Equal(t, 0, r.Map(0))
	assert.Equal(t, len([]rune(newText)), r.Map(len([]rune(oldText))))
	assert.Equal(t, 3, r.Map(3), "旧行内的偏移落在对应新行内")
}

func TestSplitLinesCRLF(t *testing.T) {
	keys, starts, total := splitLines("a\r\nb\n")
	require.Equal(t, []string{"a", "b", ""}, keys)
	require.Equal(t, []int{0, 3, 5}, starts)
	assert.Equal(t, 5, total)
}
