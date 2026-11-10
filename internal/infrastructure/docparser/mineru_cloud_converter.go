package docparser

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
)

const (
	defaultPollInterval = 3 * time.Second
	// defaultCloudTimeout bounds how long one batch is polled; it is the inner
	// budget of FR-14 ③ and must stay strictly below the outer DocReader call
	// timeout (internal/config enforces that at startup).
	// WEKNORA_MINERU_CLOUD_TIMEOUT overrides it for documents that take longer.
	defaultCloudTimeout = 30 * time.Minute
	defaultBaseURL      = "https://mineru.net/api/v4"
)

// MinerUCloudReader calls the MinerU Cloud API (mineru.net) to read/convert documents.
// Flow: POST /file-urls/batch → PUT file → poll GET /extract-results/batch/{batch_id}.
type MinerUCloudReader struct {
	apiKey        string
	baseURL       string
	timeout       time.Duration
	pollInterval  time.Duration
	model         string
	formulaEnable bool
	tableEnable   bool
	ocrEnable     bool
	language      string
}

// NewMinerUCloudReader creates a reader from ParserEngineOverrides.
func NewMinerUCloudReader(overrides map[string]string) *MinerUCloudReader {
	return &MinerUCloudReader{
		apiKey:        strings.TrimSpace(overrides["mineru_api_key"]),
		baseURL:       defaultBaseURL,
		timeout:       minerURequestTimeoutFromEnv(mineruCloudTimeoutEnv, defaultCloudTimeout),
		pollInterval:  defaultPollInterval,
		model:         stringOr(overrides["mineru_cloud_model"], "pipeline"),
		formulaEnable: parseBoolOr(overrides["mineru_cloud_enable_formula"], true),
		tableEnable:   parseBoolOr(overrides["mineru_cloud_enable_table"], true),
		ocrEnable:     parseBoolOr(overrides["mineru_cloud_enable_ocr"], true),
		language:      stringOr(overrides["mineru_cloud_language"], "ch"),
	}
}

func (c *MinerUCloudReader) Read(ctx context.Context, req *types.ReadRequest) (*types.ReadResult, error) {
	if c.apiKey == "" {
		return &types.ReadResult{Error: "MinerU Cloud API key is not configured"}, nil
	}

	content := req.FileContent
	if len(content) == 0 {
		return &types.ReadResult{Error: "no file content provided"}, nil
	}

	logger.Infof(ctx, "[MinerUCloud] Parsing file=%s size=%d via %s", req.FileName, len(content), c.baseURL)

	ext := filepath.Ext(req.FileName)
	if ext == "" && req.FileType != "" {
		ext = "." + req.FileType
	}
	if ext == "" {
		ext = ".pdf"
	}
	fileName := strings.TrimSuffix(req.FileName, ext) + ext
	if fileName == ext {
		fileName = "document" + ext
	}

	copyReq := *req
	copyReq.FileName = fileName
	// 大 PDF 走分片解析（FR-14）：云端对单任务的页数与体积都有硬上限。
	if strings.EqualFold(ext, ".pdf") {
		return c.readSplitPDF(ctx, &copyReq)
	}
	result, err := c.readCloudPart(ctx, &copyReq)
	if err != nil {
		return nil, err
	}
	result.MarkdownContent, result.ImageRefs = ensureOriginalImageRef(req, result.MarkdownContent, result.ImageRefs)
	return result, nil
}

// --- batch upload API ---

type batchApplyResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		BatchID  string   `json:"batch_id"`
		FileURLs []string `json:"file_urls"`
	} `json:"data"`
}

