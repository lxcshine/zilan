package docparser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/docreader/proto"
	"github.com/Tencent/WeKnora/internal/types"
)

// FR-21：新增的 table / formula 定位类型必须在解码时原样透传，
// decodeSourceBlock 只校验 Type 非空与 End > Start，不需要为新类型改动。
func TestDecodeSourceBlockPassesThroughCellLocators(t *testing.T) {
	blocks := decodeSourceBlocks([]*proto.SourceBlock{
		{
			Start: 10,
			End:   24,
			LocatorJson: `{"type":"table","page":7,"mapping":"exact",` +
				`"bbox":[0.1,0.2,0.8,0.25],"row_start":3,"row_end":3,` +
				`"col":2,"col_end":4,"table_id":"pdf:7:1",` +
				`"source_id":"table:pdf:7:1:3:2","quote":"| 螺栓 | 10 |"}`,
		},
		{
			Start: 30,
			End:   42,
			LocatorJson: `{"type":"formula","page":7,"mapping":"approx",` +
				`"bbox":[0.3,0.4,0.7,0.45],"source_id":"formula:7:9",` +
				`"quote":"E = mc^2"}`,
		},
	})

	require.Len(t, blocks, 2)
	table := blocks[0]
	assert.Equal(t, 10, table.Start)
	assert.Equal(t, 24, table.End)
	assert.Equal(t, types.SourceLocatorTable, table.Locator.Type)
	assert.Equal(t, 7, table.Locator.Page)
	assert.Equal(t, types.SourceMappingExact, table.Locator.Mapping)
	assert.Equal(t, []float64{0.1, 0.2, 0.8, 0.25}, table.Locator.BBox)
	assert.Equal(t, 3, table.Locator.RowStart)
	assert.Equal(t, 2, table.Locator.Col)
	assert.Equal(t, 4, table.Locator.ColEnd)
	assert.Equal(t, "pdf:7:1", table.Locator.TableID)
	assert.Equal(t, "table:pdf:7:1:3:2", table.Locator.SourceID)
	assert.Equal(t, types.SourcePrecisionCell, table.Locator.PrecisionLevel())

	formula := blocks[1]
	assert.Equal(t, types.SourceLocatorFormula, formula.Locator.Type)
	assert.Equal(t, types.SourceMappingApprox, formula.Locator.Mapping)
	assert.Equal(t, "formula:7:9", formula.Locator.SourceID)
	assert.Equal(t, "E = mc^2", formula.Locator.Quote)
	assert.Equal(t, types.SourcePrecisionRegion, formula.Locator.PrecisionLevel())
}

// 跨页合并的表格：两行同属一张表，但各自的原始页必须分别保留。
func TestDecodeSourceBlocksKeepsOriginalPageOfMergedRows(t *testing.T) {
	blocks := decodeSourceBlocks([]*proto.SourceBlock{
		{Start: 0, End: 12, LocatorJson: `{"type":"table","page":3,"mapping":"exact","row_start":2,"row_end":2,"col":1,"col_end":2,"table_id":"pdf:3:1","source_id":"table:pdf:3:1:2:1"}`},
		{Start: 13, End: 25, LocatorJson: `{"type":"table","page":4,"mapping":"exact","row_start":3,"row_end":3,"col":1,"col_end":2,"table_id":"pdf:3:1","source_id":"table:pdf:3:1:3:1"}`},
	})
	require.Len(t, blocks, 2)
	assert.Equal(t, blocks[0].Locator.TableID, blocks[1].Locator.TableID)
	assert.Equal(t, 3, blocks[0].Locator.Page)
	assert.Equal(t, 4, blocks[1].Locator.Page)
}

// 畸形/降级条目仍然按既有规则被丢弃，不会因为新类型而放宽。
func TestDecodeSourceBlockStillRejectsMalformed(t *testing.T) {
	_, ok := decodeSourceBlock(&proto.SourceBlock{Start: 5, End: 5, LocatorJson: `{"type":"table","page":1}`})
	assert.False(t, ok, "空区间被丢弃")

	_, ok = decodeSourceBlock(&proto.SourceBlock{Start: 0, End: 4, LocatorJson: `{"page":1}`})
	assert.False(t, ok, "缺 type 被丢弃")

	_, ok = decodeSourceBlock(&proto.SourceBlock{Start: 0, End: 4, LocatorJson: `not-json`})
	assert.False(t, ok, "locator 解析失败被丢弃")

	// 扫描件降级：只有页码、没有几何 —— 仍然透传，精度报为页级。
	sb, ok := decodeSourceBlock(&proto.SourceBlock{
		Start: 0, End: 4,
		LocatorJson: `{"type":"table","page":9,"mapping":"approx","row_start":1,"row_end":1}`,
	})
	require.True(t, ok)
	assert.Empty(t, sb.Locator.BBox)
	assert.Equal(t, types.SourcePrecisionRegion, sb.Locator.PrecisionLevel())
}
