package docparser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

// FR-14 ③：MinerU 的内层解析预算。外层 DocReader 调用超时
// （config.KnowledgeBase.DocReaderCallTimeout）必须严格大于这里，
// internal/config 在启动时校验并抬升外层，避免出现"内层还在跑、外层已放弃"。
const (
	// defaultMinerUTimeout 自建 MinerU 单次解析的内层预算默认值。
	// WEKNORA_MINERU_TIMEOUT 可覆盖。
	defaultMinerUTimeout = 1000 * time.Second
)

const (
	mineruTimeoutEnv      = "WEKNORA_MINERU_TIMEOUT"
	mineruCloudTimeoutEnv = "WEKNORA_MINERU_CLOUD_TIMEOUT"
)

// minerURequestTimeoutFromEnv 从环境变量读取一个正的 Go duration；
// 未设置、非法或非正值时回落 fallback（并告警，避免配置错误被静默放大）。
func minerURequestTimeoutFromEnv(env string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(env))
	if value == "" {
		return fallback
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		logger.Warnf(context.Background(), "Invalid %s %q; using %s", env, value, fallback)
		return fallback
	}
	return timeout
}

// minerUEffectiveBudget 把内层解析预算收敛到外层 ctx 的剩余时间之内：
// 内层预算取 min(自身配置, 外层剩余)，因此永远不会超过外层超时。
func minerUEffectiveBudget(ctx context.Context, configured time.Duration) time.Duration {
	if configured <= 0 {
		configured = defaultMinerUTimeout
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < configured {
			return remaining
		}
	}
	return configured
}

var (
	b64DataURIPattern     = regexp.MustCompile(`^data:image/(\w+);base64,(.+)$`)
	minerUFileTypePattern = regexp.MustCompile(`^[a-z0-9]+$`)
)

// MinerUReader calls a self-hosted MinerU API to read/convert documents.
// MinerU >= 4.0 is driven through the V1 API; older servers through the
// legacy /file_parse endpoint. The protocol is detected per request, so an
// in-place MinerU upgrade needs no configuration change.
type MinerUReader struct {
	endpoint string
	timeout  time.Duration
	apiKey   string // V1 only: the server's --api-key
	tier     string // V1 only: flash, basic, standard, advanced; "" = server default
	// Legacy (<= 3.x) only; MinerU 4.0 dropped these request parameters.
	backend       string // "pipeline", "vlm-*", "hybrid-*"
	vlmServerURL  string // vLLM server URL for vlm-http-client / hybrid-http-client
	formulaEnable bool
	tableEnable   bool
	language      string
	// Shared: legacy parse_method / V1 ocr_mode take the same values.
	parseMethod string
}

// NewMinerUReader creates a reader from ParserEngineOverrides.
func NewMinerUReader(overrides map[string]string) *MinerUReader {
	c := &MinerUReader{
		endpoint:      strings.TrimRight(overrides["mineru_endpoint"], "/"),
		timeout:       minerURequestTimeoutFromEnv(mineruTimeoutEnv, defaultMinerUTimeout),
		apiKey:        strings.TrimSpace(overrides["mineru_server_api_key"]),
		tier:          resolveMinerUTier(overrides["mineru_tier"]),
		backend:       stringOr(overrides["mineru_model"], "pipeline"),
		vlmServerURL:  overrides["mineru_vlm_server_url"],
		formulaEnable: parseBoolOr(overrides["mineru_enable_formula"], true),
		tableEnable:   parseBoolOr(overrides["mineru_enable_table"], true),
		parseMethod: resolveMinerUParseMethod(overrides["mineru_parse_method"],
			parseBoolOr(overrides["mineru_enable_ocr"], true)),
		language: stringOr(overrides["mineru_language"], "ch"),
	}
	return c
}

// resolveMinerUTier keeps only tiers MinerU 4.0 understands; anything else
// falls back to the server's default selection.
func resolveMinerUTier(raw string) string {
	tier := strings.ToLower(strings.TrimSpace(raw))
	if _, ok := mineruV1Tiers[tier]; ok {
		return tier
	}
	return ""
}

