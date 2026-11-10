package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FR-21：解析器上报的表格/公式定位 JSON 必须无损落到 SourceLocator 上，
// 否则单元格级溯源会在解码这一步就被悄悄丢掉。
func TestSourceLocatorTableCellJSONRoundTrip(t *testing.T) {
	raw := `{"type":"table","page":7,"mapping":"exact","bbox":[0.1,0.2,0.8,0.25],` +
		`"row_start":3,"row_end":3,"col":2,"col_end":4,"table_id":"pdf:7:1",` +
		`"source_id":"table:pdf:7:1:3:2","quote":"| 螺栓 | 10 | 1.5 |"}`

	var loc SourceLocator
	require.NoError(t, json.Unmarshal([]byte(raw), &loc))
	assert.Equal(t, SourceLocatorTable, loc.Type)
	assert.Equal(t, 7, loc.Page)
	assert.Equal(t, SourceMappingExact, loc.Mapping)
	assert.Equal(t, []float64{0.1, 0.2, 0.8, 0.25}, loc.BBox)
	assert.Equal(t, 3, loc.RowStart)
	assert.Equal(t, 3, loc.RowEnd)
	assert.Equal(t, 2, loc.Col)
	assert.Equal(t, 4, loc.ColEnd)
	assert.Equal(t, "pdf:7:1", loc.TableID)
	assert.Equal(t, "table:pdf:7:1:3:2", loc.SourceID)

	encoded, err := json.Marshal(loc)
	require.NoError(t, err)
	var again SourceLocator
	require.NoError(t, json.Unmarshal(encoded, &again))
	assert.Equal(t, loc, again)
	assert.Contains(t, string(encoded), `"col":2`)
	assert.Contains(t, string(encoded), `"col_end":4`)
	assert.Contains(t, string(encoded), `"table_id":"pdf:7:1"`)
}

func TestSourceLocatorFormulaJSONRoundTrip(t *testing.T) {
	raw := `{"type":"formula","page":2,"mapping":"approx","bbox":[0.3,0.4,0.7,0.45],` +
		`"source_id":"formula:2:9","quote":"\\sum x_i \\geq 0"}`

	var loc SourceLocator
	require.NoError(t, json.Unmarshal([]byte(raw), &loc))
	assert.Equal(t, SourceLocatorFormula, loc.Type)
	assert.Equal(t, SourceMappingApprox, loc.Mapping)
	// JSON 里的 \\ 解出来是一个反斜杠：LaTeX 命令必须原样保住。
	assert.Equal(t, `\sum x_i \geq 0`, loc.Quote)

	encoded, err := json.Marshal(loc)
	require.NoError(t, err)
	// 零值字段不落 JSON：公式没有行列号，序列化结果里不该出现 col/row_start。
	assert.NotContains(t, string(encoded), `"col"`)
	assert.NotContains(t, string(encoded), `"row_start"`)
	assert.NotContains(t, string(encoded), `"table_id"`)
}

// SourceLocators 作为 JSON 列存取，新增字段必须跟着一起往返。
func TestSourceLocatorsScanValueKeepsCellFields(t *testing.T) {
	in := SourceLocators{
		{Type: SourceLocatorTable, Page: 1, Mapping: SourceMappingExact,
			RowStart: 2, RowEnd: 2, Col: 1, ColEnd: 3, TableID: "pdf:1:1",
			BBox: []float64{0, 0.5, 1, 0.6}},
		{Type: SourceLocatorFormula, Page: 1, Mapping: SourceMappingApprox},
	}
	value, err := in.Value()
	require.NoError(t, err)
	require.NotNil(t, value)

	var out SourceLocators
	require.NoError(t, out.Scan(value))
	require.Len(t, out, 2)
	assert.Equal(t, in, out)
	assert.Equal(t, 3, out[0].ColEnd)
	assert.Equal(t, "pdf:1:1", out[0].TableID)
}

// 精度等级同时看 mapping 与 bbox：PDF 解析器的页级回退块也是 "exact"，
// 只看 mapping 会把「只有页码」误报成精确区域。
func TestPrecisionLevelThreeTiers(t *testing.T) {
	cases := []struct {
		name string
		loc  SourceLocator
		want string
	}{
		{"表格行有几何且精确对齐", SourceLocator{
			Type: SourceLocatorTable, Page: 3, Mapping: SourceMappingExact,
			RowStart: 2, RowEnd: 2, Col: 1, ColEnd: 4, BBox: []float64{0, 0, 1, 0.1},
		}, SourcePrecisionCell},
		{"工作表行号（无几何但结构精确）", SourceLocator{
			Type: SourceLocatorSheet, Sheet: "S", RowStart: 5, RowEnd: 6,
		}, SourcePrecisionCell},
		{"扫描件表格行降级为区域", SourceLocator{
			Type: SourceLocatorTable, Page: 3, Mapping: SourceMappingApprox,
			RowStart: 2, RowEnd: 2, BBox: []float64{0, 0, 1, 0.4},
		}, SourcePrecisionRegion},
		{"公式裁剪区域", SourceLocator{
			Type: SourceLocatorFormula, Page: 3, Mapping: SourceMappingExact,
			BBox: []float64{0.2, 0.3, 0.8, 0.35},
		}, SourcePrecisionRegion},
		{"PDF 段落区域", SourceLocator{
			Type: SourceLocatorPDF, Page: 3, Mapping: SourceMappingExact,
			BBox: []float64{0.2, 0.3, 0.8, 0.35}, SourceID: "pdf:3:1:9",
		}, SourcePrecisionRegion},
		{"PDF 页级回退块（exact 但无 bbox）", SourceLocator{
			Type: SourceLocatorPDF, Page: 3, Mapping: SourceMappingExact,
		}, SourcePrecisionPage},
		{"整页渲染图", SourceLocator{
			Type: SourceLocatorPDF, Page: 1,
		}, SourcePrecisionPage},
		{"幻灯片", SourceLocator{Type: SourceLocatorSlide, Slide: 2}, SourcePrecisionPage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, c.loc.PrecisionLevel())
		})
	}
}