func (c *MinerUCloudReader) applyUploadURLs(ctx context.Context, fileName, ext string) (string, string, error) {
	modelVersion := c.model
	if strings.ToLower(ext) == ".html" {
		modelVersion = "MinerU-HTML"
	}

	payload := map[string]interface{}{
		"files":          []map[string]string{{"name": fileName, "data_id": uuid.New().String()}},
		"model_version":  modelVersion,
		"is_ocr":         c.ocrEnable,
		"enable_formula": c.formulaEnable,
		"enable_table":   c.tableEnable,
		"language":       c.language,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("marshal payload: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/file-urls/batch", bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	client := utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{Timeout: 30 * time.Second, MaxRedirects: 5})
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", "", fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("API status %d: %s", resp.StatusCode, readErrorPreview(resp.Body))
	}

	respBody, err := readCapped(resp.Body, maxJSONResponseBytes())
	if err != nil {
		return "", "", fmt.Errorf("read response: %w", err)
	}
	var result batchApplyResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", "", fmt.Errorf("decode response: %w", err)
	}
	if result.Code != 0 {
		return "", "", fmt.Errorf("API error: %s", result.Msg)
	}
	if len(result.Data.FileURLs) == 0 {
		return "", "", fmt.Errorf("API returned no file_urls")
	}

	logger.Infof(ctx, "[MinerUCloud] batch apply ok: batch_id=%s, urls=%d", result.Data.BatchID, len(result.Data.FileURLs))
	return result.Data.BatchID, result.Data.FileURLs[0], nil
}

func (c *MinerUCloudReader) uploadFile(ctx context.Context, uploadURL string, content []byte) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("create PUT request: %w", err)
	}

	client := utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{Timeout: 10 * time.Minute, MaxRedirects: 5})
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("PUT upload: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("PUT upload status %d", resp.StatusCode)
	}
	logger.Infof(ctx, "[MinerUCloud] file uploaded, status=%d", resp.StatusCode)
	return nil
}

// --- polling ---

type batchPollResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		ExtractResult json.RawMessage `json:"extract_result"` // can be object or array
	} `json:"data"`
}

type extractResultItem struct {
	State    string `json:"state"`
	FileName string `json:"file_name"`
	Markdown string `json:"markdown"`
	Content  string `json:"content"`
	Text     string `json:"text"`
	ErrMsg   string `json:"err_msg"`
	Progress struct {
		ExtractedPages int `json:"extracted_pages"`
		TotalPages     int `json:"total_pages"`
	} `json:"extract_progress"`
	FullZipURL string `json:"full_zip_url"`
}

