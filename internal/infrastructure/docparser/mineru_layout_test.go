package docparser

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FR-14 ① 的布局解析测试：区域类型 + 归一化几何（0-1 bbox）+ 页码必须
// 完整保留到 types.SourceBlock，供下游切分（FR-05）与溯源（FR-21）使用。

// middleFixture 是 MiddleJson 2.0 的契约样本：bbox 是 [0,1] 归一化分数，
// page_idx 是原始页号，caption 是子块。刻意不是 legacy content_list 结构。
const middleFixture = `{
  "schema": "docvortex.middle",
  "schema_version": "2.0",
  "is_full_document": true,
  "metadata": {
    "file_suffix": "pdf",
    "producer": {
      "name": "mineru",
      "version": "4.0.0"
    }
  },
  "pages": [
    {
      "page_idx": 4,
      "blocks": [
        {
          "type": "text",
          "index": 0,
          "bbox": [0.1, 0.2, 0.9, 0.3],
          "content": [
            {"type": "text", "content": "设备"},
            {
              "type": "hyperlink",
              "url": "https://example.org",
              "content": [{"type": "text", "content": "验收合格"}]
            }
          ]
        },
        {
          "type": "table",
          "index": 1,
          "bbox": [0.1, 0.4, 0.9, 0.8],
          "content": [
            {
              "type": "table_caption",
              "bbox": [0.1, 0.4, 0.9, 0.45],
              "content": [{"type": "text", "content": "设备参数表"}]
            },
            {
              "type": "table_body",
              "bbox": [0.1, 0.46, 0.9, 0.8],
              "content": "<table><tr><td>限值</td><td>1.5毫米</td></tr></table>"
            }
          ]
        }
      ]
    }
  ]
}`

func TestMinerUMiddleNormalizedGeometryAndNestedBlocks(t *testing.T) {
	blocks := minerUSourceBlocks("设备验收合格\n\n设备参数表\n\n|限值|1.5毫米|", []byte(middleFixture), "pdf")
	require.Len(t, blocks, 3)
	require.Equal(t, 5, blocks[0].Locator.Page)
	require.Equal(t, []float64{0.1, 0.2, 0.9, 0.3}, blocks[0].Locator.BBox)
	require.Equal(t, "mineru:2:4:0:text", blocks[0].Locator.SourceID)
	require.Equal(t, "exact", blocks[0].Locator.Mapping)
	require.Equal(t, []float64{0.1, 0.46, 0.9, 0.8}, blocks[2].Locator.BBox)
	// 区域类型（FR-14 ①）随 SourceID 一起保留，表格子块归到 table 族。
	require.Equal(t, "mineru:2:4:1/0:table", blocks[1].Locator.SourceID)
	require.Equal(t, "mineru:2:4:1/1:table", blocks[2].Locator.SourceID)
}

// 区域类型必须归一成一组稳定标识，未知类型也不能丢。
func TestMinerURegionKind(t *testing.T) {
	for blockType, want := range map[string]string{
		"text": "text", "Text": "text", "hyperlink": "text",
		"table_caption": "table", "table_body": "table", "TABLE": "table",
		"image_footnote": "image", "interline_equation": "equation",
		"list_item": "list", "index": "list", "code_body": "code",
		"chart_body": "chart", "title": "title",
		"":                 "",
		"brand_new_region": "brand_new_region",
		"weird type!":      "weirdtype",
	} {
		assert.Equal(t, want, minerURegionKind(blockType), blockType)
	}
	assert.Equal(t, "ref:text", minerURegionSourceID("ref", "text"))
	assert.Equal(t, "ref", minerURegionSourceID("ref", ""), "类型缺失时保持原 ID")
}

func TestMinerUMiddleZIPPreferredToLegacy(t *testing.T) {
	zipData := buildMinerUZip(t, map[string]string{
		"markdown.md":           "设备验收合格",
		"middle_json.json":      middleFixture,
		"doc_content_list.json": "[]",
	})
	require.JSONEq(t, middleFixture, string(minerUContentListFromZip(zipData)))
}

