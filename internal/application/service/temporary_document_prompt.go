package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	temporaryDocumentInlineTokens   = 12000
	temporaryDocumentPromptBudget   = 12000
	temporaryDocumentMaxPromptParts = 16
	// agentAttachmentInlineBudget 限制的是一整轮里 Agent 内联的小附件 token 总量，
	// 不是单篇预算：超出的文档一律转为按需读取（PRD FR-22 ③④）。
	agentAttachmentInlineBudget         = 6000
	temporaryDocumentOutlineMaxHeadings = 30
	temporaryDocumentOutlineMaxRunes    = 1500
)

func (s *temporaryDocumentService) ResolveForPrompt(
	ctx context.Context, tenantID uint64, sessionID string, documentIDs []string, query string,
) (*types.TemporaryDocumentPromptResult, error) {
	result := &types.TemporaryDocumentPromptResult{}
	if len(documentIDs) > types.MaxTemporaryAttachmentsPerMessage {
		return nil, fmt.Errorf("a message can use at most %d attachments", types.MaxTemporaryAttachmentsPerMessage)
	}
	perDocumentBudget := temporaryDocumentPromptBudget
	if len(documentIDs) > 0 {
		perDocumentBudget = temporaryDocumentPromptBudget / len(documentIDs)
	}
	seen := make(map[string]struct{}, len(documentIDs))
	for _, documentID := range documentIDs {
		if _, duplicate := seen[documentID]; duplicate {
			continue
		}
		seen[documentID] = struct{}{}
		document, err := s.repo.GetScoped(ctx, tenantID, sessionID, documentID)
		if err != nil {
			return nil, err
		}
		if document == nil {
			return nil, fmt.Errorf("attachment %s was not found in this session", documentID)
		}
		if document.Status != types.TemporaryDocumentStatusReady {
			if document.Status == types.TemporaryDocumentStatusFailed {
				return nil, fmt.Errorf("attachment %s failed to parse: %s", document.FileName, document.ErrorMessage)
			}
			return nil, fmt.Errorf("attachment %s is still being processed", document.FileName)
		}
		content, selected, total := selectTemporaryDocumentContentWithBudget(document, query, perDocumentBudget)
		result.Attachments = append(result.Attachments, types.MessageAttachment{
			ID: document.ID, URL: document.ResourceRef, FileName: document.FileName,
			FileType: document.FileType, FileSize: document.FileSize, Content: content,
			ContentMode: map[bool]string{
				true:  types.AttachmentContentModeFull,
				false: "selected_chunks",
			}[selected == total],
			TokenCount: document.TokenCount, SelectedChunks: selected, TotalChunks: total,
		})
		// Image-type attachments always expose their image so vision models can
		// see it directly; text documents only attach extracted images when the
		// question is visual, to avoid gratuitous multimodal latency.
		if docparser.IsImageFormat(document.FileType) || isVisualDocumentQuery(query) {
			for _, image := range temporaryDocumentImageRefs(document.ImageRefs) {
				if image.URL != "" && len(result.ImageURLs) < 4 {
					result.ImageURLs = append(result.ImageURLs, image.URL)
				}
			}
		}
	}
	return result, nil
}

// DescribeForAgent 为 Agent 轮次返回附件的清单视图（PRD FR-22 ③）。
// 与 ResolveForPrompt 不同，它既不等待解析完成，也不按问题挑选内容：
// Agent 通过 read_file 自己按需读取，只有"小且已就绪"的文档才整篇内联（④）。
func (s *temporaryDocumentService) DescribeForAgent(
	ctx context.Context, tenantID uint64, sessionID string, documentIDs []string,
) (*types.TemporaryDocumentPromptResult, error) {
	result := &types.TemporaryDocumentPromptResult{}
	if len(documentIDs) > types.MaxTemporaryAttachmentsPerMessage {
		return nil, fmt.Errorf("a message can use at most %d attachments", types.MaxTemporaryAttachmentsPerMessage)
	}
	inlineBudget := agentAttachmentInlineBudget
	seen := make(map[string]struct{}, len(documentIDs))
	for _, documentID := range documentIDs {
		if _, duplicate := seen[documentID]; duplicate {
			continue
		}
		seen[documentID] = struct{}{}
		document, err := s.repo.GetScoped(ctx, tenantID, sessionID, documentID)
		if err != nil {
			return nil, err
		}
		if document == nil {
			continue
		}
		attachment := types.MessageAttachment{
			ID: document.ID, URL: document.ResourceRef, FileName: document.FileName,
			FileType: document.FileType, FileSize: document.FileSize,
			TokenCount: document.TokenCount, TotalChunks: document.ChunkCount,
			ContentMode: types.AttachmentContentModeOnDemand,
		}
		if document.Status == types.TemporaryDocumentStatusReady {
			images := document.ViewableImages()
			attachment.SelectedChunks = document.ChunkCount
			if document.TokenCount <= types.AgentAttachmentInlineTokens && document.TokenCount <= inlineBudget {
				attachment.Content = types.NumberAttachmentImages(document.Content, images)
				attachment.ContentMode = types.AttachmentContentModeFull
				inlineBudget -= document.TokenCount
			} else {
				attachment.Content = agentAttachmentReference(document, images)
			}
			if docparser.IsImageFormat(document.FileType) {
				for _, image := range temporaryDocumentImageRefs(document.ImageRefs) {
					if image.URL != "" && len(result.ImageURLs) < 4 {
						result.ImageURLs = append(result.ImageURLs, image.URL)
					}
				}
			}
		} else {
			attachment.Content = agentAttachmentReference(document, nil)
		}
		result.Attachments = append(result.Attachments, attachment)
	}
	return result, nil
}

