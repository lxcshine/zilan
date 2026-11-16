package vlm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/provider"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	openai "github.com/sashabaranov/go-openai"
)

const (
	// defaultTimeout is the fallback HTTP timeout for a single VLM request.
	// Dense scanned-PDF OCR (full-page text + layout extraction) can take well
	// over a minute on slow endpoints, so this is intentionally generous and
	// can be raised further via VLM_HTTP_TIMEOUT_SECONDS.
	defaultTimeout = 180 * time.Second
	defaultMaxToks = 5000
	defaultTemp    = float32(0.1)
)

// vlmHTTPTimeout returns the HTTP client timeout for VLM requests, read from
// the VLM_HTTP_TIMEOUT_SECONDS env var when set (and positive), falling back to
// defaultTimeout otherwise. Shared by all OpenAI-compatible VLM backends.
func vlmHTTPTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("VLM_HTTP_TIMEOUT_SECONDS")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultTimeout
}

// RemoteAPIVLM implements VLM via an OpenAI-compatible chat completions API.
type RemoteAPIVLM struct {
	modelName   string
	modelID     string
	client      *openai.Client
	baseURL     string
	apiKey      string
	temperature float32
	// thinkingDisabled 让请求显式关闭推理（thinking.type=disabled）。
	// 推理型视觉模型（如 DeepSeek-V4 系列）默认会先产出大段 reasoning，
	// 把 max_tokens 全部耗尽，content 返回空字符串——对 OCR/图片描述这类
	// 纯转写任务等效于静默失败。设为 true 时改走原始 HTTP 请求以携带该字段
	// （go-openai 的请求结构没有这个字段）。
	thinkingDisabled bool
	httpDoer         openai.HTTPDoer
}

// NewRemoteAPIVLM creates a remote-API backed VLM instance.
func NewRemoteAPIVLM(config *Config) (*RemoteAPIVLM, error) {
	if err := validateVLMBaseURL(config.BaseURL); err != nil {
		return nil, err
	}

	providerName := provider.ProviderName(config.Provider)
	if providerName == "" {
		providerName = provider.DetectProvider(config.BaseURL)
	}

	var apiCfg openai.ClientConfig
	if providerName == provider.ProviderAzureOpenAI {
		apiCfg = openai.DefaultAzureConfig(config.APIKey, config.BaseURL)
		apiCfg.AzureModelMapperFunc = func(model string) string {
			return model
		}
		if config.Extra != nil {
			if v, ok := config.Extra["api_version"]; ok {
				if vs, ok := v.(string); ok && vs != "" {
					apiCfg.APIVersion = vs
				}
			}
		}
	} else {
		apiCfg = openai.DefaultConfig(config.APIKey)
		if config.BaseURL != "" {
			apiCfg.BaseURL = config.BaseURL
		}
	}
	httpClient := newVLMHTTPClient(vlmHTTPTimeout())

	// 注入用户自定义 HTTP header（类似 OpenAI Python SDK 的 extra_headers）
	if len(config.CustomHeaders) > 0 {
		apiCfg.HTTPClient = secutils.WrapHTTPClientWithHeaders(httpClient, config.CustomHeaders)
	} else {
		apiCfg.HTTPClient = httpClient
	}

	temp := defaultTemp
	if config.Extra != nil {
		if v, ok := config.Extra["temperature"]; ok {
			if vs, ok := v.(string); ok {
				if f, err := strconv.ParseFloat(vs, 32); err == nil {
					temp = float32(f)
				}
			}
		}
	}

	// 推理开关：extra_config.thinking = disabled 时显式关闭推理。
	thinkingDisabled := false
	if config.Extra != nil {
		if v, ok := config.Extra["thinking"]; ok {
			if vs, ok := v.(string); ok {
				thinkingDisabled = strings.EqualFold(strings.TrimSpace(vs), "disabled")
			}
		}
	}

	return &RemoteAPIVLM{
		modelName:        config.ModelName,
		modelID:          config.ModelID,
		client:           openai.NewClientWithConfig(apiCfg),
		baseURL:          strings.TrimRight(config.BaseURL, "/"),
		apiKey:           config.APIKey,
		temperature:      temp,
		thinkingDisabled: thinkingDisabled,
		httpDoer:         apiCfg.HTTPClient,
	}, nil
}

