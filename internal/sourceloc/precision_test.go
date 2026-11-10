package sourceloc

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestAlignIndexFindsDuplicateOccurrences(t *testing.T) {
	text := "同一句话出现两次 同一一句话 同一句话出现两次 同一句子"
	counts := map[string]int{"同一句话出现两次": 2}
	matches := indexNormalizedMatches(normalizeMarkdown(text).text, counts)
	require.Len(t, matches["同一句话出现两次"], 2)
}

// 单元文本在文档中出现两次时，两次出现都必须有对应 unit 才能放置；
// sheet 换行单元格会让表头文本重复，正是靠这一规则逐行对齐。
func TestAlignRepeatedUnitTextPlacesAll(t *testing.T) {
	units := []Unit{
		{Text: "编号 名称", Locator: types.SourceLocator{Type: "sheet", Sheet: "S", RowStart: 1, RowEnd: 1}},
		{Text: "编号 名称", Locator: types.SourceLocator{Type: "sheet", Sheet: "S", RowStart: 2, RowEnd: 2}},
		{Text: "1 螺栓", Locator: types.SourceLocator{Type: "sheet", Sheet: "S", RowStart: 3, RowEnd: 3}},
	}
	md := "## S\n| 编号 | 名称 |\n| --- | --- |\n| 编号 | 名称 |\n| 1 | 螺栓 |\n"
	blocks := Align(md, units)
	require.Len(t, blocks, 3)
	assert.Equal(t, 1, blocks[0].Locator.RowStart)
	assert.Equal(t, 2, blocks[1].Locator.RowStart)
	assert.Equal(t, 3, blocks[2].Locator.RowStart)
}

func TestNormalizeMarkdownUnescapeBoundaries(t *testing.T) {
	md := "1 &lt; 2 &amp;&amp; 3 &gt; 2 &nbsp; A&gt;B"
	norm := normalizeMarkdown(md)
	assert.Equal(t, "1<23>2a>b", norm.text)
}

// 列数少的表格也要能对齐（回归：仅靠前缀分桶时短行曾匹配失败）。
func TestAlignTableWithFewColumns(t *testing.T) {
	units := []Unit{
		{Text: "ID Name", Locator: types.SourceLocator{Type: "sheet", Sheet: "S", RowStart: 1, RowEnd: 1}},
		{Text: "1 Bolt", Locator: types.SourceLocator{Type: "sheet", Sheet: "S", RowStart: 2, RowEnd: 2}},
	}
	md := "| ID | Name |\n| --- | --- |\n| 1 | Bolt |\n"
	blocks := Align(md, units)
	require.Len(t, blocks, 2)
	assert.Equal(t, runeIndex(md, "| ID"), blocks[0].Start)
}

func TestIndexLocatorsMarksPartial(t *testing.T) {
	md := "有源段落\n\n无源段落"
	split := runeIndex(md, "无源段落")
	idx := NewIndex(md, []types.SourceBlock{
		{Start: 0, End: split, Locator: types.SourceLocator{Type: "text", Start: 0, End: split}},
	})
	locs := idx.Locators(0, len([]rune(md)))
	require.Len(t, locs, 1)
	assert.True(t, locs[0].Partial)
	assert.Equal(t, "有源段落", locs[0].Quote)
}

// 目录页复述章节标题时，标题的多次出现没有全部对应 unit，不能放置。
func TestAlignAnchorUniquenessGuardsTOC(t *testing.T) {
	units := []Unit{
		{Text: "第一章 引言", Locator: types.SourceLocator{Type: "docx", Block: 1}},
		{Text: "正文开始", Locator: types.SourceLocator{Type: "docx", Block: 2}},
	}
	md := "第一章 引言\n\n正文开始\n\n# 第一章 引言\n"
	blocks := Align(md, units)
	require.Len(t, blocks, 1)
	assert.Equal(t, 2, blocks[0].Locator.Block)
}

func TestRemapperUnicodeOffsets(t *testing.T) {
	oldText := "中文第一段\n\n中文第二段"
	newText := "中文第一段\n\n插入的新段\n\n中文第二段"
	r := NewRemapper(oldText, newText)
	assert.Equal(t, runeIndex(newText, "中文第二段"), r.Map(runeIndex(oldText, "中文第二段")))
}

func TestLocatorsAtBounds(t *testing.T) {
	idx := NewIndex("abc", []types.SourceBlock{{Start: 0, End: 3, Locator: types.SourceLocator{Type: "pdf", Page: 1}}})
	assert.Nil(t, idx.LocatorsAt(-1))
	require.Len(t, idx.LocatorsAt(1), 1)
	assert.Equal(t, "", idx.LocatorsAt(1)[0].Quote)
}

func TestMergeLocatorsEmptySides(t *testing.T) {
	assert.Nil(t, MergeLocators(nil, nil))
	locs := types.SourceLocators{{Type: "pdf", Page: 1}}
	assert.Equal(t, locs, MergeLocators(locs, nil))
	assert.Equal(t, locs, MergeLocators(nil, locs))
}

func TestCleanQuoteCapped(t *testing.T) {
	long := strings.Repeat("字", types.SourceLocatorQuoteMax+10)
	got := CleanQuote(long)
	assert.LessOrEqual(t, len([]rune(got)), types.SourceLocatorQuoteMax)
}

func TestSortBlocksStable(t *testing.T) {
	blocks := []types.SourceBlock{
		{Start: 5, Locator: types.SourceLocator{Page: 2}},
		{Start: 0, Locator: types.SourceLocator{Page: 1}},
		{Start: 5, Locator: types.SourceLocator{Page: 3}},
	}
	SortBlocks(blocks)
	assert.Equal(t, 0, blocks[0].Start)
	assert.Equal(t, 2, blocks[1].Locator.Page)
	assert.Equal(t, 3, blocks[2].Locator.Page)
}