// resolveMinerUParseMethod 显式配置优先，否则沿用既有语义：
// 关闭 OCR 时用纯文本模式，其余情况按 OCR 解析。
func resolveMinerUParseMethod(raw string, ocrEnabled bool) string {
	switch mode := strings.ToLower(strings.TrimSpace(raw)); mode {
	case "auto", "txt", "ocr":
		return mode
	}
	if !ocrEnabled {
		return "txt"
	}
	return "ocr"
}

func (c *MinerUReader) Read(ctx context.Context, req *types.ReadRequest) (*types.ReadResult, error) {
	if c.endpoint == "" {
		return &types.ReadResult{Error: "MinerU endpoint is not configured"}, nil
	}
	if err := validateMinerUOutboundURL(c.endpoint); err != nil {
		return &types.ReadResult{Error: err.Error()}, nil
	}
	if c.vlmServerURL != "" {
		if err := validateMinerUOutboundURL(c.vlmServerURL); err != nil {
			return &types.ReadResult{Error: err.Error()}, nil
		}
	}

	content := req.FileContent
	if len(content) == 0 {
		return &types.ReadResult{Error: "no file content provided"}, nil
	}

	protocol, err := detectMinerUProtocol(ctx, utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{
		Timeout:      mineruV1APITimeout,
		MaxRedirects: 5,
	}), c.endpoint, c.apiKey)
	if err != nil {
		if protocol == minerUProtocolV1 {
			// 明确识别为 V1 但不可用（多半是缺 API Key）：直接报错，
			// 否则只会以一个含糊的 404 收场。
			return nil, fmt.Errorf("MinerU protocol detection: %w", err)
		}
		// 探测本身失败时按 legacy 继续：老版本服务没有 /v1 路由，
		// 探测报错不代表服务不可用。
		logger.Warnf(ctx, "[MinerU] protocol detection failed, falling back to legacy /file_parse: %v", err)
	}

	if protocol == minerUProtocolV1 {
		logger.Infof(ctx, "[MinerU] Parsing file=%s size=%d via %s (V1 API)", req.FileName, len(content), c.endpoint)
		return c.readV1(ctx, req)
	}

	logger.Infof(ctx, "[MinerU] Parsing file=%s size=%d via %s (legacy /file_parse)",
		req.FileName, len(content), c.endpoint)

	mdContent, imagesB64, layout, err := c.callFileParse(ctx, content, req.FileName, req.FileType)
	if err != nil {
		return nil, fmt.Errorf("MinerU file_parse: %w", err)
	}

	// MinerU already returns markdown with embedded HTML blocks (e.g. <table>, <details>).
	// Re-running the whole document through html-to-markdown corrupts valid markdown
	// by escaping headings and image syntax. Only apply narrow compatibility fixes.
	mdContent = normalizeMinerUMarkdown(mdContent)

	// Process images: decode base64, build ImageRef list, replace refs in markdown.
	// 图像预算（FR-14 ②）在这里执行，页数取自布局产物。
	imageRefs, mdContent, err := c.processImages(mdContent, imagesB64, minerULayoutPageCount(layout))
	if err != nil {
		return nil, err
	}

	mdContent, imageRefs = ensureOriginalImageRef(req, mdContent, imageRefs)

	// FR-14 ①：保留区域类型与几何信息（页码 + 归一化 bbox）供下游切分与溯源。
	sourceBlocks := minerUSourceBlocks(mdContent, layout, minerUFileTypeOf(req))

	logger.Infof(ctx, "[MinerU] Parsed successfully, markdown=%d chars, images=%d, source_blocks=%d",
		len(mdContent), len(imageRefs), len(sourceBlocks))

	return &types.ReadResult{
		MarkdownContent: mdContent,
		ImageRefs:       imageRefs,
		SourceBlocks:    sourceBlocks,
	}, nil
}

