package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/imageprep"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

const (
	// AliyunMultimodalEmbeddingEndpoint 阿里云 DashScope 多模态 Embedding API 端点
	AliyunMultimodalEmbeddingEndpoint = "/api/v1/services/embeddings/multimodal-embedding/multimodal-embedding"
)

// AliyunEmbedder implements text vectorization using Aliyun DashScope multimodal embedding API
type AliyunEmbedder struct {
	apiKey                    string
	baseURL                   string
	modelName                 string
	truncatePromptTokens      int
	dimensions                int
	modelID                   string
	httpClient                *http.Client
	timeout                   time.Duration
	maxRetries                int
	customHeaders             map[string]string
	supportsDimensionOverride bool
	EmbedderPooler
}

// SetCustomHeaders 设置用户自定义 HTTP 请求头（类似 OpenAI Python SDK 的 extra_headers）。
func (e *AliyunEmbedder) SetCustomHeaders(headers map[string]string) {
	e.customHeaders = headers
}

func (e *AliyunEmbedder) SetSupportsDimensionOverride(supported bool) {
	e.supportsDimensionOverride = supported
}

// AliyunEmbedParameters represents the parameters for Aliyun DashScope multimodal embedding
type AliyunEmbedParameters struct {
	Dimension int `json:"dimension,omitempty"`
}

// AliyunEmbedRequest represents an Aliyun DashScope multimodal embedding request
type AliyunEmbedRequest struct {
	Model      string                 `json:"model"`
	Input      AliyunEmbedInput       `json:"input"`
	Parameters *AliyunEmbedParameters `json:"parameters,omitempty"`
}

// AliyunEmbedInput represents the input structure for Aliyun embedding
type AliyunEmbedInput struct {
	Contents []AliyunContent `json:"contents"`
}

// AliyunContent represents a single content item in the input.
// Image 字段（data URI）供图文共同空间嵌入使用（FR-17）：DashScope
// multimodal-embedding 的每个 content 携带 text 或 image 之一。
type AliyunContent struct {
	Text  string `json:"text,omitempty"`
	Image string `json:"image,omitempty"`
}