func TestMinerUMiddleRejectsUnknownSchemaAndInvalidIndices(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"unknown schema":   func(d map[string]any) { d["schema"] = "docvortex.model" },
		"unknown version":  func(d map[string]any) { d["schema_version"] = "9.0" },
		"missing page_idx": func(d map[string]any) { delete(d["pages"].([]any)[0].(map[string]any), "page_idx") },
		"negative page":    func(d map[string]any) { d["pages"].([]any)[0].(map[string]any)["page_idx"] = -1 },
		// 页/块 index 必须严格递增：MinerU 按阅读顺序输出，违反即说明这份
		// 布局不描述我们拿到的 markdown，整体放弃优于把引用指到错误的页。
		"page index not increasing": func(d map[string]any) {
			pages := d["pages"].([]any)
			d["pages"] = []any{pages[0], pages[0]}
		},
		"block index not increasing": func(d map[string]any) {
			blocks := d["pages"].([]any)[0].(map[string]any)["blocks"].([]any)
			blocks[1].(map[string]any)["index"] = 0
		},
		"missing block index": func(d map[string]any) {
			blocks := d["pages"].([]any)[0].(map[string]any)["blocks"].([]any)
			delete(blocks[0].(map[string]any), "index")
		},
	} {
		t.Run(name, func(t *testing.T) {
			var d map[string]any
			require.NoError(t, json.Unmarshal([]byte(middleFixture), &d))
			mutate(d)
			raw, err := json.Marshal(d)
			require.NoError(t, err)
			require.Empty(t, minerUSourceBlocks("设备验收合格\n\n设备参数表\n\n|限值|1.5毫米|", raw, "pdf"))
		})
	}
}

// 没有自身几何的子块必须继承父块的 bbox：区域信息一旦丢失，溯源就只能退化
// 到整页。
func TestMinerUMiddleChildInheritsParentBox(t *testing.T) {
	raw := `{
	  "schema": "docvortex.middle",
	  "schema_version": "2.0",
	  "pages": [{
	    "page_idx": 0,
	    "blocks": [{
	      "type": "list",
	      "index": 0,
	      "bbox": [0.2, 0.3, 0.8, 0.9],
	      "content": [{"type": "list_item", "content": [{"type": "text", "content": "第一条列表项"}]}]
	    }]
	  }]
	}`
	blocks := minerUSourceBlocks("第一条列表项", []byte(raw), "pdf")
	require.Len(t, blocks, 1)
	require.Equal(t, []float64{0.2, 0.3, 0.8, 0.9}, blocks[0].Locator.BBox)
	require.Equal(t, 1, blocks[0].Locator.Page)
	require.Equal(t, "mineru:2:0:0/0:list", blocks[0].Locator.SourceID)
}

// 越界（非 [0,1]）或退化的 bbox 一律丢弃，而不是画到错误的位置。
func TestMinerUMiddleRejectsOutOfRangeBox(t *testing.T) {
	raw := `{
	  "schema": "docvortex.middle",
	  "schema_version": "2.0",
	  "pages": [{
	    "page_idx": 0,
	    "blocks": [{
	      "type": "text",
	      "index": 0,
	      "bbox": [0, 0, 900, 600],
	      "content": [{"type": "text", "content": "旧单位坐标"}]
	    }]
	  }]
	}`
	blocks := minerUSourceBlocks("旧单位坐标", []byte(raw), "pdf")
	require.Len(t, blocks, 1)
	require.Empty(t, blocks[0].Locator.BBox)
	require.Equal(t, 1, blocks[0].Locator.Page)
}

func TestMinerULegacyMissingPageIsNotPageOne(t *testing.T) {
	require.Empty(t, minerUSourceBlocks("设备验收合格", []byte(`[{"type":"text","text":"设备验收合格"}]`), "pdf"))
}