// pollBatchResult 轮询一个批次直到完成，返回 markdown、图片引用与布局产物
// （content_list / middle_json，供 FR-14 ① 的区域类型与几何信息使用）。
//
// 批次时长预算（FR-14 ③）：deadline 取 c.timeout 与外层 ctx 截止时间中更早
// 的那个，因此内层预算永远不会超过外层剩余时间，不会出现"内层还在跑、
// 外层已放弃"的资源浪费。
func (c *MinerUCloudReader) pollBatchResult(
	ctx context.Context, batchID string,
) (string, []types.ImageRef, []byte, error) {
	deadline := time.Now().Add(c.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	budget := time.Until(deadline)
	pollCount := 0
	headers := map[string]string{
		"Authorization": "Bearer " + c.apiKey,
	}

	for time.Now().Before(deadline) {
		// Bail out promptly on caller cancellation instead of spinning:
		// fetchBatchStatus fails immediately and sleepCtx returns at once on a
		// cancelled ctx, so without this guard the loop busy-hammers the cloud
		// API and floods logs until the deadline.
		if err := ctx.Err(); err != nil {
			return "", nil, nil, err
		}
		pollCount++

		items, err := c.fetchBatchStatus(ctx, batchID, headers)
		if err != nil {
			logger.Errorf(ctx, "[MinerUCloud] poll #%d failed: %v", pollCount, err)
			sleepCtx(ctx, c.pollInterval)
			continue
		}

		if len(items) == 0 {
			if pollCount <= 3 || pollCount%10 == 0 {
				logger.Infof(ctx, "[MinerUCloud] poll #%d: extract_result empty, retrying", pollCount)
			}
			sleepCtx(ctx, c.pollInterval)
			continue
		}

		item := items[0]
		state := strings.ToLower(item.State)

		if pollCount == 1 || pollCount%10 == 0 || state == "done" || state == "failed" {
			logger.Infof(ctx, "[MinerUCloud] poll #%d: file=%s state=%s pages=%d/%d",
				pollCount, item.FileName, state, item.Progress.ExtractedPages, item.Progress.TotalPages)
		}

		if state == "failed" {
			return "", nil, nil, fmt.Errorf("MinerU Cloud task failed: %s", item.ErrMsg)
		}

		if state == "done" {
			return c.extractDoneResult(ctx, &item)
		}

		sleepCtx(ctx, c.pollInterval)
	}

	// 预算可能只有几百毫秒（外层 ctx 快到期），按秒取整会打印成 "0s"，
	// 诊断时看不出到底给了多少时间。
	return "", nil, nil, fmt.Errorf("MinerU Cloud task timed out after %s (%d polls)",
		budget.Truncate(time.Millisecond), pollCount)
}

func (c *MinerUCloudReader) fetchBatchStatus(ctx context.Context, batchID string, headers map[string]string) ([]extractResultItem, error) {
	url := fmt.Sprintf("%s/extract-results/batch/%s", c.baseURL, batchID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	client := utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{Timeout: 30 * time.Second, MaxRedirects: 5})
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 轮询响应里可能内联整篇解析结果（markdown/content 字段），
	// 因此按结果响应上界而不是控制面上界来约束。
	respBody, err := readCapped(resp.Body, maxResultResponseBytes())
	if err != nil {
		return nil, fmt.Errorf("read poll response body: %w", err)
	}

	var pollResp batchPollResponse
	if err := json.Unmarshal(respBody, &pollResp); err != nil {
		return nil, fmt.Errorf("decode poll response: %w", err)
	}
	if pollResp.Code != 0 {
		return nil, fmt.Errorf("poll error code=%d msg=%s", pollResp.Code, pollResp.Msg)
	}

	if len(pollResp.Data.ExtractResult) == 0 {
		return nil, nil
	}

	// Dump the raw extract_result JSON for debugging
	rawExtract := string(pollResp.Data.ExtractResult)
	if len(rawExtract) > 4000 {
		logger.Infof(ctx, "[MinerUCloud] Raw extract_result (truncated to 4000 chars): %s ...", rawExtract[:4000])
	} else {
		logger.Infof(ctx, "[MinerUCloud] Raw extract_result: %s", rawExtract)
	}

	// Pretty-print the structure to reveal all available fields
	var rawObj interface{}
	if err := json.Unmarshal(pollResp.Data.ExtractResult, &rawObj); err == nil {
		logResponseStructure("MinerUCloud", rawObj, "extract_result")
	}

	// The extract_result can be either a single object or an array
	var items []extractResultItem
	if pollResp.Data.ExtractResult[0] == '[' {
		if err := json.Unmarshal(pollResp.Data.ExtractResult, &items); err != nil {
			return nil, fmt.Errorf("decode extract_result array: %w", err)
		}
	} else {
		var single extractResultItem
		if err := json.Unmarshal(pollResp.Data.ExtractResult, &single); err != nil {
			return nil, fmt.Errorf("decode extract_result object: %w", err)
		}
		items = []extractResultItem{single}
	}

	return items, nil
}

// extractDoneResult extracts markdown, images and the layout list from a
// completed batch item. Prefers inline markdown/content fields; falls back to
// downloading full_zip_url.
func (c *MinerUCloudReader) extractDoneResult(
	ctx context.Context, item *extractResultItem,
) (string, []types.ImageRef, []byte, error) {
	text := firstNonEmpty(item.Markdown, item.Content, item.Text)
	if text != "" {
		logger.Infof(ctx, "[MinerUCloud] parsed (inline), length=%d", len(text))
		return text, nil, nil, nil
	}

	if item.FullZipURL == "" {
		return "", nil, nil, fmt.Errorf("MinerU Cloud state=done but no markdown/content or full_zip_url")
	}

	md, imageRefs, contentList, err := downloadAndExtractZip(ctx, item.FullZipURL)
	if err != nil {
		return "", nil, nil, fmt.Errorf("extract zip: %w", err)
	}

	logger.Infof(ctx, "[MinerUCloud] parsed (zip), markdown=%d chars, images=%d", len(md), len(imageRefs))
	return md, imageRefs, contentList, nil
}

// IsMinerUResultTooLarge reports whether err is a deterministic budget/size
// violation rather than a transient network failure. 上层据此决定是否重试：
// 同一份文档再解析一次只会得到同样的超限结果，重试纯属浪费配额（FR-14 ③）。
func IsMinerUResultTooLarge(err error) bool {
	return errors.Is(err, errResultPackageTooLarge) || errors.Is(err, utils.ErrResponseTooLarge)
}

// --- ZIP handling ---

// downloadAndExtractZip 下载并解包云端结果，同时返回包内的布局产物
// （content_list / middle_json，可能为 nil）。ctx 贯穿下载与解包全过程。
func downloadAndExtractZip(ctx context.Context, zipURL string) (string, []types.ImageRef, []byte, error) {
	// 新的远程解析端点同样受 SSRF 防护约束（FR-14 ⑤）：预签名下载地址由
	// 云端返回，属于服务端可控输入。
	if err := utils.ValidateURLForSSRF(zipURL); err != nil {
		return "", nil, nil, fmt.Errorf("zip URL blocked by SSRF check: %v", err)
	}
	client := utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{Timeout: 10 * time.Minute, MaxRedirects: 5})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, zipURL, nil)
	if err != nil {
		return "", nil, nil, err
	}
	resp, err := client.Do(request)
	if err != nil {
		return "", nil, nil, fmt.Errorf("download zip: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, nil, fmt.Errorf("download zip status %d", resp.StatusCode)
	}

	// 下载量与解压膨胀量都要有上界（FR-07）：整包先受 maxResultPackageBytes 约束，
	// 打开后再做一次结构预检（条目数 / 解压后总量 / 压缩比 / 嵌套），
	// 最后每个条目仍由 readZipEntryCapped 的 LimitReader 兜底。
	zipData, err := readResultPackage(resp.Body, maxResultPackageBytes())
	if err != nil {
		return "", nil, nil, fmt.Errorf("read zip body: %w", err)
	}
	contentList := minerUContentListFromZip(zipData)
	md, imageRefs, err := extractMarkdownZipWithLayout(zipData, "MinerUCloud", maxZipImagesTotalBytes(), contentList)
	if err != nil {
		return "", nil, nil, err
	}
	return md, imageRefs, contentList, nil
}

