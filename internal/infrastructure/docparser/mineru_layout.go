package docparser

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/sourceloc"
	"github.com/Tencent/WeKnora/internal/types"
)

// FR-14 ①：MinerU 的布局解析结果必须保留"区域类型 + 几何信息"，
// 供下游切分（FR-05）与引用溯源（FR-21）使用。
//
// MinerU 有两种布局产物：
//   - legacy content_list.json：bbox 归一化到 0-1000，原点左上；
//   - MiddleJson 2.0：bbox 已经是 [0,1] 归一化坐标。
//
// 两者都统一转成 types.SourceBlock（Markdown rune 区间 → 页码 + 归一化 bbox）。

// minerUBBoxScale is the coordinate range of content_list bounding boxes:
// MinerU normalizes them to 0-1000 of the page, origin at the top-left.
const minerUBBoxScale = 1000.0

// minerUContentItem is one layout block of MinerU's content_list output.
type minerUContentItem struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	PageIdx      *int            `json:"page_idx"`
	BBox         []float64       `json:"bbox"`
	TableBody    string          `json:"table_body"`
	TableCaption json.RawMessage `json:"table_caption"`
	ImageCaption json.RawMessage `json:"image_caption"`
	ListItems    json.RawMessage `json:"list_items"`
	CodeBody     string          `json:"code_body"`
}

var minerUHTMLTagRe = regexp.MustCompile(`<[^>]+>`)

// text is the block's searchable text, in the order MinerU renders it to
// markdown: captions above tables and figures, then the body.
func (it minerUContentItem) text() string {
	parts := []string{it.Text}
	parts = append(parts, rawStrings(it.TableCaption)...)
	parts = append(parts, rawStrings(it.ImageCaption)...)
	if it.TableBody != "" {
		parts = append(parts, minerUHTMLTagRe.ReplaceAllString(it.TableBody, " "))
	}
	parts = append(parts, it.CodeBody)
	parts = append(parts, rawStrings(it.ListItems)...)
	return strings.TrimSpace(strings.Join(parts, " "))
}

// rawStrings reads a JSON string or array of strings.
func rawStrings(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	return nil
}

// decodeMinerUContentList accepts the list itself or the list serialized as
// a JSON string, which is how some MinerU API versions return it.
func decodeMinerUContentList(raw []byte) []minerUContentItem {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var inner string
		if json.Unmarshal(raw, &inner) != nil {
			return nil
		}
		raw = []byte(inner)
	}
	var items []minerUContentItem
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	return items
}

// minerUContentListFromZip prefers supported MiddleJson 2.0 geometry and
// falls back to legacy content_list.json for older MinerU packages.
// 条目读取一律走 readZipEntryCapped，避免布局 JSON 成为解压炸弹的入口。
func minerUContentListFromZip(zipData []byte) []byte {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil
	}
	var candidates []*zip.File
	for _, f := range zr.File {
		name := strings.ToLower(f.Name)
		if strings.HasSuffix(name, "middle_json.json") || strings.HasSuffix(name, "content_list.json") {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		mi := strings.HasSuffix(strings.ToLower(candidates[i].Name), "middle_json.json")
		mj := strings.HasSuffix(strings.ToLower(candidates[j].Name), "middle_json.json")
		if mi != mj {
			return mi
		}
		di, dj := strings.Count(candidates[i].Name, "/"), strings.Count(candidates[j].Name, "/")
		if di != dj {
			return di < dj
		}
		return candidates[i].Name < candidates[j].Name
	})
	for _, candidate := range candidates {
		data, err := readZipEntryCapped(candidate, maxZipEntryBytes())
		if err != nil {
			continue
		}
		if strings.HasSuffix(strings.ToLower(candidate.Name), "middle_json.json") {
			var doc minerUMiddle
			if json.Unmarshal(data, &doc) != nil || doc.Schema != "docvortex.middle" || doc.Version != "2.0" {
				continue
			}
		}
		return data
	}
	return nil
}

// minerUFileTypeOf resolves the file type used to decide whether MinerU's
// pages are the original document's pages: FileType first, then the name's
// extension. Office files go through an intermediate PDF inside MinerU, so
// their page numbers do not describe the uploaded file.
func minerUFileTypeOf(req *types.ReadRequest) string {
	if req == nil {
		return ""
	}
	if ft := strings.TrimSpace(req.FileType); ft != "" {
		return strings.TrimPrefix(strings.ToLower(ft), ".")
	}
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(req.FileName)), ".")
}

