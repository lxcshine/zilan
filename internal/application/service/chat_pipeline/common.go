package chatpipeline

import (
	"context"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/Tencent/WeKnora/internal/common"
	"github.com/Tencent/WeKnora/internal/contextx"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

var regThinkTags = regexp.MustCompile(`(?s)<think>.*?</think>`)

const retrievedImageOutputRequirement = `

## Retrieved Image Output Requirement
The retrieved context for this turn contains Markdown images. Images attached to retrieved passages should be treated as relevant by default.
- Unless the user explicitly requests text-only output, or every retrieved image is clearly unrelated to the answer, the final answer MUST include at least one relevant Markdown image copied from the retrieved context.
- Copy the complete Markdown image syntax and its URL verbatim. Never invent, shorten, normalize, or replace the URL.
- Use ASCII half-width parentheses in image Markdown exactly as ![alt](url). Never use full-width （ or ）.
- Place each image immediately after the paragraph it supports, rather than collecting images at the end.
- When multiple retrieved images support different sections of a multi-section answer, include them in their corresponding sections instead of stopping after the first image.
- Before finishing, silently verify that the answer contains a Markdown image whenever this requirement applies.`

func appendRetrievedImageOutputRequirement(systemPrompt, renderedContexts string) string {
	if !searchutil.MarkdownImageRegex.MatchString(renderedContexts) {
		return systemPrompt
	}
	return strings.TrimRight(systemPrompt, " \t\r\n") + retrievedImageOutputRequirement
}

// appendMemoryContext appends the recalled long-term-memory block (rendered
// by the MEMORY_RECALL stage) to the system prompt. The block already carries
// its own section header; it is placed after the base prompt so persona and
// output instructions keep their primacy.
func appendMemoryContext(systemPrompt, memoryContext string) string {
	memoryContext = strings.TrimSpace(memoryContext)
	if memoryContext == "" {
		return systemPrompt
	}
	return strings.TrimRight(systemPrompt, " \t\r\n") + "\n\n" + memoryContext
}

// pipelineInfo logs pipeline info level entries.
func pipelineInfo(ctx context.Context, stage, action string, fields map[string]interface{}) {
	common.PipelineInfo(ctx, stage, action, fields)
}

// pipelineWarn logs pipeline warning level entries.
func pipelineWarn(ctx context.Context, stage, action string, fields map[string]interface{}) {
	common.PipelineWarn(ctx, stage, action, fields)
}

// pipelineError logs pipeline error level entries.
func pipelineError(ctx context.Context, stage, action string, fields map[string]interface{}) {
	common.PipelineError(ctx, stage, action, fields)
}

// prepareChatModel shared logic to prepare chat model and options
// it gets the chat model and sets up the chat options based on the chat manage.
func prepareChatModel(ctx context.Context, modelService interfaces.ModelService,
	chatManage *types.ChatManage,
) (chat.Chat, *chat.ChatOptions, error) {
	chatModel, err := modelService.GetChatModel(ctx, chatManage.ChatModelID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get chat model: %v", err)
		return nil, nil, err
	}
	// Stash the resolved model name so downstream context budgeting can pick
	// the correct vendor tokenizer and default context window.
	if chatModel != nil {
		chatManage.ChatModelName = chatModel.GetModelName()
	}

	opt := &chat.ChatOptions{
		Temperature:         chatManage.SummaryConfig.Temperature,
		TopP:                chatManage.SummaryConfig.TopP,
		Seed:                chatManage.SummaryConfig.Seed,
		MaxTokens:           chatManage.SummaryConfig.MaxTokens,
		MaxCompletionTokens: chatManage.SummaryConfig.MaxCompletionTokens,
		FrequencyPenalty:    chatManage.SummaryConfig.FrequencyPenalty,
		PresencePenalty:     chatManage.SummaryConfig.PresencePenalty,
		Thinking:            chatManage.SummaryConfig.Thinking,
	}
	if opt.Thinking != nil {
		pipelineInfo(ctx, "Stream", "thinking_option", map[string]interface{}{
			"enabled": *opt.Thinking,
		})
	}

	return chatModel, opt, nil
}

// prepareMessagesWithHistory prepare complete messages including history.
// When SystemPromptOverride is set (e.g. by intent-specific prompt logic),
// it takes precedence over the default SummaryConfig.Prompt.
//
// When the tenant opts into the smart compression strategy, message assembly
// goes through the five-layer context-budget architecture (see
// prepareMessagesFiveLayer) instead of naive concatenation.
func prepareMessagesWithHistory(ctx context.Context, chatManage *types.ChatManage) []chat.Message {
	if smartCompressionEnabled(ctx, chatManage) {
		return prepareMessagesFiveLayer(ctx, chatManage)
	}

	base := chatManage.SummaryConfig.Prompt
	if chatManage.SystemPromptOverride != "" {
		base = chatManage.SystemPromptOverride
	}
	systemPrompt := types.RenderPromptPlaceholders(base, types.PlaceholderValues{
		"query":    chatManage.Query,
		"language": chatManage.Language,
		"contexts": chatManage.RenderedContexts,
	})
	systemPrompt = appendMemoryContext(systemPrompt, chatManage.MemoryContext)
	systemPrompt = appendRetrievedImageOutputRequirement(systemPrompt, chatManage.RenderedContexts)

	chatMessages := []chat.Message{
		{Role: "system", Content: systemPrompt},
	}

	chatMessages = AppendHistoryMessages(chatMessages, chatManage.History)

	// Add current user message. Only include images when the chat model supports
	// vision; non-vision models rely on the text description in UserContent.
	userMsg := chat.Message{Role: "user", Content: chatManage.UserContent}
	if chatManage.ChatModelSupportsVision && len(chatManage.Images) > 0 {
		userMsg.Images = chatManage.Images
	}
	chatMessages = append(chatMessages, userMsg)

	return chatMessages
}

// prepareMessagesFiveLayer assembles the prompt under the five-layer
// context-budget architecture (ima-grade):
//
//	L0 system    — base prompt; retrieval placeholder rendered empty (L2 owns
//	               retrieval content, single-injected into the user turn)
//	L1 memory    — long-term memory block, relevance-truncated to its share
//	L2 retrieval — tiered contexts; shrunk by relevance tiering when over budget
//	L3 history   — sticky + recent rounds + running summary of older rounds
//	L4 query     — current user content (retrieval block excluded, re-attached
//	               after budgeting so the registry replacement still matches)
//
// The resolved per-layer spend is recorded in chatManage.ContextDiagnostics.
func prepareMessagesFiveLayer(ctx context.Context, chatManage *types.ChatManage) []chat.Message {
	counter := contextx.CounterForModel(chatManage.ChatModelName, "", "")
	vendor := contextx.VendorFromStrings(chatManage.ChatModelName, "", "")

	explicitWindow := 0
	if cc := contextConfigFromContext(ctx, chatManage); cc != nil {
		explicitWindow = cc.MaxTokens
	}
	window := contextx.ResolveContextWindow(explicitWindow, nil, vendor, chatManage.ChatModelName)

	// L0 system — retrieval placeholder intentionally empty in smart mode.
	base := chatManage.SummaryConfig.Prompt
	if chatManage.SystemPromptOverride != "" {
		base = chatManage.SystemPromptOverride
	}
	systemPrompt := types.RenderPromptPlaceholders(base, types.PlaceholderValues{
		"query":    chatManage.Query,
		"language": chatManage.Language,
		"contexts": "",
	})
	systemPrompt = appendRetrievedImageOutputRequirement(systemPrompt, chatManage.RenderedContexts)

	// L2/L4 split: the retrieval block was injected into UserContent by
	// INTO_CHAT_MESSAGE; separate it so each layer is budgeted independently.
	retrieval := chatManage.RenderedContexts
	query := chatManage.UserContent
	if retrieval != "" {
		query = strings.TrimSpace(strings.Replace(query, retrieval, "", 1))
	}

	// L3 history turns.
	turns := make([]contextx.Turn, 0, len(chatManage.History))
	for i, h := range chatManage.History {
		if h == nil {
			continue
		}
		turns = append(turns, contextx.Turn{User: h.Query, Assistant: h.Answer, Ref: i})
	}

	asm := contextx.NewAssembler(counter)
	result := asm.Assemble(contextx.Input{
		System:         systemPrompt,
		Memory:         chatManage.MemoryContext,
		Retrieval:      retrieval,
		History:        turns,
		HistorySummary: chatManage.HistorySummary,
		Query:          query,
		Intent:         intentClassFor(chatManage.Intent),
		Window:         window,
		ReserveOutput:  reserveOutputFor(chatManage),
		ShrinkRetrieval: func(budgetTokens int) string {
			shrinker := &contextx.ShrinkRetrievalFromResults{
				Results:    searchResultsToTierResults(ctx, chatManage.MergeResult),
				Thresholds: contextx.DefaultTierThresholds(),
				Format:     contextx.CitationFormat{IncludeHeadingPath: true},
			}
			return shrinker.Shrink(ctx, budgetTokens, counter)
		},
	})

	// Keep RenderedContexts in sync with the budgeted retrieval block so the
	// downstream modelcontext registry replacement matches exactly.
	chatManage.RenderedContexts = result.Retrieval
	chatManage.ContextDiagnostics = diagToTypes(result.Diag)

	// Render the final message list: system = L0 + L1, user = L2 + L4.
	systemFinal := appendMemoryContext(result.System, result.Memory)
	userFinal := result.Query
	if result.Retrieval != "" {
		if userFinal != "" {
			userFinal = result.Retrieval + "\n\n" + userFinal
		} else {
			userFinal = result.Retrieval
		}
	}

	chatMessages := []chat.Message{{Role: "system", Content: systemFinal}}
	for _, t := range result.History {
		if strings.TrimSpace(t.User) != "" {
			chatMessages = append(chatMessages, chat.Message{Role: "user", Content: t.User})
		}
		if strings.TrimSpace(t.Assistant) != "" {
			chatMessages = append(chatMessages, chat.Message{Role: "assistant", Content: t.Assistant})
		}
	}
	userMsg := chat.Message{Role: "user", Content: userFinal}
	if chatManage.ChatModelSupportsVision && len(chatManage.Images) > 0 {
		userMsg.Images = chatManage.Images
	}
	chatMessages = append(chatMessages, userMsg)

	pipelineInfo(ctx, "ContextBudget", "assembled", map[string]interface{}{
		"session_id": chatManage.SessionID,
		"window":     result.Diag.Window,
		"usable":     result.Diag.Usable,
		"intent":     string(result.Diag.Intent),
		"actions":    len(result.Diag.Actions),
	})
	return chatMessages
}

func withPromptCacheMetadata(
	ctx context.Context,
	chatModel chat.Chat,
	messages []chat.Message,
	opts *chat.ChatOptions,
	purpose string,
) context.Context {
	prefixFingerprint := chat.PromptPrefixFingerprint(messages, opts)
	_ = chatModel // model identity is already captured by the usage sink
	return types.WithLLMCallMetadata(ctx, purpose, prefixFingerprint)
}

// AppendHistoryMessages appends prior Q&A rounds in chronological order.
// History is already filtered and truncated upstream by the load_history plugin.
func AppendHistoryMessages(messages []chat.Message, history []*types.History) []chat.Message {
	for _, history := range history {
		messages = append(messages, chat.Message{Role: "user", Content: history.Query})
		messages = append(messages, chat.Message{Role: "assistant", Content: history.Answer})
	}
	return messages
}

// loadAndProcessHistory fetches recent messages, groups them into Q&A pairs,
// strips <think> tags from assistant answers, sorts by recency, and limits to maxRounds.
// fetchCount controls how many raw messages to fetch (typically maxRounds*2+10).
func loadAndProcessHistory(
	ctx context.Context,
	messageService interfaces.MessageService,
	sessionID string,
	maxRounds int,
	fetchCount int,
) ([]*types.History, error) {
	history, err := messageService.GetRecentMessagesBySession(ctx, sessionID, fetchCount)
	if err != nil {
		return nil, err
	}

	historyMap := make(map[string]*types.History)
	for _, message := range history {
		h, ok := historyMap[message.RequestID]
		if !ok {
			h = &types.History{}
		}
		if message.Role == "user" {
			// RenderedContent is a snapshot of the prompt/context format used by
			// the original turn. Replaying it would mix legacy <context id="…">
			// envelopes and old citation instructions into the current protocol.
			// Historical references are carried separately in KnowledgeReferences
			// and can be re-merged into this turn's freshly rendered context.
			h.Query = message.Content
			h.CreateAt = message.CreatedAt
			if desc := extractImageCaptions(message.Images); desc != "" {
				h.Query += "\n\n[用户上传图片内容]\n" + desc
			}
			if len(message.Attachments) > 0 {
				h.Query += message.Attachments.BuildPrompt()
			}
		} else {
			h.Answer = regThinkTags.ReplaceAllString(message.Content, "")
			h.KnowledgeReferences = message.KnowledgeReferences
		}
		historyMap[message.RequestID] = h
	}

	historyList := make([]*types.History, 0, len(historyMap))
	for _, h := range historyMap {
		if h.Answer != "" && h.Query != "" {
			historyList = append(historyList, h)
		}
	}

	sort.Slice(historyList, func(i, j int) bool {
		return historyList[i].CreateAt.After(historyList[j].CreateAt)
	})

	if len(historyList) > maxRounds {
		historyList = historyList[:maxRounds]
	}

	slices.Reverse(historyList)
	return historyList, nil
}

// extractImageCaptions concatenates non-empty Caption fields from stored
// message images. Used when loading history so that previous turns' image
// descriptions are visible to the model.
func extractImageCaptions(images types.MessageImages) string {
	var parts []string
	for _, img := range images {
		if img.Caption != "" {
			parts = append(parts, img.Caption)
		}
	}
	return strings.Join(parts, "\n")
}

// ---------------------------------------------------------------------------
// Concurrency utilities
// ---------------------------------------------------------------------------

// ParallelTask represents a named unit of concurrent work.
type ParallelTask struct {
	Name string
	Run  func() *PluginError
}

// RunParallel executes tasks concurrently.
// Returns a map of task name → error for tasks that returned non-nil errors.
func RunParallel(tasks ...ParallelTask) map[string]*PluginError {
	errs := make(map[string]*PluginError)
	var mu sync.Mutex
	var wg sync.WaitGroup

	wg.Add(len(tasks))
	for _, task := range tasks {
		go func(t ParallelTask) {
			defer wg.Done()
			if err := t.Run(); err != nil {
				mu.Lock()
				errs[t.Name] = err
				mu.Unlock()
			}
		}(task)
	}
	wg.Wait()
	return errs
}

// ParallelMap applies fn to each element of items concurrently (up to
// maxWorkers goroutines) and returns results in the same order as items.
// If maxWorkers <= 0, concurrency is unbounded (one goroutine per item).
func ParallelMap[T, R any](items []T, maxWorkers int, fn func(int, T) R) []R {
	n := len(items)
	if n == 0 {
		return nil
	}
	results := make([]R, n)

	if maxWorkers <= 0 || maxWorkers > n {
		maxWorkers = n
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxWorkers)

	for i, item := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, it T) {
			defer func() { <-sem; wg.Done() }()
			results[idx] = fn(idx, it)
		}(i, item)
	}
	wg.Wait()
	return results
}