// extractMarkdownZip reads the shallowest .md file of a MinerU result package
// and the images it references, via markdown or inline HTML <img> syntax.
func extractMarkdownZip(zipData []byte, logLabel string) (string, []types.ImageRef, error) {
	return extractMarkdownZipBounded(zipData, logLabel, maxZipImagesTotalBytes())
}

// extractMarkdownZipBounded is extractMarkdownZip with the accumulated-image
// budget as a parameter, so a test can exercise the limit without materialising
// the production figure.
func extractMarkdownZipBounded(zipData []byte, logLabel string, maxImages int64) (string, []types.ImageRef, error) {
	return extractMarkdownZipWithLayout(zipData, logLabel, maxImages, minerUContentListFromZip(zipData))
}

// extractMarkdownZipWithLayout 在图像预算（FR-14 ②）内解包结果：
//   - 单文档图像总字节：maxImages（zip_limits.go 的 WEKNORA_MINERU_MAX_IMAGE_BYTES）
//   - 单文档图像数：WEKNORA_MAX_IMAGES_PER_DOC
//   - 单图像素 / 长边：WEKNORA_MAX_IMAGE_PIXELS / WEKNORA_MAX_IMAGE_LONG_EDGE_PX
//   - 重复图像（页眉页脚 logo）：按图像字节 sha256 统计出现次数并去重
//
// 前三项超限一律整体拒绝（不产出"看似完整、实际缺图"的结果），
// 重复图属于去重而非超限，丢弃并告警。layout 提供页数作为重复图判定的分母。
func extractMarkdownZipWithLayout(
	zipData []byte, logLabel string, maxImages int64, layout []byte,
) (string, []types.ImageRef, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return "", nil, fmt.Errorf("open zip: %w", err)
	}
	if err := checkZipArchiveLimits(zr); err != nil {
		return "", nil, err
	}

	// Find .md files
	var mdFiles []string
	entries := make(map[string]*zip.File)
	for _, f := range zr.File {
		entries[f.Name] = f
		if strings.HasSuffix(f.Name, ".md") {
			mdFiles = append(mdFiles, f.Name)
		}
	}
	if len(mdFiles) == 0 {
		return "", nil, fmt.Errorf("no .md file found in zip")
	}
	sort.Slice(mdFiles, func(i, j int) bool {
		di, dj := strings.Count(mdFiles[i], "/"), strings.Count(mdFiles[j], "/")
		if di != dj {
			return di < dj
		}
		return mdFiles[i] < mdFiles[j]
	})

	mdText, err := readZipEntry(entries[mdFiles[0]])
	if err != nil {
		return "", nil, fmt.Errorf("read md file: %w", err)
	}

	mdDir := filepath.Dir(mdFiles[0])

	// Extract referenced images.
	// 下面每张图片都会一直驻留内存直到调用方交出这个切片，
	// 所以除了单条目上界之外还需要一个总量预算：一个装着上千张小图的包
	// 永远不会触发单条目上界。
	allRefs := extractImageRefsFromContent(mdText)
	// 同一引用出现的行数 ≈ 它出现的页数（MinerU 每页产生一次引用），
	// 这是页眉页脚 logo 判定的分子。
	occurrences := minerUImageLineOccurrences(mdText)

	maxCount := maxImagesPerDoc()
	var imageBytes int64
	seen := map[string]bool{}
	candidates := make([]minerUImageCandidate, 0, len(allRefs))
	for _, imgPath := range allRefs {
		if isExternalImageRef(imgPath) {
			continue
		}
		if seen[imgPath] {
			continue
		}
		seen[imgPath] = true

		if len(candidates) >= maxCount {
			return "", nil, fmt.Errorf(
				"%w: %q references more than %d images, limit is %d (%s)",
				errResultPackageTooLarge, logLabel, len(candidates), maxCount, maxImagesPerDocEnv)
		}

		resolved := resolveInZip(imgPath, mdDir, entries)
		if resolved == nil {
			logger.Errorf(context.Background(), "[%s] image not found in zip: %s", logLabel, imgPath)
			continue
		}

		imgData, err := readZipEntryBytes(resolved)
		if err != nil {
			logger.Errorf(context.Background(), "[%s] failed to read zip image %s: %v", logLabel, imgPath, err)
			continue
		}
		// 图片总量超出预算时整体拒绝：只跳过超限的那几张会产出
		// "看起来完整、实际缺图"的结果（PRD FR-07 异常条款）。
		if imageBytes+int64(len(imgData)) > maxImages {
			return "", nil, fmt.Errorf("%w: images reached %d bytes and exceed limit %d bytes (%s)",
				errResultPackageTooLarge, imageBytes+int64(len(imgData)), maxImages, zipImagesTotalLimitEnv)
		}
		imageBytes += int64(len(imgData))
		if err := checkMinerUImageDimensions(resolved.Name, imgData, logLabel); err != nil {
			return "", nil, err
		}

		ext := strings.ToLower(filepath.Ext(resolved.Name))
		if ext == "" {
			ext = ".png"
		}
		mimeType := mime.TypeByExtension(ext)
		if mimeType == "" {
			mimeType = "image/png"
		}

		candidates = append(candidates, minerUImageCandidate{
			Refs: []types.ImageRef{{
				Filename:    filepath.Base(resolved.Name),
				OriginalRef: imgPath,
				MimeType:    mimeType,
				ImageData:   imgData,
			}},
			Occurrences: occurrences[imgPath],
			Hash:        minerUImageHash(imgData),
		})
	}

	return mdText, filterMinerURepeatedImages(candidates, logLabel, minerULayoutPageCount(layout)), nil
}

