package sourceloc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

// FR-21：表格行与公式块各带自己的几何区域与 source_id，折叠会把整张表糊成
// 一个区域、把逐行的原始页归属一起丢掉，因此同型也不折叠。
func TestFoldIntoNeverFoldsTableCells(t *testing.T) {
	// 相邻两行、列区间相同、页码相同 —— 换成 sheet 类型就会被折叠。
	list := types.SourceLocators{{
		Type: types.SourceLocatorTable, Page: 3, Mapping: types.SourceMappingExact,
		RowStart: 2, RowEnd: 2, Col: 1, ColEnd: 4, TableID: "pdf:3:1",
		SourceID: "table:pdf:3:1:2:1", BBox: []float64{0, 0.5, 1, 0.55}, Quote: "| a |",
	}}
	list = AppendLocator(list, types.SourceLocator{
		Type: types.SourceLocatorTable, Page: 3, Mapping: types.SourceMappingExact,
		RowStart: 3, RowEnd: 3, Col: 1, ColEnd: 4, TableID: "pdf:3:1",
		SourceID: "table:pdf:3:1:3:1", BBox: []float64{0, 0.55, 1, 0.6}, Quote: "| b |",
	})
	require.Len(t, list, 2, "不同行不得合并")
	assert.Equal(t, 2, list[0].RowStart)
	assert.Equal(t, 3, list[1].RowStart)
	assert.Equal(t, []float64{0, 0.55, 1, 0.6}, list[1].BBox, "行级 bbox 各自保留")

	// 同一段文本被两个块覆盖（完全相同的定位）也不折叠：折叠会让 quote 拼接，
	// 而表格行的 quote 就是行文本，重复拼接只会污染引用证据。
	same := AppendLocator(list, list[1])
	require.Len(t, same, 3)
}

// 跨页合并的表格：同一张表里第 3 页与第 4 页的行必须分别可见。
func TestFoldIntoKeepsCrossPageTableRows(t *testing.T) {
	list := types.SourceLocators{{
		Type: types.SourceLocatorTable, Page: 3, Mapping: types.SourceMappingExact,
		RowStart: 2, RowEnd: 2, Col: 1, ColEnd: 2, TableID: "pdf:3:1",
		SourceID: "table:pdf:3:1:2:1",
	}}
	list = AppendLocator(list, types.SourceLocator{
		Type: types.SourceLocatorTable, Page: 4, Mapping: types.SourceMappingExact,
		RowStart: 3, RowEnd: 3, Col: 1, ColEnd: 2, TableID: "pdf:3:1",
		SourceID: "table:pdf:3:1:3:1",
	})
	require.Len(t, list, 2)
	assert.Equal(t, 3, list[0].Page)
	assert.Equal(t, 4, list[1].Page, "续页行的原始页归属不能丢")
}

func TestFoldIntoNeverFoldsFormulas(t *testing.T) {
	list := types.SourceLocators{{
		Type: types.SourceLocatorFormula, Page: 1, Mapping: types.SourceMappingExact,
		SourceID: "formula:1:4", BBox: []float64{0.1, 0.2, 0.5, 0.25}, Quote: "a^2",
	}}
	list = AppendLocator(list, types.SourceLocator{
		Type: types.SourceLocatorFormula, Page: 1, Mapping: types.SourceMappingExact,
		SourceID: "formula:1:9", BBox: []float64{0.1, 0.6, 0.5, 0.65}, Quote: "b^2",
	})
	require.Len(t, list, 2, "不同公式不得合并")
	assert.Equal(t, "a^2", list[0].Quote)
	assert.Equal(t, "b^2", list[1].Quote)
}

// 表格块与公式块、以及与 PDF 页块之间同样不折叠（混合类型拒绝）。
func TestFoldIntoRejectsTableFormulaMixedTypes(t *testing.T) {
	list := types.SourceLocators{{Type: types.SourceLocatorPDF, Page: 1}}
	list = AppendLocator(list, types.SourceLocator{
		Type: types.SourceLocatorTable, Page: 1, RowStart: 1, RowEnd: 1, Col: 1, ColEnd: 2,
	})
	list = AppendLocator(list, types.SourceLocator{
		Type: types.SourceLocatorFormula, Page: 1, BBox: []float64{0, 0, 1, 1},
	})
	require.Len(t, list, 3)
}

// 合并检索结果时，列区间与表 ID 参与去重判定：同页同行但不同列区间不是同一处。
func TestMergeLocatorsDistinguishesColumnsAndTables(t *testing.T) {
	base := types.SourceLocator{
		Type: types.SourceLocatorTable, Page: 3, Mapping: types.SourceMappingExact,
		RowStart: 2, RowEnd: 2, TableID: "pdf:3:1", SourceID: "table:pdf:3:1:2:1",
		Quote: "| a |",
	}
	same := base
	otherCol := base
	otherCol.Col, otherCol.ColEnd = 1, 2
	otherTable := base
	otherTable.TableID, otherTable.SourceID = "pdf:3:2", "table:pdf:3:2:2:1"

	assert.Len(t, MergeLocators(
		types.SourceLocators{base}, types.SourceLocators{same},
	), 1, "完全相同的定位只保留一份")
	assert.Len(t, MergeLocators(
		types.SourceLocators{base}, types.SourceLocators{otherCol},
	), 2, "列区间不同视为不同定位")
	assert.Len(t, MergeLocators(
		types.SourceLocators{base}, types.SourceLocators{otherTable},
	), 2, "不同表视为不同定位")
}

// 单元格级块进入 Index 后，逐行 quote 与逐行 locator 一一对应。
func TestIndexLocatorsKeepsOneEntryPerTableRow(t *testing.T) {
	md := "前言\n\n| 名称 | 数量 |\n| --- | --- |\n| 螺栓 | 10 |\n| 螺母 | 20 |\n"
	rowStart := runeIndex(md, "| 螺栓")
	rowEnd := runeIndex(md, "| 螺母")
	next := runeIndex(md, "| 螺母") + len([]rune("| 螺母 | 20 |"))
	idx := NewIndex(md, []types.SourceBlock{
		{Start: 0, End: runeIndex(md, "\n\n|"), Locator: types.SourceLocator{
			Type: types.SourceLocatorPDF, Page: 1, Mapping: types.SourceMappingExact,
		}},
		{Start: rowStart, End: rowEnd, Locator: types.SourceLocator{
			Type: types.SourceLocatorTable, Page: 1, Mapping: types.SourceMappingExact,
			RowStart: 2, RowEnd: 2, Col: 1, ColEnd: 2, TableID: "pdf:1:1",
			SourceID: "table:pdf:1:1:2:1", BBox: []float64{0, 0.5, 1, 0.55},
		}},
		{Start: rowEnd, End: next, Locator: types.SourceLocator{
			Type: types.SourceLocatorTable, Page: 2, Mapping: types.SourceMappingExact,
			RowStart: 3, RowEnd: 3, Col: 1, ColEnd: 2, TableID: "pdf:1:1",
			SourceID: "table:pdf:1:1:3:1", BBox: []float64{0, 0.6, 1, 0.65},
		}},
	})

	locs := idx.Locators(rowStart, next)
	require.Len(t, locs, 2)
	assert.Equal(t, types.SourceLocatorTable, locs[0].Type)
	assert.Equal(t, "| 螺栓 | 10 |", locs[0].Quote)
	assert.Equal(t, types.SourcePrecisionCell, locs[0].PrecisionLevel())
	assert.Equal(t, 2, locs[1].Page, "第二行来自续页")
	assert.Equal(t, 3, locs[1].RowStart)
}