// legacy content_list 的 bbox 是 0-1000 归一化，必须换算成 0-1 分数。
func TestMinerULegacyContentListNormalizesBBox(t *testing.T) {
	list := `[
	  {"type":"text","text":"第一段","page_idx":0,"bbox":[100,200,900,300]},
	  {"type":"text","text":"第二段","page_idx":2,"bbox":[0,0,1200,300]}
	]`
	blocks := minerUSourceBlocks("第一段\n\n第二段", []byte(list), "pdf")
	require.Len(t, blocks, 2)
	require.Equal(t, 1, blocks[0].Locator.Page)
	require.Equal(t, []float64{0.1, 0.2, 0.9, 0.3}, blocks[0].Locator.BBox)
	require.Equal(t, "mineru:legacy:0:0:text", blocks[0].Locator.SourceID)
	// 越界的 bbox 被丢弃，但块本身（页码）仍然保留。
	require.Equal(t, 3, blocks[1].Locator.Page)
	require.Empty(t, blocks[1].Locator.BBox)
}

// 部分 MinerU 版本把 content_list 作为 JSON 字符串返回，同样要能解析。
func TestMinerUContentListAcceptsStringWrappedList(t *testing.T) {
	raw, err := json.Marshal(`[{"type":"text","text":"字符串包裹","page_idx":1,"bbox":[0,0,500,500]}]`)
	require.NoError(t, err)
	blocks := minerUSourceBlocks("字符串包裹", raw, "pdf")
	require.Len(t, blocks, 1)
	require.Equal(t, 2, blocks[0].Locator.Page)
	require.Equal(t, []float64{0, 0, 0.5, 0.5}, blocks[0].Locator.BBox)
}

// Office 文件在 MinerU 内部要先转成中间 PDF，其页号不描述用户上传的文件，
// 因此不产出 SourceBlock（交给结构对齐器）。
func TestMinerUSourceBlocksSkipsOfficeFiles(t *testing.T) {
	list := `[{"type":"text","text":"设备验收合格","page_idx":0,"bbox":[0,0,500,500]}]`
	for _, ft := range []string{"docx", "pptx", "xlsx", ""} {
		require.Empty(t, minerUSourceBlocks("设备验收合格", []byte(list), ft), ft)
	}
	for _, ft := range []string{"pdf", "PDF", ".png", "jpg"} {
		require.Len(t, minerUSourceBlocks("设备验收合格", []byte(list), ft), 1, ft)
	}
}

func TestMinerUBoxesRejectNonFiniteAndWrongUnits(t *testing.T) {
	for _, box := range [][]float64{{0, 0, math.NaN(), 1}, {0, 0, math.Inf(1), 1}, {0, 0, 900, 600}, {.9, 0, .1, 1}} {
		require.False(t, normalizedMinerUBox(box))
	}
	_, ok := minerUBBox([]float64{0, 0, math.NaN(), 100})
	require.False(t, ok)
	_, ok = minerUBBox([]float64{0, 0, 1001, 100})
	require.False(t, ok, "content_list bbox beyond the 0-1000 range must be dropped")
}

func TestMinerULegacyZIPWithUnsupportedMiddleKeepsContentList(t *testing.T) {
	legacy := `[{"type":"text","text":"设备验收合格","page_idx":1,"bbox":[100,200,900,300]}]`
	zipData := buildMinerUZip(t, map[string]string{
		"doc_middle_json.json":  `{"pdf_info":[]}`,
		"doc_content_list.json": legacy,
	})
	blocks := minerUSourceBlocks("设备验收合格", minerUContentListFromZip(zipData), "pdf")
	require.Len(t, blocks, 1)
	require.Equal(t, 2, blocks[0].Locator.Page)
	require.Equal(t, []float64{.1, .2, .9, .3}, blocks[0].Locator.BBox)
}

// 页数是重复图（页眉页脚 logo）判定的分母，两种布局产物都要能读出来。
func TestMinerULayoutPageCount(t *testing.T) {
	require.Zero(t, minerULayoutPageCount(nil))
	require.Equal(t, 3, minerULayoutPageCount([]byte(
		`[{"page_idx":0},{"page_idx":2},{"page_idx":1}]`)))
	require.Equal(t, 5, minerULayoutPageCount([]byte(middleFixture)))
	require.Zero(t, minerULayoutPageCount([]byte(`{"schema":"other"}`)))
}

func buildMinerUZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}
