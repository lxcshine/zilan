package attachment

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubStore 是 Store 的内存替身。readyAfter 表示"第 N 次加载之后状态变成 ready"，
// 用来驱动轮询；0 表示状态永远不变。
type stubStore struct {
	attachments types.MessageAttachments
	docs        map[string]*types.TemporaryDocument
	readyAfter  int

	mu    sync.Mutex
	loads map[string]int
}

func newStubStore(docs ...*types.TemporaryDocument) *stubStore {
	s := &stubStore{loads: map[string]int{}, docs: map[string]*types.TemporaryDocument{}}
	for _, doc := range docs {
		s.docs[doc.ID] = doc
		s.attachments = append(s.attachments, types.MessageAttachment{
			ID: doc.ID, FileName: doc.FileName, FileType: doc.FileType, FileSize: doc.FileSize,
		})
	}
	return s
}

func (s *stubStore) SessionAttachments(context.Context) (types.MessageAttachments, error) {
	return s.attachments, nil
}

func (s *stubStore) TemporaryDocument(_ context.Context, id string) (*types.TemporaryDocument, error) {
	s.mu.Lock()
	s.loads[id]++
	n := s.loads[id]
	s.mu.Unlock()
	doc := s.docs[id]
	if doc == nil {
		return nil, nil
	}
	out := *doc
	if s.readyAfter > 0 && n > s.readyAfter {
		out.Status = types.TemporaryDocumentStatusReady
	}
	return &out, nil
}

func (s *stubStore) loadCount(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads[id]
}

func pendingDoc(id string) *types.TemporaryDocument {
	return &types.TemporaryDocument{
		ID: id, FileName: id + ".pdf", FileType: ".pdf",
		Status: types.TemporaryDocumentStatusProcessing,
	}
}

// FR-22 ②：等待到点后返回"当前状态"而不是错误 —— 让调用方告诉模型"还在解析"，
// 而不是让整轮失败。
func TestAwaitDocumentReturnsCurrentStatusAtDeadline(t *testing.T) {
	store := newStubStore(pendingDoc("doc-1"))
	reader := NewReader(store, 0, nil)

	got, err := reader.Resolve(context.Background(), "doc-1", true)
	require.NoError(t, err)
	require.NotNil(t, got.Document)
	assert.Equal(t, types.TemporaryDocumentStatusProcessing, got.Document.Status)
	assert.Equal(t, 1, store.loadCount("doc-1"), "parseWait=0 时不应轮询")
}

// 仍在解析时按 pollInterval 轮询，解析完成后立刻返回 ready。
func TestAwaitDocumentPollsUntilReady(t *testing.T) {
	store := newStubStore(pendingDoc("doc-1"))
	store.readyAfter = 1
	reader := NewReader(store, 10*time.Second, nil)

	start := time.Now()
	got, err := reader.Resolve(context.Background(), "doc-1", true)
	require.NoError(t, err)
	require.NotNil(t, got.Document)
	assert.Equal(t, types.TemporaryDocumentStatusReady, got.Document.Status)
	assert.GreaterOrEqual(t, store.loadCount("doc-1"), 2)
	assert.Less(t, time.Since(start), 5*time.Second, "解析完成后不应继续等到 parseWait 用尽")
}