// Predict sends an image with a text prompt to the OpenAI-compatible API.
func (v *RemoteAPIVLM) Predict(ctx context.Context, imgBytesList [][]byte, prompt string) (string, error) {
	var parts []openai.ChatMessagePart

	// Add text prompt first
	parts = append(parts, openai.ChatMessagePart{
		Type: openai.ChatMessagePartTypeText,
		Text: prompt,
	})

	// Add images
	for _, imgBytes := range imgBytesList {
		if len(imgBytes) > 0 {
			mimeType := detectImageMIME(imgBytes)
			b64 := base64.StdEncoding.EncodeToString(imgBytes)
			dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, b64)
			parts = append(parts, openai.ChatMessagePart{
				Type: openai.ChatMessagePartTypeImageURL,
				ImageURL: &openai.ChatMessageImageURL{
					URL:    dataURI,
					Detail: openai.ImageURLDetailAuto,
				},
			})
		}
	}

	req := openai.ChatCompletionRequest{
		Model: v.modelName,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:         openai.ChatMessageRoleUser,
				MultiContent: parts,
			},
		},
		MaxTokens:   defaultMaxToks,
		Temperature: v.temperature,
	}

	totalImageSize := 0
	for _, img := range imgBytesList {
		totalImageSize += len(img)
	}
	logger.Infof(ctx, "[VLM] Calling OpenAI-compatible API, model=%s, baseURL=%s, numImages=%d, totalImageSize=%d",
		v.modelName, v.baseURL, len(imgBytesList), totalImageSize)

	if v.thinkingDisabled {
		return v.predictWithoutThinking(ctx, imgBytesList, prompt)
	}

	resp, err := v.client.CreateChatCompletion(ctx, req)
	if err != nil {
		return "", fmt.Errorf("OpenAI VLM request: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("OpenAI VLM returned no choices")
	}

	content := resp.Choices[0].Message.Content
	logger.Infof(ctx, "[VLM] OpenAI response received, len=%d", len(content))
	return content, nil
}

func (v *RemoteAPIVLM) GetModelName() string { return v.modelName }
func (v *RemoteAPIVLM) GetModelID() string   { return v.modelID }

// predictWithoutThinking 走原始 HTTP 请求，额外携带 thinking.type=disabled。
//
// 只在模型显式配置 extra_config.thinking=disabled 时使用；其余路径仍走
// go-openai 客户端，避免影响既有供应商的请求形状。
func (v *RemoteAPIVLM) predictWithoutThinking(
	ctx context.Context, imgBytesList [][]byte, prompt string,
) (string, error) {
	content := make([]map[string]any, 0, len(imgBytesList)+1)
	content = append(content, map[string]any{"type": "text", "text": prompt})
	for _, imgBytes := range imgBytesList {
		if len(imgBytes) == 0 {
			continue
		}
		dataURI := fmt.Sprintf("data:%s;base64,%s",
			detectImageMIME(imgBytes), base64.StdEncoding.EncodeToString(imgBytes))
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": dataURI},
		})
	}

	body, err := json.Marshal(map[string]any{
		"model":       v.modelName,
		"messages":    []map[string]any{{"role": "user", "content": content}},
		"max_tokens":  defaultMaxToks,
		"temperature": v.temperature,
		"thinking":    map[string]any{"type": "disabled"},
	})
	if err != nil {
		return "", fmt.Errorf("VLM request marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		v.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("VLM request build: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.apiKey)

	resp, err := v.httpDoer.Do(req)
	if err != nil {
		return "", fmt.Errorf("OpenAI VLM request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("OpenAI VLM read response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("OpenAI VLM request: error, status code: %d, body: %s",
			resp.StatusCode, truncateForLog(string(raw)))
	}

	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("OpenAI VLM decode response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("OpenAI VLM returned no choices")
	}
	out := parsed.Choices[0].Message.Content
	if parsed.Choices[0].FinishReason == "length" {
		// 截断的 OCR 结果比空结果更危险：它看起来像成功，但尾部内容丢失。
		logger.Warnf(ctx, "[VLM] response truncated by max_tokens (len=%d), OCR may be incomplete", len(out))
	}
	logger.Infof(ctx, "[VLM] OpenAI response received (thinking=disabled), len=%d", len(out))
	return out, nil
}

// truncateForLog 截断错误响应正文，避免把整页 base64 或大段 JSON 写进日志。
func truncateForLog(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

// detectImageMIME returns the MIME type for the given image bytes.
func detectImageMIME(data []byte) string {
	ct := http.DetectContentType(data)
	if strings.HasPrefix(ct, "image/") {
		return ct
	}
	return "image/png"
}