// readV1 走 MinerU 4.0 的 /v1 异步 API：上传 → 建任务 → 轮询 → 下载 zip 产物。
// 内层预算与远端产物清理都由 minerUV1Client 负责。
func (c *MinerUReader) readV1(ctx context.Context, req *types.ReadRequest) (*types.ReadResult, error) {
	client := newMinerUV1Client(c.endpoint, c.apiKey, minerUEffectiveBudget(ctx, c.timeout))
	mdContent, imageRefs, contentList, err := client.ParseWithLayout(ctx, req.FileContent,
		minerUUploadFileName(req.FileName, req.FileType),
		minerUV1ParseOptions{Tier: c.tier, OCRMode: c.parseMethod})
	if err != nil {
		return nil, fmt.Errorf("MinerU V1: %w", err)
	}

	mdContent = normalizeMinerUMarkdown(mdContent)
	mdContent, imageRefs = ensureOriginalImageRef(req, mdContent, imageRefs)

	logger.Infof(ctx, "[MinerU] Parsed successfully (V1), markdown=%d chars, images=%d",
		len(mdContent), len(imageRefs))

	return &types.ReadResult{
		MarkdownContent: mdContent,
		ImageRefs:       imageRefs,
		SourceBlocks:    minerUSourceBlocks(mdContent, contentList, minerUFileTypeOf(req)),
	}, nil
}

// mineruFileEntry 是 legacy /file_parse 响应里单个文件的结果。
type mineruFileEntry struct {
	MDContent string            `json:"md_content"`
	Images    map[string]string `json:"images"` // path -> "data:image/png;base64,..." or raw base64
	// ContentList 是布局块列表（每块的页码与 bbox），依 MinerU 版本不同
	// 可能是 JSON 数组，也可能是装着数组的 JSON 字符串。
	ContentList json.RawMessage `json:"content_list"`
	// MiddleJson 是 MiddleJson 2.0 布局产物，几何精度优于 content_list。
	MiddleJson json.RawMessage `json:"middle_json"`
}

// layout 优先返回 middle_json，回落到 content_list。
func (e mineruFileEntry) layout() json.RawMessage {
	if len(bytes.TrimSpace(e.MiddleJson)) > 0 {
		return e.MiddleJson
	}
	return e.ContentList
}

func minerUCleanFileType(fileType string) string {
	cleanType := strings.ToLower(strings.TrimSpace(fileType))
	cleanType = strings.TrimPrefix(cleanType, ".")
	if minerUFileTypePattern.MatchString(cleanType) {
		return cleanType
	}
	return ""
}

// minerUUploadFileName 构造一个安全、带扩展名的上传文件名：MinerU 用扩展名
// 选择解析器，也用文件名作为响应 results 的键。
func minerUUploadFileName(fileName, fileType string) string {
	cleanName := strings.TrimSpace(fileName)
	cleanName = strings.ReplaceAll(cleanName, `\`, "/")
	cleanName = path.Base(cleanName)
	cleanName = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 {
			return -1
		}
		return r
	}, cleanName)
	cleanName = strings.TrimSpace(cleanName)
	if cleanName != "" && cleanName != "." && cleanName != ".." && cleanName != "/" {
		if filepath.Ext(cleanName) == "" {
			if cleanType := minerUCleanFileType(fileType); cleanType != "" {
				return cleanName + "." + cleanType
			}
		}
		return cleanName
	}

	if cleanType := minerUCleanFileType(fileType); cleanType != "" {
		return "document." + cleanType
	}
	return "document"
}

// minerUResultStem mirrors MinerU's upload.stem: the basename without extension.
func minerUResultStem(uploadFileName string) string {
	stem := strings.TrimSuffix(path.Base(uploadFileName), filepath.Ext(uploadFileName))
	if stem == "" || stem == "." {
		return ""
	}
	return stem
}

func minerUResultLookupKeys(uploadFileName string) []string {
	keys := make([]string, 0, 3)
	if stem := minerUResultStem(uploadFileName); stem != "" {
		keys = append(keys, stem)
	}
	return append(keys, "document", "files")
}

// parseMinerUFileParseResponse 从 legacy 响应里取出 markdown、内联 base64 图片
// 与布局产物。results 的键随版本不同（上传文件名的 stem / document / files），
// 因此按优先级依次尝试，最后兜底遍历。
func parseMinerUFileParseResponse(respBody []byte, uploadFileName string) (string, map[string]string, json.RawMessage, error) {
	var envelope struct {
		Results map[string]mineruFileEntry `json:"results"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return "", nil, nil, fmt.Errorf("decode response: %w", err)
	}
	if len(envelope.Results) == 0 {
		return "", nil, nil, nil
	}

	pick := func(entry mineruFileEntry, key string) (string, map[string]string, json.RawMessage, error) {
		logger.Infof(context.Background(), "[MinerU] Using response path: results.%s", key)
		return entry.MDContent, entry.Images, entry.layout(), nil
	}

	for _, key := range minerUResultLookupKeys(uploadFileName) {
		if entry, ok := envelope.Results[key]; ok {
			if entry.MDContent != "" || len(entry.Images) > 0 {
				return pick(entry, key)
			}
		}
	}

	for key, entry := range envelope.Results {
		if entry.MDContent != "" || len(entry.Images) > 0 {
			return pick(entry, key)
		}
	}
	return "", nil, nil, nil
}