// minerUSourceBlocks aligns MinerU's layout blocks against its markdown so
// each paragraph, table and figure points back at its page and region. Only
// PDFs and images have pages a viewer can show; for office files MinerU's
// pages belong to an intermediate PDF, so those are left to the structure
// aligner.
func minerUSourceBlocks(markdown string, contentList []byte, fileType string) []types.SourceBlock {
	ft := strings.ToLower(strings.TrimPrefix(fileType, "."))
	if ft != "pdf" && !IsImageFormat(ft) {
		return nil
	}
	raw := bytes.TrimSpace(contentList)
	if len(raw) > 0 && raw[0] == '{' {
		return minerUMiddleSourceBlocks(markdown, raw)
	}
	items := decodeMinerUContentList(contentList)
	if len(items) == 0 {
		return nil
	}
	units := make([]sourceloc.Unit, 0, len(items))
	for i, it := range items {
		if it.PageIdx == nil || *it.PageIdx < 0 {
			continue
		}
		loc := types.SourceLocator{
			Type:     types.SourceLocatorPDF,
			Page:     *it.PageIdx + 1,
			SourceID: minerURegionSourceID(fmt.Sprintf("mineru:legacy:%d:%d", *it.PageIdx, i), it.Type),
		}
		if bbox, ok := minerUBBox(it.BBox); ok {
			loc.BBox = bbox
		}
		units = append(units, sourceloc.Unit{Text: it.text(), Locator: loc})
	}
	return sourceloc.Align(markdown, units)
}

// minerUBBox converts a 0-1000 content_list box to page fractions. Boxes
// outside that range come from MinerU versions that report other units and
// are dropped rather than drawn in the wrong place.
func minerUBBox(b []float64) ([]float64, bool) {
	if len(b) != 4 || b[2] <= b[0] || b[3] <= b[1] {
		return nil, false
	}
	for _, v := range b {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > minerUBBoxScale {
			return nil, false
		}
	}
	out := make([]float64, 4)
	for i, v := range b {
		out[i] = v / minerUBBoxScale
	}
	return out, true
}

// minerULayoutPageCount reports how many pages the layout describes, used as
// the denominator of the repeated-image (header/footer logo) rule. Zero means
// "unknown", in which case the repeat rule is skipped rather than guessed.
func minerULayoutPageCount(contentList []byte) int {
	raw := bytes.TrimSpace(contentList)
	if len(raw) == 0 {
		return 0
	}
	pages := 0
	bump := func(idx *int) {
		if idx != nil && *idx+1 > pages {
			pages = *idx + 1
		}
	}
	if raw[0] == '{' {
		var doc minerUMiddle
		if json.Unmarshal(raw, &doc) != nil {
			return 0
		}
		for _, page := range doc.Pages {
			bump(page.PageIdx)
		}
		return pages
	}
	for _, it := range decodeMinerUContentList(raw) {
		bump(it.PageIdx)
	}
	return pages
}

// MiddleJson 2.0 uses [0,1] normalized boxes (NOT legacy 0..1000).
// Child annotations keep their own geometry. Inline spans have no geometry.
type minerUMiddleBlock struct {
	Type    string          `json:"type"`
	Index   *int            `json:"index"`
	BBox    []float64       `json:"bbox"`
	Content json.RawMessage `json:"content"`
}

type minerUMiddle struct {
	Schema  string `json:"schema"`
	Version string `json:"schema_version"`
	Pages   []struct {
		PageIdx *int                `json:"page_idx"`
		Blocks  []minerUMiddleBlock `json:"blocks"`
	} `json:"pages"`
}

// minerUMiddleSourceBlocks turns MiddleJson 2.0 into source blocks. Page and
// block indexes must be strictly increasing: MinerU emits them in reading
// order, so a violation means the package does not describe the markdown we
// were handed. Returning nothing is safer than pointing a citation at the
// wrong page.
func minerUMiddleSourceBlocks(markdown string, raw []byte) []types.SourceBlock {
	var doc minerUMiddle
	if json.Unmarshal(raw, &doc) != nil || doc.Schema != "docvortex.middle" || doc.Version != "2.0" {
		return nil
	}
	var units []sourceloc.Unit
	previousPage := -1
	for _, page := range doc.Pages {
		if page.PageIdx == nil || *page.PageIdx <= previousPage {
			return nil
		}
		previousPage = *page.PageIdx
		previousBlock := -1
		for _, block := range page.Blocks {
			if block.Index == nil || *block.Index <= previousBlock {
				return nil
			}
			previousBlock = *block.Index
			ref := fmt.Sprintf("mineru:2:%d:%d", *page.PageIdx, *block.Index)
			units = append(units, minerUMiddleUnits(block, *page.PageIdx+1, ref, nil)...)
		}
	}
	return sourceloc.Align(markdown, units)
}