// --- 图像预算（FR-14 ②）---
//
// 字节类上界（整包 / 单条目 / 图片总量）由 zip_limits.go 提供，这里只补
// 它没有覆盖的三项：单文档图像数、单图像素、单图长边，以及重复图去重。

const (
	// defaultMaxImagesPerDoc 单文档图像数上界默认值。正常文档的插图数在
	// 数十到数百量级；上千张图要么是扫描版逐页出图，要么是解析异常。
	defaultMaxImagesPerDoc = 500

	// defaultMaxImagePixels 单图像素数上界默认值：16,000,000（约 4000x4000）。
	defaultMaxImagePixels int64 = 16_000_000

	// defaultMaxImageLongEdge 单图长边像素上界默认值：2000px。
	// 下游 VLM 与前端展示都不需要更大的图，超大图只会白占内存与带宽。
	defaultMaxImageLongEdge = 2000

	// minerURepeatImageFraction 重复图判定比例：出现在半数及以上页面的同字节
	// 图像视为页眉页脚 logo。与 docreader/parser/pdf_parser.py 的同名常量一致。
	minerURepeatImageFraction = 0.5

	// minerURepeatImageMinPages 重复图判定的最小出现页数，避免短文档误杀正文插图。
	minerURepeatImageMinPages = 2
)