func (c *MinerUReader) callFileParse(
	ctx context.Context,
	content []byte,
	fileName string,
	fileType string,
) (string, map[string]string, json.RawMessage, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Form fields
	fields := map[string]string{
		"return_md":           "true",
		"return_images":       "true",
		"table_enable":        fmt.Sprintf("%v", c.tableEnable),
		"formula_enable":      fmt.Sprintf("%v", c.formulaEnable),
		"parse_method":        c.parseMethod,
		"start_page_id":       "0",
		"end_page_id":         "99999",
		"backend":             c.backend,
		"response_format_zip": "false",
		// 布局产物是 FR-14 ① 的来源：middle_json 带完整的区域类型与
		// 归一化几何，content_list 作为老版本的回落。
		"return_middle_json":  "true",
		"return_model_output": "false",
		"return_content_list": "true",
	}
	if c.language != "" {
		fields["lang_list"] = c.language
	}
	if c.vlmServerURL != "" && (strings.HasPrefix(c.backend, "vlm-http-client") || strings.HasPrefix(c.backend, "hybrid-http-client")) {
		fields["server_url"] = c.vlmServerURL
	}
	for k, v := range fields {
		_ = writer.WriteField(k, v)
	}

	uploadFileName := minerUUploadFileName(fileName, fileType)

	// File part
	part, err := writer.CreateFormFile("files", uploadFileName)
	if err != nil {
		return "", nil, nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return "", nil, nil, fmt.Errorf("write file content: %w", err)
	}
	writer.Close()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/file_parse", &body)
	if err != nil {
		return "", nil, nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())

	// 内层预算取 min(自身配置, 外层剩余)，见 minerUEffectiveBudget（FR-14 ③）。
	client := utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{
		Timeout:      minerUEffectiveBudget(ctx, c.timeout),
		MaxRedirects: 5,
	})
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", nil, nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", nil, nil, fmt.Errorf("MinerU API status %d: %s", resp.StatusCode, readErrorPreview(resp.Body))
	}

	// 响应体内联整篇解析结果与 base64 图片，按结果响应上界约束。
	respBody, err := readCapped(resp.Body, maxResultResponseBytes())
	if err != nil {
		return "", nil, nil, fmt.Errorf("read response body: %w", err)
	}

	// Dump raw response for debugging (truncate if too large)
	rawStr := string(respBody)
	if len(rawStr) > 4000 {
		logger.Infof(ctx, "[MinerU] Raw response (truncated to 4000 chars): %s ...", rawStr[:4000])
	} else {
		logger.Infof(ctx, "[MinerU] Raw response: %s", rawStr)
	}

	// Also pretty-print the top-level structure (without large base64 blobs)
	var rawMap map[string]interface{}
	if err := json.Unmarshal(respBody, &rawMap); err == nil {
		c.logMinerUResponseStructure(rawMap, "")
	}

	mdContent, imagesB64, layout, err := parseMinerUFileParseResponse(respBody, uploadFileName)
	if err != nil {
		return "", nil, nil, err
	}
	if mdContent == "" && len(imagesB64) == 0 && len(layout) == 0 {
		logger.Errorf(ctx, "[MinerU] Response has no markdown/images under results")
	}
	return mdContent, imagesB64, layout, nil
}

