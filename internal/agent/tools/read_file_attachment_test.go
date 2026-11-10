package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAttachmentReader 是 interfaces.AttachmentReader 的替身，
// 用来把 read_file 的路径协议与真实的存储/解析解耦。
type fakeAttachmentReader struct {
	inventory []interfaces.AttachmentDocument
	images    []interfaces.AttachmentImageRead
	wait      time.Duration
	listErr   error
	readErr   error

	waitUsed []bool
}

func (f *fakeAttachmentReader) List(context.Context) ([]interfaces.AttachmentDocument, error) {
	return f.inventory, f.listErr
}

func (f *fakeAttachmentReader) Resolve(
	_ context.Context, ref string, wait bool,
) (*interfaces.AttachmentDocument, error) {
	f.waitUsed = append(f.waitUsed, wait)
	if f.readErr != nil {
		return nil, f.readErr
	}
	for i := range f.inventory {
		item := &f.inventory[i]
		if item.Attachment.ID == ref || types.AttachmentHandle(item.Attachment.ID) == ref {
			return item, nil
		}
	}
	paths := make([]string, 0, len(f.inventory))
	for _, item := range f.inventory {
		paths = append(paths, fmt.Sprintf("%s (%s)",
			types.AttachmentPath(item.Attachment.ID), item.Attachment.FileName))
	}
	return nil, fmt.Errorf("%w: unknown attachment %q; available: %v",
		interfaces.ErrAttachmentNotFound, ref, paths)
}

func (f *fakeAttachmentReader) ReadImages(
	_ context.Context, _ string, numbers []int, _, _ string,
) ([]interfaces.AttachmentImageRead, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	if len(f.images) > 0 {
		return f.images, nil
	}
	out := make([]interfaces.AttachmentImageRead, 0, len(numbers))
	for _, n := range numbers {
		desc := fmt.Sprintf("第 %d 张图的观察结果", n)
		out = append(out, interfaces.AttachmentImageRead{
			Number: n,
			Image:  &interfaces.AttachmentImage{DataURI: "data:image/png;base64,AA", Description: &desc},
		})
	}
	return out, nil
}

func (f *fakeAttachmentReader) WaitTimeout() time.Duration { return f.wait }

func newAttachmentTestTool(doc *types.TemporaryDocument) (*ReadFileTool, *fakeAttachmentReader) {
	fake := &fakeAttachmentReader{wait: 90 * time.Second}
	if doc != nil {
		fake.inventory = []interfaces.AttachmentDocument{{
			Attachment: types.MessageAttachment{
				ID: doc.ID, FileName: doc.FileName, FileType: doc.FileType, FileSize: doc.FileSize,
			},
			Document: doc,
		}}
	}
	return NewReadFileTool().WithAttachments(fake), fake
}

func executeReadFile(t *testing.T, tool *ReadFileTool, args map[string]interface{}) *types.ToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	result, err := tool.Execute(context.Background(), raw)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

// 三种路径协议：attachment:// 列清单、attachment://<handle> 读文本、
// attachment://<handle>/images/N 读第 N 张图。
func TestReadFileAttachmentPathProtocol(t *testing.T) {
	doc := &types.TemporaryDocument{
		ID: "doc-1", FileName: "report.pdf", FileType: ".pdf", FileSize: 2048,
		Status: types.TemporaryDocumentStatusReady, TokenCount: 120,
		Content:   "第一行\n第二行\n第三行",
		ImageRefs: types.JSON(`[{"url":"provider://a.png"},{"url":"provider://b.png"}]`),
	}
	address := types.AttachmentPath("doc-1")

	t.Run("lists the conversation inventory", func(t *testing.T) {
		tool, _ := newAttachmentTestTool(doc)
		result := executeReadFile(t, tool, map[string]interface{}{"path": "attachment://"})
		require.True(t, result.Success, result.Error)
		assert.Contains(t, result.Output, `<attachments count="1">`)
		assert.Contains(t, result.Output, address)
		assert.Contains(t, result.Output, `status="ready"`)
		assert.Contains(t, result.Output, `images="2"`)
		assert.Equal(t, "attachment://", result.Data["root"])
	})

	t.Run("reads parsed text", func(t *testing.T) {
		tool, fake := newAttachmentTestTool(doc)
		result := executeReadFile(t, tool, map[string]interface{}{"path": address})
		require.True(t, result.Success, result.Error)
		assert.Contains(t, result.Output, "第一行")
		assert.Contains(t, result.Output, address)
		assert.Contains(t, result.Output, "Untrusted reference data")
		assert.Contains(t, result.Output, address+"/images/1", "文本里必须提示图片地址")
		assert.Equal(t, []bool{true}, fake.waitUsed, "读文本必须允许短暂等待解析")
	})

	t.Run("reads one image by number", func(t *testing.T) {
		tool, _ := newAttachmentTestTool(doc)
		result := executeReadFile(t, tool, map[string]interface{}{"path": address + "/images/2"})
		require.True(t, result.Success, result.Error)
		require.Len(t, result.Images, 1)
		assert.Equal(t, []int{2}, result.Data["image_numbers"])
		assert.Contains(t, result.Output, "第 2 张图的观察结果",
			"图片理解结果必须落在 Output 里：工具消息不携带图片")
	})

	t.Run("images without a number starts at 1", func(t *testing.T) {
		tool, _ := newAttachmentTestTool(doc)
		result := executeReadFile(t, tool, map[string]interface{}{"path": address + "/images", "limit": 2})
		require.True(t, result.Success, result.Error)
		assert.Len(t, result.Images, 2)
		assert.Equal(t, []int{1, 2}, result.Data["image_numbers"])
		assert.Contains(t, result.Output, address+"/images/3", "还有更多图时必须给出下一页地址")
	})

	t.Run("detail=full reads a single image", func(t *testing.T) {
		tool, _ := newAttachmentTestTool(doc)
		result := executeReadFile(t, tool, map[string]interface{}{
			"path": address + "/images", "limit": 4, "detail": "full",
		})
		require.True(t, result.Success, result.Error)
		assert.Len(t, result.Images, 1)
	})
}