const (
	maxImagesPerDocEnv  = "WEKNORA_MAX_IMAGES_PER_DOC"
	maxImagePixelsEnv   = "WEKNORA_MAX_IMAGE_PIXELS"
	maxImageLongEdgeEnv = "WEKNORA_MAX_IMAGE_LONG_EDGE_PX"
)

func maxImagesPerDoc() int {
	return int(utils.EnvByteLimit(maxImagesPerDocEnv, defaultMaxImagesPerDoc))
}

func maxImagePixels() int64 {
	return utils.EnvByteLimit(maxImagePixelsEnv, defaultMaxImagePixels)
}

func maxImageLongEdge() int {
	return int(utils.EnvByteLimit(maxImageLongEdgeEnv, defaultMaxImageLongEdge))
}

// isExternalImageRef 报告引用是否指向包外（绝对 URL 或内联 data URI），
// 这类引用不在结果包里，也不参与图像预算。
func isExternalImageRef(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "data:")
}

func minerUImageHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// minerUImageLineOccurrences 统计每个图片引用出现在多少个不同的 markdown 行上。
// MinerU 每页产出一次引用，所以"行数"≈"出现页数"，是重复图判定的分子。
// 同一行里的多种写法（markdown 语法 + 内联 HTML 的 src）只算一次：否则一张
// 正文插图会因为被写了两种形式而被误判成"出现在两页"，进而被当成 logo 丢掉。
func minerUImageLineOccurrences(markdown string) map[string]int {
	counts := make(map[string]int)
	for _, line := range strings.Split(markdown, "\n") {
		seenLine := make(map[string]bool)
		for _, ref := range extractImageRefsFromContent(line) {
			if seenLine[ref] {
				continue
			}
			seenLine[ref] = true
			counts[ref]++
		}
	}
	return counts
}

// checkMinerUImageDimensions 拒绝超出像素 / 长边上界的图像。
// 读不出尺寸的格式（例如 MinerU 偶尔产出的 svg）放行：不能因为量不出
// 大小就把正文插图丢掉。
func checkMinerUImageDimensions(name string, data []byte, logLabel string) error {
	cfg, ok := minerUImageConfig(data)
	if !ok {
		return nil
	}
	pixels := int64(cfg.Width) * int64(cfg.Height)
	if limit := maxImagePixels(); pixels > limit {
		return fmt.Errorf("%w: image %q in %q has %d pixels (%dx%d), limit is %d pixels (%s)",
			errResultPackageTooLarge, name, logLabel, pixels, cfg.Width, cfg.Height, limit, maxImagePixelsEnv)
	}
	longEdge := cfg.Width
	if cfg.Height > longEdge {
		longEdge = cfg.Height
	}
	if limit := maxImageLongEdge(); longEdge > limit {
		return fmt.Errorf("%w: image %q in %q has a %dpx long edge, limit is %dpx (%s)",
			errResultPackageTooLarge, name, logLabel, longEdge, limit, maxImageLongEdgeEnv)
	}
	return nil
}

