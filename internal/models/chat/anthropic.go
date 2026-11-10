package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Tencent/WeKnora/internal/models/provider"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

const anthropicVersion = "2023-06-01"

type AnthropicChat struct {
	modelName     string
	modelID       string
	baseURL       string
	apiKey        string
	customHeaders map[string]string
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content anthropicContent `json:"content"`
}

// anthropicTextBlock 是 Anthropic Messages 协议的文本内容块。CacheControl 非空时
// 该块就是一个缓存断点（FR-11 ③）：断点及其之前的内容构成可复用的稳定前缀。
type anthropicTextBlock struct {
	Type         string              `json:"type"`
	Text         string              `json:"text"`
	CacheControl *CacheControlMarker `json:"cache_control,omitempty"`
}

// anthropicContent 兼容 Anthropic 的两种内容写法：纯字符串（不带缓存标记，序列化
// 结果与改造前逐字节一致）与文本块数组（带 cache_control 断点）。
type anthropicContent struct {
	Text   string
	Blocks []anthropicTextBlock
}

// newAnthropicText 构造纯字符串内容。
func newAnthropicText(text string) anthropicContent { return anthropicContent{Text: text} }

// newAnthropicCachedText 构造带缓存断点的文本内容；marker 为 nil 或文本为空时
// 退化为纯字符串，不改变请求形态。
func newAnthropicCachedText(text string, marker *CacheControlMarker) anthropicContent {
	if marker == nil || text == "" {
		return anthropicContent{Text: text}
	}
	return anthropicContent{Blocks: []anthropicTextBlock{{Type: "text", Text: text, CacheControl: marker}}}
}

// MarshalJSON 无缓存断点时输出字符串，有断点时输出内容块数组。
func (c anthropicContent) MarshalJSON() ([]byte, error) {
	if len(c.Blocks) == 0 {
		return json.Marshal(c.Text)
	}
	return json.Marshal(c.Blocks)
}

// UnmarshalJSON 同时接受字符串与内容块数组两种写法（便于测试与网关回放）。
func (c *anthropicContent) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		c.Text, c.Blocks = text, nil
		return nil
	}
	var blocks []anthropicTextBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return err
	}
	c.Text, c.Blocks = "", blocks
	return nil
}

type anthropicRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	Stream    bool   `json:"stream,omitempty"`
	// System 用指针以便 omitempty 生效：没有系统消息时整个字段不下发，
	// 与改造前的请求字节保持一致。
	System      *anthropicContent  `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
}

type anthropicResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens              int  `json:"input_tokens"`
		OutputTokens             int  `json:"output_tokens"`
		CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage struct {
			InputTokens              int  `json:"input_tokens"`
			OutputTokens             int  `json:"output_tokens"`
			CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message,omitempty"`
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta,omitempty"`
	Usage *struct {
		InputTokens              int  `json:"input_tokens"`
		OutputTokens             int  `json:"output_tokens"`
		CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	} `json:"usage,omitempty"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func NewAnthropicChat(config *ChatConfig) (*AnthropicChat, error) {
	if config.BaseURL != "" {
		if err := secutils.ValidateURLForSSRF(config.BaseURL); err != nil {
			return nil, fmt.Errorf("baseURL SSRF check failed: %w", err)
		}
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, fmt.Errorf("Anthropic provider: API key is required")
	}

	baseURL := strings.TrimRight(config.BaseURL, "/")
	if baseURL == "" {
		baseURL = provider.AnthropicBaseURL
	}

	return &AnthropicChat{
		modelName:     config.ModelName,
		modelID:       config.ModelID,
		baseURL:       baseURL,
		apiKey:        config.APIKey,
		customHeaders: config.CustomHeaders,
	}, nil
}

func (c *AnthropicChat) Chat(ctx context.Context, messages []Message, opts *ChatOptions) (*types.ChatResponse, error) {
	reqBody := c.buildRequest(messages, opts)
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	ctx, cancel := withLLMTimeout(ctx, defaultChatTimeout)
	defer cancel()

	endpoint := c.endpoint()
	if err := secutils.ValidateURLForSSRF(endpoint); err != nil {
		return nil, fmt.Errorf("endpoint SSRF check failed: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	secutils.ApplyCustomHeaders(httpReq, c.customHeaders)
	// 会话亲和 header：让同一会话的多轮请求落到同一缓存分片（FR-11 ③）。
	AttachSessionAffinityHeaders(httpReq, promptCacheRoutingKey(opts))

	resp, err := rawHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	// 响应体受上界约束（FR-07）：超限直接报错，不把无界内容读进内存。
	body, err := secutils.ReadResponseCapped(resp.Body, secutils.ModelResponseLimit())
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		chatResp, err := parseAnthropicSSE(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, chatResp.Content)
		}
		logUsage(ctx, c.modelName, &chatResp.Usage)
		return chatResp, nil
	}

	var chatResp anthropicResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if chatResp.Error != nil && chatResp.Error.Message != "" {
			return nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, chatResp.Error.Message)
		}
		return nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, secutils.ErrorPreviewString(body))
	}

	result := c.parseResponse(&chatResp)
	logUsage(ctx, c.modelName, &result.Usage)
	return result, nil
}

func (c *AnthropicChat) ChatStream(ctx context.Context, messages []Message, opts *ChatOptions) (<-chan types.StreamResponse, error) {
	reqBody := c.buildRequest(messages, opts)
	reqBody.Stream = true
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	endpoint := c.endpoint()
	if err := secutils.ValidateURLForSSRF(endpoint); err != nil {
		return nil, fmt.Errorf("endpoint SSRF check failed: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	secutils.ApplyCustomHeaders(httpReq, c.customHeaders)
	// 会话亲和 header：让同一会话的多轮请求落到同一缓存分片（FR-11 ③）。
	AttachSessionAffinityHeaders(httpReq, promptCacheRoutingKey(opts))

	resp, err := rawHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		preview := secutils.ReadErrorPreview(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, preview)
	}

	streamChan := make(chan types.StreamResponse)
	go processAnthropicStream(ctx, c.modelName, resp, streamChan)
	return streamChan, nil
}

func (c *AnthropicChat) GetModelName() string {
	return c.modelName
}

func (c *AnthropicChat) GetModelID() string {
	return c.modelID
}

func (c *AnthropicChat) endpoint() string {
	baseURL := strings.TrimRight(c.baseURL, "/")
	if isAnthropicMessagesEndpoint(baseURL) {
		return baseURL
	}
	if isAnthropicVersionedBaseURL(baseURL) {
		return baseURL + "/messages"
	}
	return baseURL + "/v1/messages"
}

func isAnthropicMessagesEndpoint(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	path := strings.TrimRight(u.Path, "/")
	return strings.HasSuffix(path, "/messages")
}

func isAnthropicVersionedBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	path := strings.TrimRight(u.Path, "/")
	return strings.HasSuffix(path, "/v1") || strings.HasSuffix(path, "/v1beta")
}

func (c *AnthropicChat) buildRequest(messages []Message, opts *ChatOptions) anthropicRequest {
	req := anthropicRequest{
		Model:     c.modelName,
		MaxTokens: 1024,
		Messages:  make([]anthropicMessage, 0, len(messages)),
	}
	if opts != nil {
		if opts.MaxTokens > 0 {
			req.MaxTokens = opts.MaxTokens
		} else if opts.MaxCompletionTokens > 0 {
			req.MaxTokens = opts.MaxCompletionTokens
		}
		if opts.Temperature > 0 {
			temperature := opts.Temperature
			req.Temperature = &temperature
		}
		if opts.TopP > 0 {
			topP := opts.TopP
			req.TopP = &topP
		}
	}

	var systemParts []string
	for _, msg := range messages {
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			content = textFromMultiContent(msg.MultiContent)
		}
		if content == "" {
			continue
		}
		switch msg.Role {
		case "system":
			systemParts = append(systemParts, content)
		case "assistant":
			req.Messages = append(req.Messages, anthropicMessage{Role: "assistant", Content: newAnthropicText(content)})
		case "user":
			req.Messages = append(req.Messages, anthropicMessage{Role: "user", Content: newAnthropicText(content)})
		default:
			req.Messages = append(req.Messages, anthropicMessage{Role: "user", Content: newAnthropicText(content)})
		}
	}

	// 缓存断点（FR-11 ③）：Anthropic 的 prompt 缓存必须显式标记，不会自动生效。
	// 断点打在稳定前缀的边界上——system（指令 + 记忆常驻块，跨轮不变）与最后一条
	// 会话消息（让下一轮复用本轮之前的全部对话）。CacheRetentionNone 时 marker 为
	// nil，请求形态与改造前完全一致。
	marker := CacheControlFor(ResolveCacheRetention(opts), LongCacheTTL)
	if system := strings.Join(systemParts, "\n\n"); system != "" {
		cached := newAnthropicCachedText(system, marker)
		req.System = &cached
	}
	if marker != nil && len(req.Messages) > 0 {
		last := &req.Messages[len(req.Messages)-1]
		last.Content = newAnthropicCachedText(last.Content.Text, marker)
	}
	return req
}

func textFromMultiContent(parts []MessageContentPart) string {
	if len(parts) == 0 {
		return ""
	}
	textParts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
			textParts = append(textParts, strings.TrimSpace(part.Text))
		}
	}
	return strings.Join(textParts, "\n")
}

func (c *AnthropicChat) parseResponse(resp *anthropicResponse) *types.ChatResponse {
	parts := make([]string, 0, len(resp.Content))
	for _, part := range resp.Content {
		if part.Type == "text" && part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	inputTokens := resp.Usage.InputTokens
	outputTokens := resp.Usage.OutputTokens
	cacheRead := valueOrZero(resp.Usage.CacheReadInputTokens)
	cacheWrite := valueOrZero(resp.Usage.CacheCreationInputTokens)
	promptTokens := inputTokens + cacheRead + cacheWrite
	usage := types.TokenUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: outputTokens,
		TotalTokens:      promptTokens + outputTokens,
	}
	usage.SetPromptCacheUsage(cacheRead, cacheWrite, max(0, promptTokens-cacheRead),
		resp.Usage.CacheReadInputTokens != nil || resp.Usage.CacheCreationInputTokens != nil)
	return &types.ChatResponse{
		Content:      strings.Join(parts, ""),
		FinishReason: resp.StopReason,
		Usage:        usage,
	}
}

func parseAnthropicSSE(reader io.Reader) (*types.ChatResponse, error) {
	sseReader := NewSSEReader(reader)
	var contentParts []string
	var finishReason string
	var inputTokens int
	var outputTokens int
	var cacheReadTokens int
	var cacheWriteTokens int
	var cacheReported bool

	for {
		event, err := sseReader.ReadEvent()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read SSE response: %w", err)
		}
		if event.Done {
			break
		}
		if len(event.Data) == 0 {
			continue
		}

		var streamEvent anthropicStreamEvent
		if err := json.Unmarshal(event.Data, &streamEvent); err != nil {
			return nil, fmt.Errorf("decode SSE response: %w", err)
		}
		if streamEvent.Error != nil && streamEvent.Error.Message != "" {
			return nil, fmt.Errorf("API stream error: %s", streamEvent.Error.Message)
		}
		if streamEvent.Message != nil {
			inputTokens = max(inputTokens, streamEvent.Message.Usage.InputTokens)
			outputTokens = max(outputTokens, streamEvent.Message.Usage.OutputTokens)
			cacheReadTokens, cacheWriteTokens, cacheReported = mergeAnthropicCacheCounters(
				cacheReadTokens, cacheWriteTokens, cacheReported,
				streamEvent.Message.Usage.CacheReadInputTokens,
				streamEvent.Message.Usage.CacheCreationInputTokens,
			)
		}
		if streamEvent.Delta != nil {
			if streamEvent.Delta.Type == "text_delta" && streamEvent.Delta.Text != "" {
				contentParts = append(contentParts, streamEvent.Delta.Text)
			}
			if streamEvent.Delta.StopReason != "" {
				finishReason = streamEvent.Delta.StopReason
			}
		}
		if streamEvent.Usage != nil {
			inputTokens = max(inputTokens, streamEvent.Usage.InputTokens)
			outputTokens = max(outputTokens, streamEvent.Usage.OutputTokens)
			cacheReadTokens, cacheWriteTokens, cacheReported = mergeAnthropicCacheCounters(
				cacheReadTokens, cacheWriteTokens, cacheReported,
				streamEvent.Usage.CacheReadInputTokens,
				streamEvent.Usage.CacheCreationInputTokens,
			)
		}
	}
	promptTokens := inputTokens + cacheReadTokens + cacheWriteTokens
	usage := types.TokenUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: outputTokens,
		TotalTokens:      promptTokens + outputTokens,
	}
	usage.SetPromptCacheUsage(cacheReadTokens, cacheWriteTokens,
		max(0, promptTokens-cacheReadTokens), cacheReported)

	return &types.ChatResponse{
		Content:      strings.Join(contentParts, ""),
		FinishReason: finishReason,
		Usage:        usage,
	}, nil
}

func processAnthropicStream(ctx context.Context, model string, resp *http.Response, streamChan chan types.StreamResponse) {
	defer close(streamChan)
	defer resp.Body.Close()

	sseReader := NewSSEReader(resp.Body)
	var usage *types.TokenUsage
	var finishReason string

	for {
		event, err := sseReader.ReadEvent()
		if err != nil {
			if err == io.EOF {
				logUsage(ctx, model, usage)
				streamChan <- types.StreamResponse{
					ResponseType: types.ResponseTypeAnswer,
					Content:      "",
					Done:         true,
					Usage:        usage,
					FinishReason: finishReason,
				}
			} else {
				streamChan <- types.StreamResponse{
					ResponseType: types.ResponseTypeError,
					Content:      err.Error(),
					Done:         true,
				}
			}
			return
		}
		if event.Done {
			logUsage(ctx, model, usage)
			streamChan <- types.StreamResponse{
				ResponseType: types.ResponseTypeAnswer,
				Content:      "",
				Done:         true,
				Usage:        usage,
				FinishReason: finishReason,
			}
			return
		}
		if len(event.Data) == 0 {
			continue
		}

		var streamEvent anthropicStreamEvent
		if err := json.Unmarshal(event.Data, &streamEvent); err != nil {
			streamChan <- types.StreamResponse{
				ResponseType: types.ResponseTypeError,
				Content:      fmt.Sprintf("decode SSE response: %v", err),
				Done:         true,
			}
			return
		}
		if streamEvent.Error != nil && streamEvent.Error.Message != "" {
			streamChan <- types.StreamResponse{
				ResponseType: types.ResponseTypeError,
				Content:      streamEvent.Error.Message,
				Done:         true,
			}
			return
		}
		if streamEvent.Message != nil {
			usage = mergeAnthropicUsage(usage, streamEvent.Message.Usage.InputTokens,
				streamEvent.Message.Usage.OutputTokens,
				streamEvent.Message.Usage.CacheReadInputTokens,
				streamEvent.Message.Usage.CacheCreationInputTokens)
		}
		if streamEvent.Delta != nil {
			if streamEvent.Delta.StopReason != "" {
				finishReason = streamEvent.Delta.StopReason
			}
			if streamEvent.Delta.Type == "text_delta" && streamEvent.Delta.Text != "" {
				streamChan <- types.StreamResponse{
					ResponseType: types.ResponseTypeAnswer,
					Content:      streamEvent.Delta.Text,
					Done:         false,
				}
			}
		}
		if streamEvent.Usage != nil {
			usage = mergeAnthropicUsage(usage, streamEvent.Usage.InputTokens,
				streamEvent.Usage.OutputTokens,
				streamEvent.Usage.CacheReadInputTokens,
				streamEvent.Usage.CacheCreationInputTokens)
		}
	}
}

func mergeAnthropicUsage(
	current *types.TokenUsage,
	inputTokens, outputTokens int,
	cacheRead, cacheWrite *int,
) *types.TokenUsage {
	if current == nil {
		current = &types.TokenUsage{}
	}
	read, write, reported := mergeAnthropicCacheCounters(
		current.CacheReadTokens, current.CacheWriteTokens, current.CacheReported,
		cacheRead, cacheWrite,
	)
	uncachedInput := max(0, current.PromptTokens-current.CacheReadTokens-current.CacheWriteTokens)
	uncachedInput = max(uncachedInput, inputTokens)
	current.PromptTokens = uncachedInput + read + write
	current.CompletionTokens = max(current.CompletionTokens, outputTokens)
	current.TotalTokens = current.PromptTokens + current.CompletionTokens
	current.SetPromptCacheUsage(read, write, max(0, current.PromptTokens-read), reported)
	return current
}

func mergeAnthropicCacheCounters(
	currentRead, currentWrite int,
	currentReported bool,
	cacheRead, cacheWrite *int,
) (read, write int, reported bool) {
	read = currentRead
	write = currentWrite
	reported = currentReported || cacheRead != nil || cacheWrite != nil
	if cacheRead != nil {
		read = max(read, *cacheRead)
	}
	if cacheWrite != nil {
		write = max(write, *cacheWrite)
	}
	return read, write, reported
}
