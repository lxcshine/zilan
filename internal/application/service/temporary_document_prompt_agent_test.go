package service

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTemporaryDocumentRepo 只提供 DescribeForAgent / ResolveForPrompt 用到的查询。
type stubTemporaryDocumentRepo struct {
	interfaces.TemporaryDocumentRepository
	docs map[string]*types.TemporaryDocument
}

func (r *stubTemporaryDocumentRepo) GetScoped(
	_ context.Context, _ uint64, _ string, documentID string,
) (*types.TemporaryDocument, error) {
	return r.docs[documentID], nil
}

func newAgentPromptService(docs ...*types.TemporaryDocument) *temporaryDocumentService {
	repo := &stubTemporaryDocumentRepo{docs: map[string]*types.TemporaryDocument{}}
	for _, doc := range docs {
		repo.docs[doc.ID] = doc
	}
	return &temporaryDocumentService{repo: repo}
}

func readyDoc(id string, tokens int, content string) *types.TemporaryDocument {
	return &types.TemporaryDocument{
		ID: id, FileName: id + ".pdf", FileType: ".pdf", FileSize: 1024,
		Status: types.TemporaryDocumentStatusReady, Content: content,
		TokenCount: tokens, ChunkCount: 3, ResourceRef: "provider://" + id,
	}
}

// FR-22 ④：小的、已就绪的文件直接内联。
func TestDescribeForAgentInlinesSmallReadyDocument(t *testing.T) {
	svc := newAgentPromptService(readyDoc("doc-1", 800, "小文档正文"))

	got, err := svc.DescribeForAgent(context.Background(), 7, "sess-1", []string{"doc-1"})
	require.NoError(t, err)
	require.Len(t, got.Attachments, 1)

	att := got.Attachments[0]
	assert.Equal(t, types.AttachmentContentModeFull, att.ContentMode)
	assert.Equal(t, "小文档正文", att.Content)
	assert.Equal(t, 800, att.TokenCount)
}

// FR-22 ③：超过单篇内联阈值的文档只给引用（attachment:// 地址 + 状态 + 大纲）。
func TestDescribeForAgentTurnsLargeDocumentIntoReference(t *testing.T) {
	content := "# 第一章\n\n正文\n\n## 1.1 小节\n\n更多正文\n\n# 第二章\n"
	svc := newAgentPromptService(readyDoc("doc-1", types.AgentAttachmentInlineTokens+1, content))

	got, err := svc.DescribeForAgent(context.Background(), 7, "sess-1", []string{"doc-1"})
	require.NoError(t, err)
	require.Len(t, got.Attachments, 1)

	att := got.Attachments[0]
	assert.Equal(t, types.AttachmentContentModeOnDemand, att.ContentMode)
	assert.Contains(t, att.Content, types.AttachmentPath("doc-1"), "引用块必须带 attachment:// 地址")
	assert.Contains(t, att.Content, types.AttachmentPath("doc-1")+"/images/")
	assert.Contains(t, att.Content, "status: "+types.TemporaryDocumentStatusReady)
	assert.Contains(t, att.Content, "# 第一章", "引用块应带大纲")
	assert.NotContains(t, att.Content, "更多正文", "引用块不得内联正文")
}

// 大纲只取靠前的 markdown 标题，并有条数上限。
func TestTemporaryDocumentOutlineIsBounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < temporaryDocumentOutlineMaxHeadings+20; i++ {
		b.WriteString("## 标题 ")
		b.WriteString(strings.Repeat("长", 60))
		b.WriteString("\n\n正文\n\n")
	}
	outline := temporaryDocumentOutline(b.String())
	lines := strings.Split(outline, "\n")
	assert.LessOrEqual(t, len(lines), temporaryDocumentOutlineMaxHeadings+1)
	assert.Equal(t, "…", lines[len(lines)-1], "超出上限时必须以省略号收尾")
	assert.LessOrEqual(t, len([]rune(outline)), temporaryDocumentOutlineMaxRunes+2)

	assert.Empty(t, temporaryDocumentOutline("没有标题的纯正文"))
}

// agentAttachmentInlineBudget 是整轮的总预算，不是单篇预算：
// 每篇都在单篇阈值内，但累计超出后剩下的必须全部转按需读取。
func TestDescribeForAgentEnforcesWholeTurnInlineBudget(t *testing.T) {
	perDoc := types.AgentAttachmentInlineTokens - 200 // 1800
	// 用例前提：每篇都在单篇阈值内，前三篇累计不超总预算，第四篇会超。
	require.LessOrEqual(t, perDoc, types.AgentAttachmentInlineTokens)
	require.LessOrEqual(t, perDoc*3, agentAttachmentInlineBudget)
	require.Greater(t, perDoc*4, agentAttachmentInlineBudget)

	docs := []*types.TemporaryDocument{
		readyDoc("doc-1", perDoc, "正文一"),
		readyDoc("doc-2", perDoc, "正文二"),
		readyDoc("doc-3", perDoc, "正文三"),
		readyDoc("doc-4", perDoc, "正文四"),
	}
	svc := newAgentPromptService(docs...)

	got, err := svc.DescribeForAgent(context.Background(), 7, "sess-1",
		[]string{"doc-1", "doc-2", "doc-3", "doc-4"})
	require.NoError(t, err)
	require.Len(t, got.Attachments, 4)

	inlined := 0
	for i, att := range got.Attachments {
		if att.ContentMode == types.AttachmentContentModeFull {
			inlined++
			continue
		}
		assert.Equal(t, types.AttachmentContentModeOnDemand, att.ContentMode)
		assert.Contains(t, att.Content, types.AttachmentPath(docs[i].ID))
	}
	// 3 × 1800 = 5400 ≤ 6000，第 4 篇会到 7200 > 6000，因此只内联前三篇。
	assert.Equal(t, 3, inlined)
	assert.Equal(t, types.AttachmentContentModeFull, got.Attachments[0].ContentMode)
	assert.Equal(t, types.AttachmentContentModeFull, got.Attachments[2].ContentMode)
	assert.Equal(t, types.AttachmentContentModeOnDemand, got.Attachments[3].ContentMode)
}