// AliyunEmbedResponse represents an Aliyun DashScope embedding response
type AliyunEmbedResponse struct {
	Output struct {
		Embeddings []struct {
			Embedding []float32 `json:"embedding"`
			TextIndex int       `json:"text_index"`
		} `json:"embeddings"`
	} `json:"output"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
	RequestID string `json:"request_id"`
}

// AliyunErrorResponse represents an error response from Aliyun DashScope
type AliyunErrorResponse struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// NewAliyunEmbedder creates a new Aliyun DashScope embedder
func NewAliyunEmbedder(apiKey, baseURL, modelName string,
	truncatePromptTokens int, dimensions int, modelID string, pooler EmbedderPooler,
) (*AliyunEmbedder, error) {
	if baseURL == "" {
		baseURL = "https://dashscope.aliyuncs.com"
	}

	// Remove trailing slash and any existing path suffix
	baseURL = strings.TrimRight(baseURL, "/")
	// If baseURL contains /compatible-mode/v1, strip it for multimodal API
	if strings.Contains(baseURL, "/compatible-mode/v1") {
		baseURL = strings.Replace(baseURL, "/compatible-mode/v1", "", 1)
	}

	if modelName == "" {
		return nil, fmt.Errorf("model name is required")
	}

	if truncatePromptTokens == 0 {
		truncatePromptTokens = 511
	}

	timeout := 60 * time.Second

	if err := validateEmbeddingBaseURL(baseURL); err != nil {
		return nil, err
	}

	return &AliyunEmbedder{
		apiKey:               apiKey,
		baseURL:              baseURL,
		modelName:            modelName,
		httpClient:           newEmbeddingHTTPClient(timeout),
		truncatePromptTokens: truncatePromptTokens,
		EmbedderPooler:       pooler,
		dimensions:           dimensions,
		modelID:              modelID,
		timeout:              timeout,
		maxRetries:           3,
	}, nil
}

// Embed converts text to vector
func (e *AliyunEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	for range 3 {
		embeddings, err := e.BatchEmbed(ctx, []string{text})
		if err != nil {
			return nil, err
		}
		if len(embeddings) > 0 {
			return embeddings[0], nil
		}
	}
	return nil, fmt.Errorf("no embedding returned")
}

func (e *AliyunEmbedder) doRequestWithRetry(ctx context.Context, jsonData []byte) (*http.Response, error) {
	var resp *http.Response
	var err error
	url := e.baseURL + AliyunMultimodalEmbeddingEndpoint

	for i := 0; i <= e.maxRetries; i++ {
		if i > 0 {
			backoffTime := time.Duration(1<<uint(i-1)) * time.Second
			if backoffTime > 10*time.Second {
				backoffTime = 10 * time.Second
			}
			logger.GetLogger(ctx).
				Infof("AliyunEmbedder retrying request (%d/%d), waiting %v", i, e.maxRetries, backoffTime)

			select {
			case <-time.After(backoffTime):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonData))
		if err != nil {
			logger.GetLogger(ctx).Errorf("AliyunEmbedder failed to create request: %v", err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
		secutils.ApplyCustomHeaders(req, e.customHeaders)

		resp, err = e.httpClient.Do(req)
		if err == nil {
			return resp, nil
		}

		logger.GetLogger(ctx).Errorf("AliyunEmbedder request failed (attempt %d/%d): %v", i+1, e.maxRetries+1, err)
	}

	return nil, err
}

func (e *AliyunEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	// Build contents array from texts
	contents := make([]AliyunContent, 0, len(texts))
	for _, text := range texts {
		contents = append(contents, AliyunContent{Text: text})
	}

	// Create request body
	reqBody := AliyunEmbedRequest{
		Model: e.modelName,
		Input: AliyunEmbedInput{
			Contents: contents,
		},
	}
	if e.supportsDimensionsParam() {
		reqBody.Parameters = &AliyunEmbedParameters{Dimension: e.dimensions}
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		logger.GetLogger(ctx).Errorf("AliyunEmbedder BatchEmbed marshal request error: %v", err)
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := e.doRequestWithRetry(ctx, jsonData)
	if err != nil {
		logger.GetLogger(ctx).Errorf("AliyunEmbedder BatchEmbed send request error: %v", err)
		return nil, fmt.Errorf("send request: %w", err)
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}

	body, err := secutils.ReadResponseCapped(resp.Body, secutils.ModelResponseLimit())
	if err != nil {
		logger.GetLogger(ctx).Errorf("AliyunEmbedder BatchEmbed read response error: %v", err)
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Try to parse error response
		var errResp AliyunErrorResponse
		if json.Unmarshal(body, &errResp) == nil && errResp.Message != "" {
			logger.GetLogger(ctx).Errorf("AliyunEmbedder BatchEmbed API error: %s - %s", errResp.Code, errResp.Message)
			return nil, fmt.Errorf("API error: %s - %s", errResp.Code, errResp.Message)
		}
		logger.GetLogger(ctx).Errorf("AliyunEmbedder BatchEmbed API error: Http Status %s", resp.Status)
		return nil, fmt.Errorf("BatchEmbed API error: Http Status %s", resp.Status)
	}

	// Parse response
	var response AliyunEmbedResponse
	if err := json.Unmarshal(body, &response); err != nil {
		logger.GetLogger(ctx).Errorf("AliyunEmbedder BatchEmbed unmarshal response error: %v", err)
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	// Extract embedding vectors, preserving order by text_index
	embeddings := make([][]float32, len(texts))
	for _, emb := range response.Output.Embeddings {
		if emb.TextIndex >= 0 && emb.TextIndex < len(embeddings) {
			embeddings[emb.TextIndex] = emb.Embedding
		}
	}

	return embeddings, nil
}

// GetModelName returns the model name
func (e *AliyunEmbedder) GetModelName() string {
	return e.modelName
}

// EmbedImages 将一批图片编码为与文本同一向量空间的向量（FR-17）。
// DashScope multimodal-embedding 的图片输入为 data URI；请求/响应结构
// 与文本嵌入完全一致，只是每个 content 的 text 换成 image。
func (e *AliyunEmbedder) EmbedImages(ctx context.Context, images []imageprep.Image) ([][]float32, error) {
	contents := make([]AliyunContent, 0, len(images))
	for _, img := range images {
		contents = append(contents, AliyunContent{Image: img.DataURI()})
	}
	reqBody := AliyunEmbedRequest{
		Model: e.modelName,
		Input: AliyunEmbedInput{Contents: contents},
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	resp, err := e.doRequestWithRetry(ctx, jsonData)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	body, err := secutils.ReadResponseCapped(resp.Body, secutils.ModelResponseLimit())
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var errResp AliyunErrorResponse
		if json.Unmarshal(body, &errResp) == nil && errResp.Message != "" {
			return nil, fmt.Errorf("API error: %s - %s", errResp.Code, errResp.Message)
		}
		return nil, fmt.Errorf("EmbedImages API error: Http Status %s", resp.Status)
	}
	var response AliyunEmbedResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	embeddings := make([][]float32, len(images))
	for _, emb := range response.Output.Embeddings {
		if emb.TextIndex >= 0 && emb.TextIndex < len(embeddings) {
			embeddings[emb.TextIndex] = emb.Embedding
		}
	}
	return embeddings, nil
}

func (e *AliyunEmbedder) supportsDimensionsParam() bool {
	return e.supportsDimensionOverride && e.dimensions > 0
}

// GetDimensions returns the vector dimensions
func (e *AliyunEmbedder) GetDimensions() int {
	return e.dimensions
}

// GetModelID returns the model ID
func (e *AliyunEmbedder) GetModelID() string {
	return e.modelID
}