// ctx 取消必须立刻返回，不能把 parseWait 坐满。
func TestAwaitDocumentHonoursContextCancellation(t *testing.T) {
	store := newStubStore(pendingDoc("doc-1"))
	reader := NewReader(store, time.Hour, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := reader.Resolve(ctx, "doc-1", true)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// 猜错引用时，错误信息必须列出可用 handle 清单，否则模型会反复试错烧 token。
func TestResolveUnknownRefListsAvailableHandles(t *testing.T) {
	store := newStubStore(pendingDoc("doc-1"))
	reader := NewReader(store, time.Second, nil)

	_, err := reader.Resolve(context.Background(), "att_nope", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, interfaces.ErrAttachmentNotFound)
	assert.Contains(t, err.Error(), types.AttachmentPath("doc-1"))
	assert.Contains(t, err.Error(), "doc-1.pdf")
}

func TestResolveMatchesHandleAndFileName(t *testing.T) {
	store := newStubStore(pendingDoc("doc-1"))
	reader := NewReader(store, time.Second, nil)

	byHandle, err := reader.Resolve(context.Background(), types.AttachmentHandle("doc-1"), false)
	require.NoError(t, err)
	assert.Equal(t, "doc-1", byHandle.Document.ID)

	byName, err := reader.Resolve(context.Background(), "DOC-1.PDF", false)
	require.NoError(t, err)
	assert.Equal(t, "doc-1", byName.Document.ID)
}

// 清单去重：同一份文档在多轮消息里重复出现时只保留一条，
// 且后出现的（带解析内容）优先。内联上传（无 ID）不可读，直接排除。
func TestListDedupesAndSkipsInlineUploads(t *testing.T) {
	store := newStubStore(pendingDoc("doc-1"))
	store.attachments = append(store.attachments,
		types.MessageAttachment{ID: "doc-1", FileName: "doc-1.pdf", Content: "解析后的正文"},
		types.MessageAttachment{FileName: "inline.txt", Content: "legacy inline"},
		types.MessageAttachment{ID: "  ", FileName: "blank"},
	)
	reader := NewReader(store, time.Second, nil)

	got, err := reader.List(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "doc-1", got[0].Attachment.ID)
	assert.Equal(t, "解析后的正文", got[0].Attachment.Content)
}

func TestListEmptyInventory(t *testing.T) {
	reader := NewReader(&stubStore{loads: map[string]int{}}, time.Second, nil)
	got, err := reader.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got)
}

// FR-22 超时分层：外层必须严格大于内层，否则工具被引擎杀掉而底层 VLM 还在跑。
func TestWaitTimeoutLayering(t *testing.T) {
	parseWait := 60 * time.Second
	plain := NewReader(nil, parseWait, nil)
	assert.Equal(t, parseWait, plain.WaitTimeout())

	withImages := NewReader(nil, parseWait, nil)
	withImages.Images = &ImageReader{Model: "vlm", Describe: func(context.Context, []byte, string) (string, error) {
		return "", nil
	}}
	require.Greater(t, withImages.WaitTimeout(), plain.WaitTimeout())
	assert.Equal(t,
		parseWait+imageDescribeFullTimeout+imageDescribeTimeout,
		withImages.WaitTimeout())

	// 只配了 ImageReader 但没有 Describe（对话模型自己能看图）时不加图片预算。
	noDescribe := NewReader(nil, parseWait, nil)
	noDescribe.Images = &ImageReader{Model: "vlm"}
	assert.Equal(t, parseWait, noDescribe.WaitTimeout())
}

// 图片编号越界要作为单张失败上报，而不是让整批失败。
func TestReadImagesReportsOutOfRangePerImage(t *testing.T) {
	doc := &types.TemporaryDocument{
		ID: "doc-1", FileName: "doc-1.pdf", FileType: ".pdf", MimeType: "application/pdf",
		Status:    types.TemporaryDocumentStatusReady,
		ImageRefs: types.JSON(`[{"url":"provider://a.png"},{"url":"provider://b.png"}]`),
	}
	store := newStubStore(doc)
	reader := NewReader(store, time.Second, func(context.Context, string) ([]byte, error) {
		return pngBytes(t), nil
	})

	reads, err := reader.ReadImages(context.Background(), "doc-1", []int{1, 9}, "brief", "")
	require.NoError(t, err)
	require.Len(t, reads, 2)
	assert.Equal(t, 1, reads[0].Number)
	require.NoError(t, reads[0].Err)
	assert.NotNil(t, reads[0].Image)
	assert.Equal(t, 9, reads[1].Number)
	require.Error(t, reads[1].Err)
	assert.Contains(t, reads[1].Err.Error(), "out of range")
}

// 文档还没解析完时不读图：ReadImages 用 wait=false，避免为了看图白等一轮解析。
func TestReadImagesRefusesUnreadyDocument(t *testing.T) {
	store := newStubStore(pendingDoc("doc-1"))
	reader := NewReader(store, time.Second, nil)

	_, err := reader.ReadImages(context.Background(), "doc-1", []int{1}, "brief", "")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "not ready"))
}

func TestAttachmentPending(t *testing.T) {
	assert.True(t, attachmentPending(types.TemporaryDocumentStatusUploaded))
	assert.True(t, attachmentPending(types.TemporaryDocumentStatusProcessing))
	assert.False(t, attachmentPending(types.TemporaryDocumentStatusReady))
	assert.False(t, attachmentPending(types.TemporaryDocumentStatusFailed))
	assert.False(t, attachmentPending(""))
}

func TestNewReaderWithoutStoreFailsClosed(t *testing.T) {
	reader := NewReader(nil, time.Second, nil)
	_, err := reader.List(context.Background())
	require.Error(t, err)
	assert.False(t, errors.Is(err, interfaces.ErrAttachmentNotFound))
}