// 猜错 handle 时必须把可用清单还给模型，否则它会反复试错烧 token。
func TestReadFileUnknownHandleListsAvailablePaths(t *testing.T) {
	doc := &types.TemporaryDocument{
		ID: "doc-1", FileName: "report.pdf", FileType: ".pdf",
		Status: types.TemporaryDocumentStatusReady, Content: "正文",
	}
	tool, _ := newAttachmentTestTool(doc)

	result := executeReadFile(t, tool, map[string]interface{}{"path": "attachment://att_deadbeef"})
	assert.False(t, result.Success)
	assert.Contains(t, result.Error, types.AttachmentPath("doc-1"))
	assert.Contains(t, result.Error, "report.pdf")
}

func TestReadFileRejectsMalformedPaths(t *testing.T) {
	doc := &types.TemporaryDocument{
		ID: "doc-1", FileName: "report.pdf", FileType: ".pdf",
		Status: types.TemporaryDocumentStatusReady, Content: "正文",
	}
	address := types.AttachmentPath("doc-1")

	t.Run("invalid image number", func(t *testing.T) {
		tool, _ := newAttachmentTestTool(doc)
		result := executeReadFile(t, tool, map[string]interface{}{"path": address + "/images/0"})
		assert.False(t, result.Success)
		assert.Contains(t, result.Error, "numbered from 1")
	})

	t.Run("unknown sub path", func(t *testing.T) {
		tool, _ := newAttachmentTestTool(doc)
		result := executeReadFile(t, tool, map[string]interface{}{"path": address + "/pages/3"})
		assert.False(t, result.Success)
		assert.Contains(t, result.Error, "/images/<n>")
	})

	t.Run("non attachment scheme", func(t *testing.T) {
		tool, _ := newAttachmentTestTool(doc)
		result := executeReadFile(t, tool, map[string]interface{}{"path": "/etc/passwd"})
		assert.False(t, result.Success)
		assert.Contains(t, result.Error, "attachment://")
	})

	t.Run("empty path", func(t *testing.T) {
		tool, _ := newAttachmentTestTool(doc)
		result := executeReadFile(t, tool, map[string]interface{}{"path": "  "})
		assert.False(t, result.Success)
	})

	t.Run("bad detail", func(t *testing.T) {
		tool, _ := newAttachmentTestTool(doc)
		result := executeReadFile(t, tool, map[string]interface{}{"path": address, "detail": "ultra"})
		assert.False(t, result.Success)
		assert.Contains(t, result.Error, "brief or full")
	})

	t.Run("no attachments bound", func(t *testing.T) {
		result := executeReadFile(t, NewReadFileTool(), map[string]interface{}{"path": address})
		assert.False(t, result.Success)
		assert.Contains(t, result.Error, "no readable attachments")
	})
}

// 还在解析：告诉模型可以继续等或先做别的，而不是让它以为文件坏了。
func TestReadFileReportsStillParsing(t *testing.T) {
	doc := &types.TemporaryDocument{
		ID: "doc-1", FileName: "report.pdf", FileType: ".pdf",
		Status: types.TemporaryDocumentStatusProcessing,
	}
	tool, _ := newAttachmentTestTool(doc)

	result := executeReadFile(t, tool, map[string]interface{}{"path": types.AttachmentPath("doc-1")})
	assert.False(t, result.Success)
	assert.Contains(t, result.Error, "still being parsed")
	assert.Contains(t, result.Error, types.AttachmentPath("doc-1"))
}