// decodeMinerUBase64Image 解出 MinerU 内联的一张图片：既接受
// data:image/...;base64, 形式，也接受裸 base64。
func decodeMinerUBase64Image(ipath, b64Str string) ([]byte, string, bool) {
	if m := b64DataURIPattern.FindStringSubmatch(b64Str); len(m) == 3 {
		decoded, err := base64.StdEncoding.DecodeString(m[2])
		if err != nil {
			logger.Errorf(context.Background(), "[MinerU] Failed to decode base64 image %s: %v", ipath, err)
			return nil, "", false
		}
		return decoded, m[1], true
	}
	decoded, err := base64.StdEncoding.DecodeString(b64Str)
	if err != nil {
		logger.Errorf(context.Background(), "[MinerU] Failed to decode raw base64 image %s: %v", ipath, err)
		return nil, "", false
	}
	ext := strings.TrimPrefix(filepath.Ext(ipath), ".")
	if ext == "" {
		ext = "png"
	}
	return decoded, ext, true
}

// processImages decodes base64 images from the MinerU response, enforces the
// image budget (FR-14 ②) and returns the ImageRef list. pages 是布局产物给出
// 的文档页数，作为重复图（页眉页脚 logo）判定的分母，0 表示未知。
func (c *MinerUReader) processImages(mdContent string, imagesB64 map[string]string, pages int) ([]types.ImageRef, string, error) {
	maxTotal := maxZipImagesTotalBytes()
	maxCount := maxImagesPerDoc()
	var total int64
	candidates := make([]minerUImageCandidate, 0, len(imagesB64))
	lineCounts := minerUImageLineOccurrences(mdContent)

	for ipath, b64Str := range imagesB64 {
		matchedRefs := mineruImageOriginalRefs(mdContent, ipath)
		if len(matchedRefs) == 0 {
			continue
		}
		imgBytes, ext, ok := decodeMinerUBase64Image(ipath, b64Str)
		if !ok {
			continue
		}
		if len(candidates) >= maxCount {
			return nil, "", fmt.Errorf("%w: MinerU returned more than %d images, limit is %d (%s)",
				errResultPackageTooLarge, len(candidates), maxCount, maxImagesPerDocEnv)
		}
		if total+int64(len(imgBytes)) > maxTotal {
			return nil, "", fmt.Errorf("%w: images reached %d bytes and exceed limit %d bytes (%s)",
				errResultPackageTooLarge, total+int64(len(imgBytes)), maxTotal, zipImagesTotalLimitEnv)
		}
		total += int64(len(imgBytes))
		if err := checkMinerUImageDimensions(ipath, imgBytes, "MinerU"); err != nil {
			return nil, "", err
		}

		mimeType := mime.TypeByExtension("." + ext)
		if mimeType == "" {
			mimeType = "image/png"
		}

		refs := make([]types.ImageRef, 0, len(matchedRefs))
		for _, originalRef := range matchedRefs {
			refs = append(refs, types.ImageRef{
				Filename:    ipath,
				OriginalRef: originalRef,
				MimeType:    mimeType,
				ImageData:   imgBytes,
			})
		}
		// 同一张图的多种写法（"images/a.png" 与 "./a.png"）取行数最大值，
		// 而不是相加：同一行里的两种写法只代表一页。
		occurrences := 0
		for _, ref := range matchedRefs {
			if n := lineCounts[ref]; n > occurrences {
				occurrences = n
			}
		}
		candidates = append(candidates, minerUImageCandidate{
			Refs:        refs,
			Occurrences: occurrences,
			Hash:        minerUImageHash(imgBytes),
		})
	}

	return filterMinerURepeatedImages(candidates, "MinerU", pages), mdContent, nil
}

// logMinerUResponseStructure logs the structure of the MinerU API response.
func (c *MinerUReader) logMinerUResponseStructure(obj interface{}, prefix string) {
	logResponseStructure("MinerU", obj, prefix)
}

// validateMinerUOutboundURL rejects MinerU endpoints that would reach private
// or otherwise restricted hosts when parsed or probed from the app server.
func validateMinerUOutboundURL(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	if err := utils.ValidateURLForSSRF(rawURL); err != nil {
		return fmt.Errorf("MinerU URL blocked by SSRF check: %v", err)
	}
	return nil
}