// minerUImageConfig 只读图像头部取尺寸，不解码像素。逐个尝试 MinerU 会
// 产出的三种位图格式，都读不出时返回 false。
func minerUImageConfig(data []byte) (image.Config, bool) {
	for _, decode := range []func(io.Reader) (image.Config, error){
		png.DecodeConfig, jpeg.DecodeConfig, gif.DecodeConfig,
	} {
		if cfg, err := decode(bytes.NewReader(data)); err == nil && cfg.Width > 0 && cfg.Height > 0 {
			return cfg, true
		}
	}
	return image.Config{}, false
}

// minerUImageCandidate 是一组共享同一份图像字节的引用：Refs 是它对应的
// 图片引用（同一张图在 markdown 里可能有多种写法），Occurrences 是它出现的
// 行数（近似"出现页数"，见 minerUImageLineOccurrences），Hash 是图像字节的 sha256。
type minerUImageCandidate struct {
	Refs        []types.ImageRef
	Occurrences int
	Hash        string
}

// filterMinerURepeatedImages 复刻 docreader/parser/pdf_parser.py 的
// _select_embedded_images 规则：同一份图像字节出现在
// max(2, 页数*0.5) 个及以上页面时判定为页眉页脚 logo，整组丢弃。
// pages <= 0 表示页数未知，此时跳过该规则——按猜测丢图比多留几张更糟。
func filterMinerURepeatedImages(candidates []minerUImageCandidate, logLabel string, pages int) []types.ImageRef {
	out := make([]types.ImageRef, 0, len(candidates))
	if pages <= 0 {
		for _, c := range candidates {
			out = append(out, c.Refs...)
		}
		return out
	}
	threshold := int(math.Ceil(float64(pages) * minerURepeatImageFraction))
	if threshold < minerURepeatImageMinPages {
		threshold = minerURepeatImageMinPages
	}
	// 每个候选对应一份独立的图像字节，因此按 hash 求和即为出现的页面数。
	pagesSeen := make(map[string]int, len(candidates))
	for _, c := range candidates {
		pagesSeen[c.Hash] += c.Occurrences
	}
	dropped := 0
	for _, c := range candidates {
		if pagesSeen[c.Hash] >= threshold {
			dropped++
			continue
		}
		out = append(out, c.Refs...)
	}
	if dropped > 0 {
		logger.Warnf(context.Background(),
			"[%s] dropped %d repeated images (identical bytes on >= %d of %d pages) as header/footer logos",
			logLabel, dropped, threshold, pages)
	}
	return out
}

func resolveInZip(imgPath, mdDir string, entries map[string]*zip.File) *zip.File {
	candidates := []string{strings.ReplaceAll(imgPath, "\\", "/")}
	if decoded, err := url.PathUnescape(candidates[0]); err == nil && decoded != candidates[0] {
		candidates = append(candidates, decoded)
	}
	for _, c := range candidates {
		c = strings.TrimPrefix(c, "./")
		if f, ok := entries[c]; ok {
			return f
		}
		if mdDir != "" && mdDir != "." {
			if f, ok := entries[mdDir+"/"+c]; ok {
				return f
			}
		}
	}
	return nil
}

// readZipEntry 读取文本条目（结果包里的 markdown），受单条目上界约束。
func readZipEntry(f *zip.File) (string, error) {
	data, err := readZipEntryCapped(f, maxZipEntryBytes())
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// readZipEntryBytes 读取二进制条目（抽取出的图片），受单条目上界约束。
func readZipEntryBytes(f *zip.File) ([]byte, error) {
	return readZipEntryCapped(f, maxZipEntryBytes())
}

// PingMinerUCloud checks if the MinerU Cloud API is reachable with the given API key.
func PingMinerUCloud(apiKey string) (bool, string) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return false, "未配置 MinerU Cloud API Key"
	}

	targetURL := defaultBaseURL + "/file-urls/batch"
	payload := []byte(`{"files":[],"model_version":"pipeline"}`)
	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		return false, fmt.Sprintf("构建请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{
		Timeout:      10 * time.Second,
		MaxRedirects: 5,
	})
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Sprintf("MinerU Cloud 不可达: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return false, "MinerU Cloud API Key 无效"
	}
	return true, ""
}