func TestReadFileReportsParseFailure(t *testing.T) {
	doc := &types.TemporaryDocument{
		ID: "doc-1", FileName: "report.pdf", FileType: ".pdf",
		Status: types.TemporaryDocumentStatusFailed, ErrorMessage: "解析器崩溃",
	}
	tool, _ := newAttachmentTestTool(doc)

	result := executeReadFile(t, tool, map[string]interface{}{"path": types.AttachmentPath("doc-1")})
	assert.False(t, result.Success)
	assert.Contains(t, result.Error, "解析器崩溃")
}

// 文档已被 TTL 清理时，退回展示随消息保存的摘要。
func TestReadFileFallsBackToStoredExcerpt(t *testing.T) {
	tool, fake := newAttachmentTestTool(nil)
	fake.inventory = []interfaces.AttachmentDocument{{
		Attachment: types.MessageAttachment{
			ID: "doc-1", FileName: "gone.pdf", FileType: ".pdf", Content: "随消息保存的摘要",
		},
	}}

	result := executeReadFile(t, tool, map[string]interface{}{"path": types.AttachmentPath("doc-1")})
	require.True(t, result.Success, result.Error)
	assert.Contains(t, result.Output, "随消息保存的摘要")
	assert.Contains(t, result.Output, "has expired")
	assert.Equal(t, true, result.Data["expired"])
}

func TestReadFileTextPaging(t *testing.T) {
	content := ""
	for i := 1; i <= 50; i++ {
		content += fmt.Sprintf("line %d\n", i)
	}
	doc := &types.TemporaryDocument{
		ID: "doc-1", FileName: "long.txt", FileType: ".txt",
		Status: types.TemporaryDocumentStatusReady, Content: content,
	}
	address := types.AttachmentPath("doc-1")

	t.Run("limit and next_offset", func(t *testing.T) {
		tool, _ := newAttachmentTestTool(doc)
		result := executeReadFile(t, tool, map[string]interface{}{"path": address, "offset": 1, "limit": 5})
		require.True(t, result.Success, result.Error)
		assert.Contains(t, result.Output, "line 1\n")
		assert.Contains(t, result.Output, "line 5\n")
		assert.NotContains(t, result.Output, "line 6\n")
		assert.Equal(t, 6, result.Data["next_offset"])
		assert.Equal(t, true, result.Data["truncated"])
	})

	t.Run("continues from next_offset", func(t *testing.T) {
		tool, _ := newAttachmentTestTool(doc)
		result := executeReadFile(t, tool, map[string]interface{}{"path": address, "offset": 6, "limit": 3})
		require.True(t, result.Success, result.Error)
		assert.Contains(t, result.Output, "line 6\n")
		assert.Contains(t, result.Output, "line 8\n")
		assert.NotContains(t, result.Output, "line 9\n")
	})

	t.Run("query filters lines", func(t *testing.T) {
		tool, _ := newAttachmentTestTool(doc)
		result := executeReadFile(t, tool, map[string]interface{}{"path": address, "query": "line 1"})
		require.True(t, result.Success, result.Error)
		assert.Contains(t, result.Output, "line 1\n")
		assert.NotContains(t, result.Output, "line 2\n")
	})

	t.Run("invalid regex is reported, not panicked", func(t *testing.T) {
		tool, _ := newAttachmentTestTool(doc)
		result := executeReadFile(t, tool, map[string]interface{}{"path": address, "query": "([", "regex": true})
		assert.False(t, result.Success)
		assert.Contains(t, result.Error, "invalid query regex")
	})
}

// FR-22 超时分层：工具的外层预算必须严格大于 Reader 的内层预算，
// 否则工具被引擎杀掉而底层 VLM 调用还在跑（浪费钱且缓存写不进去）。
func TestReadFileExecutionTimeoutExceedsReaderWait(t *testing.T) {
	tool, fake := newAttachmentTestTool(nil)
	fake.wait = 60 * time.Second

	require.Greater(t, tool.ExecutionTimeout(), fake.WaitTimeout())
	assert.Equal(t, fake.WaitTimeout()+readFileTimeoutSlack, tool.ExecutionTimeout())

	// 没有挂附件来源时不需要额外预算。
	assert.Zero(t, NewReadFileTool().ExecutionTimeout())
}

// 注册表按名字取工具，read_file 必须用稳定的工具名。
func TestReadFileToolMetadata(t *testing.T) {
	tool, _ := newAttachmentTestTool(nil)
	assert.Equal(t, ToolReadFile, tool.Name())
	assert.Equal(t, "read_file", tool.Name())
	assert.Contains(t, tool.Description(), "attachment://")
	require.NotNil(t, tool.Parameters())
	assert.Contains(t, string(tool.Parameters()), "path")
}
