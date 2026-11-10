package service

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/types"
)

func docxZip(t *testing.T, paragraphs ...string) []byte {
	t.Helper()
	var body strings.Builder
	for _, p := range paragraphs {
		body.WriteString(`<w:p><w:r><w:t>`)
		body.WriteString(p)
		body.WriteString(`</w:t></w:r></w:p>`)
	}
	doc := `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
		body.String() + `</w:body></w:document>`
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	require.NoError(t, err)
	_, err = w.Write([]byte(doc))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func TestAttachStructureBlocksDocx(t *testing.T) {
	s := &knowledgeService{}
	result := &types.ReadResult{MarkdownContent: "标题\n\n正文第一段。\n\n正文第二段。"}
	s.attachStructureBlocks("docx", docxZip(t, "标题", "正文第一段。", "正文第二段。"), result)
	require.Len(t, result.SourceBlocks, 3)
	assert.Equal(t, 2, result.SourceBlocks[1].Locator.Block)
	assert.Equal(t, "exact", result.SourceBlocks[1].Locator.Mapping)
}

func TestAttachStructureBlocksPassThroughText(t *testing.T) {
	s := &knowledgeService{}
	result := &types.ReadResult{MarkdownContent: "# 标题\n\n第一段内容。"}
	s.attachStructureBlocks("md", nil, result)
	require.Len(t, result.SourceBlocks, 2)
	assert.Equal(t, types.SourceLocatorText, result.SourceBlocks[0].Locator.Type)
	assert.Equal(t, 0, result.SourceBlocks[0].Locator.Start)
}

func TestAttachStructureBlocksSkipsErrorAndAudio(t *testing.T) {
	s := &knowledgeService{}
	failed := &types.ReadResult{Error: "boom", MarkdownContent: "x"}
	s.attachStructureBlocks("docx", docxZip(t, "x"), failed)
	assert.Empty(t, failed.SourceBlocks)

	audio := &types.ReadResult{IsAudio: true, AudioData: []byte("mp3"), MarkdownContent: "[audio placeholder]"}
	s.attachStructureBlocks("docx", docxZip(t, "x"), audio)
	assert.Empty(t, audio.SourceBlocks, "音频占位文本不应生成定位块")
}

func TestAttachStructureBlocksKeepsParserBlocks(t *testing.T) {
	s := &knowledgeService{}
	result := &types.ReadResult{
		MarkdownContent: "p1\n\np2",
		SourceBlocks:    []types.SourceBlock{{Start: 0, End: 3, Locator: types.SourceLocator{Type: types.SourceLocatorPDF, Page: 1}}},
	}
	s.attachStructureBlocks("docx", docxZip(t, "p1", "p2"), result)
	require.Len(t, result.SourceBlocks, 1)
	assert.Equal(t, types.SourceLocatorPDF, result.SourceBlocks[0].Locator.Type, "解析器上报的定位块优先，不被结构对齐覆盖")
}

func TestTranscriptWithSegmentsAndBlocks(t *testing.T) {
	segs := []transcriptSegment{
		{Text: " 第一句话。 ", StartMs: 0, EndMs: 1500},
		{Text: "", StartMs: 1500, EndMs: 2000},
		{Text: "第二句话。", StartMs: 2000, EndMs: 4000},
	}
	md := transcriptWithSegments("第一句话。 第二句话。", segs)
	assert.Equal(t, "第一句话。\n\n第二句话。", md)

	blocks := transcriptSourceBlocks(segs)
	require.Len(t, blocks, 2)
	assert.Equal(t, types.SourceLocatorTime, blocks[0].Locator.Type)
	assert.Equal(t, int64(0), blocks[0].Locator.StartMs)
	assert.Equal(t, int64(1500), blocks[0].Locator.EndMs)
	assert.Equal(t, 0, blocks[0].Start)
	assert.Equal(t, len([]rune("第一句话。")), blocks[0].End)
	assert.Equal(t, len([]rune("第一句话。"))+2, blocks[1].Start)
	assert.Equal(t, int64(4000), blocks[1].Locator.EndMs)

	// 无分段时保持全文
	assert.Equal(t, "全文", transcriptWithSegments("全文", nil))
}

func TestBuildSourceIndexRemapsImageURLs(t *testing.T) {
	s := &knowledgeService{}
	knowledge := &types.Knowledge{ID: "k1", FileHash: "hash-1"}
	parsedMarkdown := "前文\n\n![图](images/a.png)\n\n后文"
	finalMarkdown := "前文\n\n![图](local://1/images/0123456789abcdef.png)\n\n后文"
	blocks := []types.SourceBlock{
		{Start: 0, End: runeIndexFor(t, parsedMarkdown, "![图]"), Locator: types.SourceLocator{Type: types.SourceLocatorPDF, Page: 1}},
		{Start: runeIndexFor(t, parsedMarkdown, "![图]"), End: len([]rune(parsedMarkdown)), Locator: types.SourceLocator{Type: types.SourceLocatorPDF, Page: 2}},
	}
	images := []docparser.StoredImage{{OriginalRef: "images/a.png", ServingURL: "local://1/images/0123456789abcdef.png"}}

	idx := s.buildSourceIndex(knowledge, parsedMarkdown, blocks, finalMarkdown, images)
	require.NotNil(t, idx)
	require.Len(t, images, 1)
	require.NotEmpty(t, images[0].SourceLocators, "图片应定位到其所在页")

	// 定位块区间 → 页码
	locs := idx.Locators(0, runeIndexFor(t, finalMarkdown, "![图]"))
	require.Len(t, locs, 1)
	assert.Equal(t, 1, locs[0].Page)
	assert.Equal(t, "hash-1", locs[0].SourceHash, "定位块绑定源文件版本哈希")

	locs2 := idx.Locators(runeIndexFor(t, finalMarkdown, "后文"), len([]rune(finalMarkdown)))
	require.Len(t, locs2, 1)
	assert.Equal(t, 2, locs2[0].Page)
}

func TestBuildSourceIndexEmptyBlocks(t *testing.T) {
	s := &knowledgeService{}
	idx := s.buildSourceIndex(&types.Knowledge{}, "abc", nil, "abc", nil)
	assert.Nil(t, idx, "无定位块时索引为 nil，链路照常")
}

func runeIndexFor(t *testing.T, s, sub string) int {
	t.Helper()
	i := strings.Index(s, sub)
	require.NotEqual(t, -1, i)
	return len([]rune(s[:i]))
}