// agentAttachmentReference 渲染按需附件的引用块。它写进 MessageAttachment.Content，
// 由既有的 BuildPrompt 包进 <attachments> 交给模型：模型据此知道用哪个
// attachment:// 地址、当前解析到什么状态、以及文档大致结构。
func agentAttachmentReference(document *types.TemporaryDocument, images []types.TemporaryDocumentImage) string {
	path := types.AttachmentPath(document.ID)
	var b strings.Builder
	b.WriteString("[on_demand] 本附件未内联，需要时用 read_file 按路径读取。\n")
	fmt.Fprintf(&b, "path: %s\n", path)
	fmt.Fprintf(&b, "status: %s\n", document.Status)
	switch document.Status {
	case types.TemporaryDocumentStatusFailed:
		fmt.Fprintf(&b, "error: %s\n", strings.TrimSpace(document.ErrorMessage))
	case types.TemporaryDocumentStatusUploaded, types.TemporaryDocumentStatusProcessing:
		b.WriteString("note: 仍在后台解析。read_file 会在需要时短暂等待；未就绪时先处理任务的其他部分。\n")
	}
	if document.TokenCount > 0 {
		fmt.Fprintf(&b, "scale: %d tokens / %d chunks\n", document.TokenCount, document.ChunkCount)
	}
	if len(images) > 0 {
		fmt.Fprintf(&b, "images: %d（%s/images/1 … %s/images/%d）\n",
			len(images), path, path, len(images))
	}
	b.WriteString("read: read_file path=" + path + " 读解析文本；path=" + path +
		"/images/N 读第 N 张图。\n")
	if outline := temporaryDocumentOutline(document.Content); outline != "" {
		b.WriteString("outline:\n")
		b.WriteString(outline)
		b.WriteString("\n")
	}
	return b.String()
}

var markdownHeadingPattern = regexp.MustCompile(`^#{1,4}\s+\S`)

// temporaryDocumentOutline 列出文档靠前的 markdown 标题，
// 让 Agent 在取内容之前就能判断该读哪里。
func temporaryDocumentOutline(content string) string {
	var lines []string
	runes := 0
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !markdownHeadingPattern.MatchString(line) {
			continue
		}
		runes += utf8.RuneCountInString(line)
		if len(lines) >= temporaryDocumentOutlineMaxHeadings || runes > temporaryDocumentOutlineMaxRunes {
			lines = append(lines, "…")
			break
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func selectTemporaryDocumentContent(document *types.TemporaryDocument, query string) (string, int, int) {
	return selectTemporaryDocumentContentWithBudget(document, query, temporaryDocumentPromptBudget)
}

func selectTemporaryDocumentContentWithBudget(
	document *types.TemporaryDocument, query string, budget int,
) (string, int, int) {
	var chunks []types.TemporaryDocumentChunk
	_ = json.Unmarshal(document.Chunks, &chunks)
	if budget <= 0 {
		budget = temporaryDocumentPromptBudget
	}
	if len(chunks) == 0 || (document.TokenCount <= temporaryDocumentInlineTokens && document.TokenCount <= budget) {
		return document.Content, len(chunks), len(chunks)
	}
	terms := temporaryDocumentQueryTerms(query)
	type rankedChunk struct {
		chunk types.TemporaryDocumentChunk
		score int
	}
	ranked := make([]rankedChunk, 0, len(chunks))
	for _, part := range chunks {
		text := strings.ToLower(part.ContextHeader + "\n" + part.Content)
		score := 0
		for _, term := range terms {
			score += strings.Count(text, term) * (1 + len([]rune(term))/2)
		}
		ranked = append(ranked, rankedChunk{chunk: part, score: score})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score == ranked[j].score {
			return ranked[i].chunk.Seq < ranked[j].chunk.Seq
		}
		return ranked[i].score > ranked[j].score
	})
	selected := make([]types.TemporaryDocumentChunk, 0, temporaryDocumentMaxPromptParts)
	tokens := 0
	for _, candidate := range ranked {
		if len(selected) >= temporaryDocumentMaxPromptParts {
			break
		}
		if tokens > 0 && tokens+candidate.chunk.TokenCount > budget {
			continue
		}
		selected = append(selected, candidate.chunk)
		tokens += candidate.chunk.TokenCount
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Seq < selected[j].Seq })
	var builder strings.Builder
	for _, part := range selected {
		if builder.Len() > 0 {
			builder.WriteString("\n\n---\n\n")
		}
		if part.ContextHeader != "" {
			builder.WriteString(part.ContextHeader)
			builder.WriteString("\n\n")
		}
		builder.WriteString(strings.TrimSpace(part.Content))
	}
	return builder.String(), len(selected), len(chunks)
}

func temporaryDocumentQueryTerms(query string) []string {
	query = strings.ToLower(strings.TrimSpace(query))
	seen := make(map[string]struct{})
	var terms []string
	for _, field := range strings.FieldsFunc(query, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	}) {
		if len([]rune(field)) < 2 {
			continue
		}
		if _, ok := seen[field]; !ok {
			seen[field] = struct{}{}
			terms = append(terms, field)
		}
	}
	runes := []rune(query)
	for i := 0; i+1 < len(runes); i++ {
		if unicode.Is(unicode.Han, runes[i]) && unicode.Is(unicode.Han, runes[i+1]) {
			term := string(runes[i : i+2])
			if _, ok := seen[term]; !ok {
				seen[term] = struct{}{}
				terms = append(terms, term)
			}
		}
	}
	return terms
}

func isVisualDocumentQuery(query string) bool {
	lower := strings.ToLower(query)
	for _, marker := range []string{"图", "表格", "截图", "页面", "排版", "chart", "figure", "diagram", "image", "layout"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