// 还在解析的附件不阻塞、不失败：清单里带上状态，让 Agent 自己决定要不要等。
func TestDescribeForAgentDoesNotWaitForParsing(t *testing.T) {
	pending := readyDoc("doc-1", 100, "正文")
	pending.Status = types.TemporaryDocumentStatusProcessing
	pending.Content = ""
	pending.TokenCount = 0

	failed := readyDoc("doc-2", 100, "正文")
	failed.Status = types.TemporaryDocumentStatusFailed
	failed.ErrorMessage = "解析器超时"

	svc := newAgentPromptService(pending, failed)
	got, err := svc.DescribeForAgent(context.Background(), 7, "sess-1", []string{"doc-1", "doc-2"})
	require.NoError(t, err)
	require.Len(t, got.Attachments, 2)

	assert.Equal(t, types.AttachmentContentModeOnDemand, got.Attachments[0].ContentMode)
	assert.Contains(t, got.Attachments[0].Content, "status: processing")
	assert.Contains(t, got.Attachments[1].Content, "status: failed")
	assert.Contains(t, got.Attachments[1].Content, "解析器超时")
}

// 会话里不存在的文档直接跳过，不让整份清单失败。
func TestDescribeForAgentSkipsMissingDocuments(t *testing.T) {
	svc := newAgentPromptService(readyDoc("doc-1", 10, "正文"))
	got, err := svc.DescribeForAgent(context.Background(), 7, "sess-1", []string{"doc-1", "ghost"})
	require.NoError(t, err)
	require.Len(t, got.Attachments, 1)
	assert.Equal(t, "doc-1", got.Attachments[0].ID)
}

func TestDescribeForAgentDedupesAndCapsIDs(t *testing.T) {
	svc := newAgentPromptService(readyDoc("doc-1", 10, "正文"))

	got, err := svc.DescribeForAgent(context.Background(), 7, "sess-1", []string{"doc-1", "doc-1"})
	require.NoError(t, err)
	assert.Len(t, got.Attachments, 1)

	tooMany := make([]string, types.MaxTemporaryAttachmentsPerMessage+1)
	for i := range tooMany {
		tooMany[i] = "doc-1"
	}
	// 去重发生在限量检查之后，因此超限必须直接报错而不是静默截断。
	_, err = svc.DescribeForAgent(context.Background(), 7, "sess-1", tooMany)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most")
}

// 独立图片上传才自动进视觉上下文；文档内嵌图片由 Agent 按需读取。
func TestDescribeForAgentOnlyAttachesStandaloneImages(t *testing.T) {
	image := readyDoc("img-1", 10, "图片描述文本")
	image.FileType = ".png"
	image.MimeType = "image/png"
	image.ImageRefs = types.JSON(`[{"url":"provider://img-1.png"}]`)

	doc := readyDoc("doc-1", 10, "文档正文 ![](provider://doc-1-figure.png)")
	doc.ImageRefs = types.JSON(`[{"url":"provider://doc-1-figure.png"}]`)

	svc := newAgentPromptService(image, doc)
	got, err := svc.DescribeForAgent(context.Background(), 7, "sess-1", []string{"img-1", "doc-1"})
	require.NoError(t, err)
	assert.Equal(t, []string{"provider://img-1.png"}, got.ImageURLs)
}

// 内联时文档里的图片引用必须带编号，模型才知道该读 /images/几。
func TestDescribeForAgentNumbersInlinedImages(t *testing.T) {
	doc := readyDoc("doc-1", 10, "看这张图 ![](provider://fig.png)")
	doc.ImageRefs = types.JSON(`[{"url":"provider://fig.png"}]`)

	svc := newAgentPromptService(doc)
	got, err := svc.DescribeForAgent(context.Background(), 7, "sess-1", []string{"doc-1"})
	require.NoError(t, err)
	require.Len(t, got.Attachments, 1)
	assert.Contains(t, got.Attachments[0].Content, "![](provider://fig.png) [image #1]")
}

// ResolveForPrompt（快速问答路径）保持原有语义：未就绪直接报错，由调用方决定跳过。
func TestResolveForPromptStillRejectsUnreadyDocuments(t *testing.T) {
	pending := readyDoc("doc-1", 10, "正文")
	pending.Status = types.TemporaryDocumentStatusProcessing

	svc := newAgentPromptService(pending)
	_, err := svc.ResolveForPrompt(context.Background(), 7, "sess-1", []string{"doc-1"}, "问题")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still being processed")

	failed := readyDoc("doc-2", 10, "正文")
	failed.Status = types.TemporaryDocumentStatusFailed
	failed.ErrorMessage = "解析失败"
	svc = newAgentPromptService(failed)
	_, err = svc.ResolveForPrompt(context.Background(), 7, "sess-1", []string{"doc-2"}, "问题")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse")
}
