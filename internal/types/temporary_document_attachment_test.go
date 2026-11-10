package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FR-22 ⑤：图片理解缓存存在 temporary_documents.metadata 里，
// 属于服务端缓存，绝不能出现在对客户端的响应中。
func TestTemporaryDocumentForClientStripsImageDescriptions(t *testing.T) {
	doc := &TemporaryDocument{
		ID:       "doc-1",
		FileName: "report.pdf",
		Metadata: JSON(`{
			"parser": "document_reader",
			"attachment_image_descriptions": {"abc": {"text": "内部观察结果", "model": "vlm-1"}}
		}`),
	}

	client := doc.ForClient()
	require.NotNil(t, client)

	var metadata map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(client.Metadata, &metadata))
	assert.NotContains(t, metadata, TemporaryDocumentImageDescriptionsKey,
		"对客户端输出前必须剥离图片理解缓存")
	assert.Contains(t, metadata, "parser", "其余解析器元数据必须保留")
	assert.NotContains(t, string(client.Metadata), "内部观察结果")

	// 接收者本身不能被修改：缓存还要留给后续轮次复用。
	var original map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(doc.Metadata, &original))
	assert.Contains(t, original, TemporaryDocumentImageDescriptionsKey)

	// 返回的是副本，不是同一个指针。
	assert.NotSame(t, doc, client)
}

func TestTemporaryDocumentForClientKeepsMetadataWithoutCache(t *testing.T) {
	doc := &TemporaryDocument{ID: "doc-1", Metadata: JSON(`{"parser":"asr"}`)}
	client := doc.ForClient()
	require.NotNil(t, client)
	assert.JSONEq(t, `{"parser":"asr"}`, string(client.Metadata))

	var empty *TemporaryDocument
	assert.Nil(t, empty.ForClient())

	noMetadata := (&TemporaryDocument{ID: "doc-2"}).ForClient()
	require.NotNil(t, noMetadata)
	assert.Empty(t, noMetadata.Metadata)

	broken := (&TemporaryDocument{ID: "doc-3", Metadata: JSON(`not json`)}).ForClient()
	require.NotNil(t, broken)
	assert.Equal(t, JSON(`not json`), broken.Metadata)
}

// ViewableImages 决定 read_file 的 /images/N 编号，必须过滤空 URL，
// 并在"上传本身就是图片、解析器没存任何图"时退回源文件。
func TestViewableImages(t *testing.T) {
	doc := &TemporaryDocument{
		ImageRefs: JSON(`[{"url":"provider://a.png"},{"url":"  "},{"url":"provider://b.png"}]`),
	}
	images := doc.ViewableImages()
	require.Len(t, images, 2)
	assert.Equal(t, "provider://a.png", images[0].URL)
	assert.Equal(t, "provider://b.png", images[1].URL)

	upload := &TemporaryDocument{
		MimeType: "image/png", ResourceRef: "provider://upload.png", ImageRefs: JSON(`[]`),
	}
	require.Len(t, upload.ViewableImages(), 1)
	assert.Equal(t, "provider://upload.png", upload.ViewableImages()[0].URL)

	none := &TemporaryDocument{MimeType: "application/pdf", ResourceRef: "provider://x.pdf"}
	assert.Empty(t, none.ViewableImages())
}

// 文本里的图片必须带上编号，模型才知道该请求 /images/几。
func TestNumberAttachmentImages(t *testing.T) {
	images := []TemporaryDocumentImage{{URL: "provider://a.png"}, {URL: "provider://b.png"}}
	content := "前文 ![图](provider://a.png) 中间 ![](provider://b.png) 结尾 ![未知](provider://zzz.png)"

	got := NumberAttachmentImages(content, images)
	assert.Contains(t, got, "![图](provider://a.png) [image #1]")
	assert.Contains(t, got, "![](provider://b.png) [image #2]")
	// 未知引用保持原样，不标注编号。
	assert.Contains(t, got, "![未知](provider://zzz.png)")
	assert.NotContains(t, got, "provider://zzz.png) [image")

	// 没有图片或没有图片语法时原样返回。
	assert.Equal(t, "plain text", NumberAttachmentImages("plain text", images))
	assert.Equal(t, content, NumberAttachmentImages(content, nil))
}

// handle 必须稳定（跨轮次、跨历史回放指代同一个文件）且不是原始文档 ID。
func TestAttachmentHandleAndPath(t *testing.T) {
	handle := AttachmentHandle("doc-1")
	require.NotEmpty(t, handle)
	assert.True(t, len(handle) > 4)
	assert.Contains(t, handle, "att_")
	assert.Equal(t, handle, AttachmentHandle("  doc-1  "))
	assert.Equal(t, handle, AttachmentHandle("doc-1"), "handle 必须稳定")
	assert.NotEqual(t, handle, AttachmentHandle("doc-2"))
	assert.NotEqual(t, "doc-1", handle, "handle 不能是原始文档 ID")

	assert.Equal(t, AttachmentPathScheme+handle, AttachmentPath("doc-1"))
	assert.Empty(t, AttachmentHandle(""))
	assert.Empty(t, AttachmentPath("   "))
}

// 图片读取请求的归一化决定了缓存键，必须补齐默认档并限制 focus 长度。
func TestNormalizeImageRead(t *testing.T) {
	detail, focus := NormalizeImageRead("", "  看这里  ")
	assert.Equal(t, ImageDetailBrief, detail)
	assert.Equal(t, "看这里", focus)

	detail, _ = NormalizeImageRead(ImageDetailFull, "")
	assert.Equal(t, ImageDetailFull, detail)

	detail, _ = NormalizeImageRead("nonsense", "")
	assert.Equal(t, ImageDetailBrief, detail, "未知档位必须退回 brief")

	long := string(make([]rune, ImageReadFocusRunes+50))
	_, focus = NormalizeImageRead(ImageDetailBrief, long)
	assert.Equal(t, ImageReadFocusRunes, len([]rune(focus)))
}

func TestImageReadPromptCarriesFocus(t *testing.T) {
	plain := ImageReadPrompt(ImageDetailBrief, "")
	withFocus := ImageReadPrompt(ImageDetailBrief, "柱状图的纵轴单位")
	assert.NotEqual(t, plain, withFocus)
	assert.Contains(t, withFocus, "柱状图的纵轴单位")
	assert.NotEqual(t, plain, ImageReadPrompt(ImageDetailFull, ""), "detail 不同提示词必须不同")
}