// minerUMiddleUnits flattens one MiddleJson block into text units. Container
// blocks recurse; a child without its own geometry inherits the parent's box
// so the region is never lost.
func minerUMiddleUnits(block minerUMiddleBlock, page int, ref string, parentBox []float64) []sourceloc.Unit {
	box := block.BBox
	if len(box) == 0 {
		box = parentBox
	}
	loc := types.SourceLocator{Type: types.SourceLocatorPDF, Page: page,
		SourceID: minerURegionSourceID(ref, block.Type)}
	if normalizedMinerUBox(box) {
		loc.BBox = append([]float64(nil), box...)
	}
	// These containers contain blocks; text and hyperlinks contain inline spans.
	switch block.Type {
	case "list", "index", "table", "image", "chart", "code":
		var children []minerUMiddleBlock
		if json.Unmarshal(block.Content, &children) != nil {
			return nil
		}
		var out []sourceloc.Unit
		for i, child := range children {
			out = append(out, minerUMiddleUnits(child, page, fmt.Sprintf("%s/%d", ref, i), box)...)
		}
		return out
	default:
		text := minerUMiddleText(block.Content)
		if block.Type == "table_body" || block.Type == "chart_body" {
			text = html.UnescapeString(minerUHTMLTagRe.ReplaceAllString(text, " "))
		}
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []sourceloc.Unit{{Text: text, Locator: loc}}
	}
}

func minerUMiddleText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var spans []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &spans) != nil {
		return ""
	}
	var out strings.Builder
	for _, span := range spans {
		out.WriteString(minerUMiddleText(span.Content))
	}
	return out.String()
}

// normalizedMinerUBox reports whether box is a usable [0,1] region.
func normalizedMinerUBox(box []float64) bool {
	if len(box) != 4 || box[0] >= box[2] || box[1] >= box[3] {
		return false
	}
	for _, v := range box {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return false
		}
	}
	return true
}

// --- 区域类型（FR-14 ①）---
//
// 布局产物在解析结束后就被丢弃，只有 SourceBlock 会随 chunk 落库，所以区域
// 类型必须写进 locator 本身。types.SourceLocator 没有区域类型字段，而
// SourceID 对下游是不透明字符串，因此把类型编进它的命名空间：
//
//	MiddleJson 2.0    mineru:2:<pageIdx>:<blockIdx>[/<childIdx>…]:<kind>
//	legacy content_list  mineru:legacy:<pageIdx>:<itemIdx>:<kind>
//	云端分片再前置      part-<n>/…（见 mineru_cloud_parts.go）
//
// 下游（FR-05 切分、FR-21 表格与公式的单元格级溯源）取 SourceID 最后一段
// 即可拿到区域类型，不必重新解析 MinerU 的布局 JSON。

// minerURegionKinds 把 MinerU 的区域类型归一成一组稳定标识：同一族的子块
// （table_caption / table_body / table_footnote）归到同一个族，下游只需认
// 这几个值。
var minerURegionKinds = map[string]string{
	"text": "text", "paragraph": "text", "hyperlink": "text", "text_span": "text",
	"title": "title", "section_title": "title",
	"header": "header", "footer": "footer",
	"table": "table", "table_body": "table", "table_caption": "table", "table_footnote": "table",
	"image": "image", "figure": "image", "image_body": "image",
	"image_caption": "image", "image_footnote": "image",
	"equation": "equation", "formula": "equation",
	"interline_equation": "equation", "inline_equation": "equation",
	"code": "code", "code_body": "code", "code_caption": "code",
	"list": "list", "list_item": "list", "index": "list",
	"chart": "chart", "chart_body": "chart", "chart_caption": "chart",
	"reference": "reference", "footnote": "footnote",
}

var minerURegionTypeSanitizer = regexp.MustCompile(`[^a-z0-9_]+`)

// minerURegionKind 归一一个 MinerU 区域类型；空类型返回空串。
func minerURegionKind(blockType string) string {
	normalized := strings.ToLower(strings.TrimSpace(blockType))
	if kind, ok := minerURegionKinds[normalized]; ok {
		return kind
	}
	// 新版本 MinerU 可能带来未知类型：清洗后原样保留。宁可多带一个陌生标识，
	// 也不要把区域类型丢掉。
	return minerURegionTypeSanitizer.ReplaceAllString(normalized, "")
}

// minerURegionSourceID 在块 ID 后追加区域类型。
func minerURegionSourceID(ref, blockType string) string {
	if kind := minerURegionKind(blockType); kind != "" {
		return ref + ":" + kind
	}
	return ref
}