// PingMinerU checks if the self-hosted MinerU service is reachable. MinerU 4.0
// is recognised through /v1/health; anything the probe cannot decide falls back
// to the legacy /docs liveness check so an in-place upgrade never marks a
// working service unavailable.
func PingMinerU(endpoint string) (bool, string) {
	endpoint = strings.TrimRight(endpoint, "/")
	if endpoint == "" {
		return false, "未配置 MinerU 端点"
	}
	if err := validateMinerUOutboundURL(endpoint); err != nil {
		return false, err.Error()
	}
	client := utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{
		Timeout:      5 * time.Second,
		MaxRedirects: 5,
	})
	ctx := context.Background()
	if protocol, err := detectMinerUProtocol(ctx, client, endpoint, ""); err == nil && protocol == minerUProtocolV1 {
		return true, ""
	}

	resp, err := client.Get(endpoint + "/docs")
	if err != nil {
		return false, fmt.Sprintf("MinerU 服务不可达: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return false, fmt.Sprintf("MinerU 服务返回状态 %d", resp.StatusCode)
	}
	return true, ""
}

// escapedImageSyntaxPattern matches markdown image references whose `[` was
// over-escaped to `\[` by html-to-markdown. The URL group mirrors the
// downstream image-extraction regex so escapes are only stripped for actual
// image references.
var escapedImageSyntaxPattern = regexp.MustCompile(`!\\\[(.*?)\\?\]\(([^()\n]*(?:\([^)]*\)[^()\n]*)*)\)`)

// escapedHeadingPattern restores markdown headings that were over-escaped to
// \# Heading. We only touch line-leading heading markers to avoid rewriting
// body text that intentionally contains escaped # characters.
var escapedHeadingPattern = regexp.MustCompile(`(?m)^\\(#{1,6})(\s+)`)

// unescapeMarkdownImageSyntax restores `![alt](url)` from html-to-markdown's
// over-escaped `!\[alt\](url)` form. Without this, the downstream image regex
// in ImageResolver fails to match and images are never persisted.
func unescapeMarkdownImageSyntax(content string) string {
	return escapedImageSyntaxPattern.ReplaceAllString(content, "![$1]($2)")
}

func normalizeEscapedMarkdownHeadings(content string) string {
	return escapedHeadingPattern.ReplaceAllString(content, `$1$2`)
}

func normalizeMinerUMarkdown(content string) string {
	content = unescapeMarkdownImageSyntax(content)
	content = normalizeEscapedMarkdownHeadings(content)
	return content
}

func mineruImageOriginalRefs(mdContent, imagePath string) []string {
	normalizedTarget := normalizeMinerUImagePath(imagePath)
	if normalizedTarget == "" {
		return nil
	}

	referenced := extractImageRefsFromContent(mdContent)
	seen := make(map[string]struct{}, len(referenced))
	var matched []string
	for _, refPath := range referenced {
		if normalizeMinerUImagePath(refPath) != normalizedTarget {
			continue
		}
		if _, ok := seen[refPath]; ok {
			continue
		}
		matched = append(matched, refPath)
		seen[refPath] = struct{}{}
	}

	return matched
}

// imgMarkdownPatternAllowSpaces matches markdown image syntax while allowing
// spaces in the URL group, so that paths like "images/第 1 页.jpg" produced by
// MinerU on Chinese documents are still detected as image references.
var imgMarkdownPatternAllowSpaces = regexp.MustCompile(
	`!\[(.*?)\]\(([^()\n]*(?:\([^)]*\)[^()\n]*)*)\)`,
)

func extractImageRefsFromContent(content string) []string {
	var refs []string

	for _, match := range imgMarkdownPatternAllowSpaces.FindAllStringSubmatch(content, -1) {
		if len(match) >= 3 {
			refs = append(refs, strings.TrimSpace(match[2]))
		}
	}
	for _, match := range imgHTMLRelativeSrc.FindAllStringSubmatch(content, -1) {
		if len(match) >= 3 {
			refs = append(refs, match[2])
		}
	}

	return refs
}

func normalizeMinerUImagePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if decoded, err := url.PathUnescape(p); err == nil && decoded != "" {
		p = decoded
	}
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimPrefix(p, "images/")
	return p
}
