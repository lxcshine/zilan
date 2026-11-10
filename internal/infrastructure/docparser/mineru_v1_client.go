package docparser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

// MinerU 4.0 replaced the synchronous /file_parse endpoint with the V1 API:
// upload → parse job → poll → download artifacts. See
// https://opendatalab.github.io/MinerU/usage/http_api/.
//
// FR-14 ③④：内层解析预算由 jobTimeout 承载（ParseWithLayout 内部再起一层
// context.WithTimeout，因此天然不会超过外层 ctx 的剩余时间）；失败或取消时
// 用独立的 background ctx + cancelBudget 取消 job 并删除已上传的文件，
// 不在远端留孤儿产物。

const (
	mineruV1PollInitial  = 2 * time.Second
	mineruV1PollMax      = 30 * time.Second
	mineruV1APITimeout   = 30 * time.Second
	mineruV1CancelBudget = 10 * time.Second
	// mineruV1MaxJSONBytes 控制面 JSON 响应体上界（任务状态、上传凭据）。
	mineruV1MaxJSONBytes int64 = 16 << 20
	// mineruV1ErrorPreviewBytes 失败响应写入错误串的预览上界。
	mineruV1ErrorPreviewBytes = 300
)

// mineruV1Tiers are the tiers accepted by POST /v1/parse/jobs. An empty tier
// means the server's default selection policy (standard → basic for
// PDF/image).
var mineruV1Tiers = map[string]struct{}{
	"flash":    {},
	"basic":    {},
	"standard": {},
	"advanced": {},
}

// mineruV1OCRModes are the ocr_mode values accepted by POST /v1/parse/jobs.
var mineruV1OCRModes = map[string]struct{}{
	"auto": {},
	"txt":  {},
	"ocr":  {},
}

type minerUProtocol int

const (
	minerUProtocolLegacy minerUProtocol = iota // MinerU <= 3.x: POST /file_parse
	minerUProtocolV1                           // MinerU >= 4.0: /v1/*
)

// minerUV1Client talks to any MinerU V1 deployment (self-hosted api-server or
// the official mineru.net/api).
type minerUV1Client struct {
	baseURL      string // without the /v1 suffix
	apiKey       string
	apiClient    *http.Client // small JSON calls
	bulkClient   *http.Client // byte upload and artifact download
	pollInitial  time.Duration
	pollMax      time.Duration
	jobTimeout   time.Duration
	maxZipBytes  int64
	logLabel     string
	cancelBudget time.Duration
}

type minerUV1ParseOptions struct {
	Tier    string // "" → server default
	OCRMode string // auto, txt, ocr
}

// newMinerUV1Client builds a client whose inner parse budget is jobTimeout.
// 出站一律走 SSRF 安全客户端（FR-14 ⑤：新接入的远程解析端点同样受约束）。
func newMinerUV1Client(baseURL, apiKey string, jobTimeout time.Duration) *minerUV1Client {
	if jobTimeout <= 0 {
		jobTimeout = defaultMinerUTimeout
	}
	return &minerUV1Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  strings.TrimSpace(apiKey),
		apiClient: utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{
			Timeout:      mineruV1APITimeout,
			MaxRedirects: 5,
		}),
		bulkClient: utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{
			Timeout:      jobTimeout,
			MaxRedirects: 5,
		}),
		pollInitial:  mineruV1PollInitial,
		pollMax:      mineruV1PollMax,
		jobTimeout:   jobTimeout,
		maxZipBytes:  maxResultPackageBytes(),
		logLabel:     "MinerU",
		cancelBudget: mineruV1CancelBudget,
	}
}

// --- wire types ---

type minerUV1ErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

type minerUV1File struct {
	ID string `json:"id"`
}

type minerUV1Upload struct {
	ID            string            `json:"id"`
	Status        string            `json:"status"`
	UploadURL     string            `json:"upload_url"`
	UploadMethod  string            `json:"upload_method"`
	UploadHeaders map[string]string `json:"upload_headers"`
	File          *minerUV1File     `json:"file"`
}

type minerUV1FileRef struct {
	FileID string `json:"file_id"`
	Bytes  int64  `json:"bytes"`
}

type minerUV1JobFile struct {
	Name        string                     `json:"name"`
	Status      string                     `json:"status"`
	OutputFiles map[string]minerUV1FileRef `json:"output_files"`
	Error       *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Parse *struct {
		ModelUsed     string `json:"model_used"`
		DurationMS    int64  `json:"duration_ms"`
		ParserVersion string `json:"parser_version"`
	} `json:"parse"`
}

type minerUV1Job struct {
	JobID  string            `json:"job_id"`
	Status string            `json:"status"`
	Tier   string            `json:"tier"`
	Files  []minerUV1JobFile `json:"files"`
}

func minerUV1JobTerminal(status string) bool {
	switch status {
	case "completed", "partial", "failed", "canceled":
		return true
	}
	return false
}

// --- protocol detection ---

// detectMinerUProtocol probes GET /v1/health. MinerU 4.0 answers 200 (or 503
// while models are still preloading); older servers have no such route and
// answer 404/405. When the service sits behind an authenticating API gateway
// or reverse proxy, the configured apiKey is attached as Authorization: Bearer.
func detectMinerUProtocol(
	ctx context.Context, client *http.Client, endpoint, apiKey string,
) (minerUProtocol, error) {
	endpoint = strings.TrimRight(endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/health", nil)
	if err != nil {
		return minerUProtocolLegacy, fmt.Errorf("create health request: %w", err)
	}
	if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return minerUProtocolLegacy, fmt.Errorf("MinerU service unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	switch resp.StatusCode {
	case http.StatusOK:
		var health struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(body, &health); err != nil || health.Status != "ok" {
			return minerUProtocolLegacy, fmt.Errorf("unexpected /v1/health response: %s",
				truncateForLog(string(body), mineruV1ErrorPreviewBytes))
		}
		return minerUProtocolV1, nil
	case http.StatusUnauthorized:
		if apiKey == "" {
			return minerUProtocolV1, fmt.Errorf("MinerU service requires authentication, please configure API key")
		}
		if msg := minerUV1ErrorMessage(body); msg != "" {
			return minerUProtocolV1, fmt.Errorf("MinerU authentication failed (status 401): %s", msg)
		}
		return minerUProtocolV1, fmt.Errorf("MinerU /v1/health status 401: %s",
			truncateForLog(string(body), mineruV1ErrorPreviewBytes))
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return minerUProtocolLegacy, nil
	default:
		if msg := minerUV1ErrorMessage(body); msg != "" {
			return minerUProtocolV1, fmt.Errorf("MinerU service not ready (status %d): %s", resp.StatusCode, msg)
		}
		return minerUProtocolLegacy, fmt.Errorf("MinerU /v1/health status %d: %s",
			resp.StatusCode, truncateForLog(string(body), mineruV1ErrorPreviewBytes))
	}
}

func minerUV1ErrorMessage(body []byte) string {
	var env minerUV1ErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	if env.Error.Code != "" && env.Error.Message != "" {
		return env.Error.Code + ": " + env.Error.Message
	}
	return firstNonEmpty(env.Error.Message, env.Error.Code)
}

// truncateForLog cuts s to at most n bytes, backing up to a rune boundary
// so a multibyte character in the body is never split.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// --- parse flow ---

// ParseWithLayout uploads content, runs a parse job and returns the markdown,
// the images extracted from the job's zip artifact and the package's layout
// list (per-block page and box; nil when the package carried none).
//
// 内层预算：这里在调用方 ctx 之上再套一层 jobTimeout 的超时，
// context.WithTimeout 取两者更早的截止时间，因此内层预算永远不会超过
// 外层剩余时间（FR-14 ③）。
func (c *minerUV1Client) ParseWithLayout(
	ctx context.Context,
	content []byte,
	fileName string,
	opts minerUV1ParseOptions,
) (md string, refs []types.ImageRef, contentList []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, c.jobTimeout)
	defer cancel()

	fileID, err := c.upload(ctx, content, fileName)
	if err != nil {
		return "", nil, nil, fmt.Errorf("upload: %w", err)
	}

	// FR-14 ④：解析失败或被取消时清理远端中间产物（job + 已上传文件），
	// 不留孤儿。清理走独立的 background ctx，因为此时调用方 ctx 往往已经结束。
	jobID := ""
	defer func() {
		if err != nil {
			c.cleanupRemote(jobID, fileID)
		}
	}()

	jobID, err = c.createJob(ctx, fileID, opts)
	if err != nil {
		return "", nil, nil, fmt.Errorf("create parse job: %w", err)
	}
	logger.Infof(ctx, "[%s] parse job created: job_id=%s file_id=%s tier=%q ocr_mode=%s",
		c.logLabel, jobID, fileID, opts.Tier, opts.OCRMode)

	job, err := c.waitJob(ctx, jobID)
	if err != nil {
		return "", nil, nil, err
	}

	zipRef, err := minerUV1ZipOutput(job)
	if err != nil {
		return "", nil, nil, err
	}

	zipData, err := c.downloadFile(ctx, zipRef.FileID)
	if err != nil {
		return "", nil, nil, fmt.Errorf("download zip artifact: %w", err)
	}

	md, refs, err = extractMarkdownZip(zipData, c.logLabel)
	if err != nil {
		return "", nil, nil, fmt.Errorf("extract zip artifact: %w", err)
	}
	return md, refs, minerUContentListFromZip(zipData), nil
}

func (c *minerUV1Client) upload(ctx context.Context, content []byte, fileName string) (string, error) {
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])

	var created minerUV1Upload
	err := c.doJSON(ctx, http.MethodPost, "/v1/uploads", map[string]interface{}{
		"filename":  fileName,
		"bytes":     len(content),
		"mime_type": minerUV1MimeType(fileName),
		"purpose":   "parse",
		"sha256sum": sha,
	}, &created)
	if err != nil {
		return "", err
	}

	switch created.Status {
	case "completed":
		// Same bytes already stored on the server: no transfer needed.
		if created.File == nil || created.File.ID == "" {
			return "", fmt.Errorf("upload completed without file id")
		}
		return created.File.ID, nil
	case "pending":
	default:
		return "", fmt.Errorf("unexpected upload status %q", created.Status)
	}
	if created.ID == "" || created.UploadURL == "" {
		return "", fmt.Errorf("pending upload missing id or upload_url")
	}

	if err := c.putBytes(ctx, &created, content); err != nil {
		return "", err
	}

	var completed minerUV1Upload
	err = c.doJSON(ctx, http.MethodPost, "/v1/uploads/"+url.PathEscape(created.ID)+"/complete",
		map[string]string{"sha256sum": sha}, &completed)
	if err != nil {
		return "", fmt.Errorf("complete upload: %w", err)
	}
	if completed.File == nil || completed.File.ID == "" {
		return "", fmt.Errorf("complete upload returned no file id (status %q)", completed.Status)
	}
	return completed.File.ID, nil
}

// putBytes sends the file to the URL returned by POST /v1/uploads. The MinerU
// API key is attached only when that URL is same-origin with the configured
// endpoint: the official API hands out pre-signed object-storage URLs that
// carry their own authorization and must never receive our key.
func (c *minerUV1Client) putBytes(ctx context.Context, up *minerUV1Upload, content []byte) error {
	target, err := c.resolveURL(up.UploadURL)
	if err != nil {
		return fmt.Errorf("invalid upload_url: %w", err)
	}
	method := strings.ToUpper(stringOr(up.UploadMethod, http.MethodPut))

	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("create upload request: %w", err)
	}
	for k, v := range up.UploadHeaders {
		req.Header.Set(k, v)
	}
	if c.sameOrigin(target) {
		c.setAuth(req)
	}

	resp, err := c.bulkClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload bytes: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("upload bytes status %d: %s", resp.StatusCode,
			truncateForLog(string(body), mineruV1ErrorPreviewBytes))
	}
	return nil
}

func (c *minerUV1Client) createJob(ctx context.Context, fileID string, opts minerUV1ParseOptions) (string, error) {
	payload := map[string]interface{}{
		"files": []map[string]interface{}{
			{"source": map[string]string{"type": "file_id", "file_id": fileID}},
		},
		"output_formats": []string{"zip"},
	}
	if opts.Tier != "" {
		payload["tier"] = opts.Tier
	}
	if opts.OCRMode != "" {
		payload["ocr_mode"] = opts.OCRMode
	}

	var job minerUV1Job
	if err := c.doJSON(ctx, http.MethodPost, "/v1/parse/jobs", payload, &job); err != nil {
		return "", err
	}
	if job.JobID == "" {
		return "", fmt.Errorf("response has no job_id")
	}
	return job.JobID, nil
}

func (c *minerUV1Client) waitJob(ctx context.Context, jobID string) (*minerUV1Job, error) {
	interval := c.pollInitial
	for poll := 1; ; poll++ {
		var job minerUV1Job
		err := c.doJSON(ctx, http.MethodGet, "/v1/parse/jobs/"+url.PathEscape(jobID), nil, &job)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("parse job %s: %w", jobID, ctxErr)
			}
			var apiErr *minerUV1APIError
			if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
				// Job state lives in process memory; a restarted server forgets it.
				return nil, fmt.Errorf("parse job %s no longer exists (MinerU restarted?): %w", jobID, err)
			}
			logger.Warnf(ctx, "[%s] poll #%d for job %s failed: %v", c.logLabel, poll, jobID, err)
		} else {
			if poll == 1 || poll%10 == 0 || minerUV1JobTerminal(job.Status) {
				logger.Infof(ctx, "[%s] poll #%d: job=%s status=%s", c.logLabel, poll, jobID, job.Status)
			}
			if minerUV1JobTerminal(job.Status) {
				return &job, nil
			}
		}

		sleepCtx(ctx, interval)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("parse job %s: %w", jobID, ctxErr)
		}
		interval *= 2
		if interval > c.pollMax {
			interval = c.pollMax
		}
	}
}

// cleanupRemote cancels a job we stopped waiting for and deletes the uploaded
// file so the server neither keeps burning GPU time nor keeps orphan artifacts
// (FR-14 ④). 它跑在独立 ctx 上：调用方的 ctx 通常已经超时或被取消。
func (c *minerUV1Client) cleanupRemote(jobID, fileID string) {
	if jobID != "" {
		c.cancelJob(jobID)
	}
	if fileID != "" {
		c.deleteFile(fileID)
	}
}

// cancelJob best-effort cancels a parse job.
func (c *minerUV1Client) cancelJob(jobID string) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cancelBudget)
	defer cancel()
	if err := c.doJSON(ctx, http.MethodDelete, "/v1/parse/jobs/"+url.PathEscape(jobID), nil, nil); err != nil {
		logger.Warnf(ctx, "[%s] cancel job %s: %v", c.logLabel, jobID, err)
		return
	}
	logger.Infof(ctx, "[%s] canceled parse job %s", c.logLabel, jobID)
}

// deleteFile best-effort deletes an uploaded file (and, server-side, the
// artifacts derived from it) so a failed or canceled parse leaves no orphan
// object on the remote endpoint.
func (c *minerUV1Client) deleteFile(fileID string) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cancelBudget)
	defer cancel()
	if err := c.doJSON(ctx, http.MethodDelete, "/v1/files/"+url.PathEscape(fileID), nil, nil); err != nil {
		logger.Warnf(ctx, "[%s] delete remote file %s: %v", c.logLabel, fileID, err)
		return
	}
	logger.Infof(ctx, "[%s] deleted remote file %s", c.logLabel, fileID)
}

// minerUV1ZipOutput returns the zip artifact of the single submitted file, or
// the per-file error when parsing did not succeed.
func minerUV1ZipOutput(job *minerUV1Job) (minerUV1FileRef, error) {
	if len(job.Files) == 0 {
		return minerUV1FileRef{}, fmt.Errorf("parse job %s ended %s without file results", job.JobID, job.Status)
	}
	f := job.Files[0]
	if f.Status != "completed" {
		reason := job.Status
		if f.Error != nil {
			reason = firstNonEmpty(strings.TrimSpace(f.Error.Code+": "+f.Error.Message), job.Status)
		}
		return minerUV1FileRef{}, fmt.Errorf("parse job %s %s: %s", job.JobID, job.Status, reason)
	}
	ref, ok := f.OutputFiles["zip"]
	if !ok || ref.FileID == "" {
		return minerUV1FileRef{}, fmt.Errorf("parse job %s completed without zip output", job.JobID)
	}
	return ref, nil
}

func (c *minerUV1Client) downloadFile(ctx context.Context, fileID string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/files/"+url.PathEscape(fileID)+"/content", nil)
	if err != nil {
		return nil, err
	}
	// Go drops Authorization when following a redirect to another host, so a
	// 302 to a CDN (official API) does not leak the key.
	c.setAuth(req)

	resp, err := c.bulkClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, newMinerUV1APIError(resp.StatusCode, body)
	}
	// 与 zip 下载路径同一上界（FR-07）：先按上界读，超限即拒绝而不是先缓冲。
	data, err := readResultPackage(resp.Body, c.maxZipBytes)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// --- HTTP helpers ---

type minerUV1APIError struct {
	status int
	msg    string
}

func (e *minerUV1APIError) Error() string {
	return fmt.Sprintf("MinerU API status %d: %s", e.status, e.msg)
}

func newMinerUV1APIError(status int, body []byte) *minerUV1APIError {
	msg := minerUV1ErrorMessage(body)
	if msg == "" {
		msg = truncateForLog(string(body), 500)
	}
	return &minerUV1APIError{status: status, msg: msg}
}

func (c *minerUV1Client) doJSON(ctx context.Context, method, path string, in, out interface{}) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.setAuth(req)

	resp, err := c.apiClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := readCapped(resp.Body, mineruV1MaxJSONBytes)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newMinerUV1APIError(resp.StatusCode, respBody)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	return nil
}

func (c *minerUV1Client) setAuth(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

// resolveURL resolves a (possibly relative) URL handed back by the server
// against the configured base, and rejects anything but http/https so a
// server-supplied upload_url cannot point at another scheme.
func (c *minerUV1Client) resolveURL(raw string) (*url.URL, error) {
	base, err := url.Parse(c.baseURL + "/")
	if err != nil {
		return nil, err
	}
	ref, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	resolved := base.ResolveReference(ref)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", resolved.Scheme)
	}
	return resolved, nil
}

// sameOrigin compares scheme, host and effective port with the base URL.
// 凭据只发给同源端点：预签名对象存储 URL 自带授权，绝不能收到我们的 key。
func (c *minerUV1Client) sameOrigin(u *url.URL) bool {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(base.Scheme, u.Scheme) &&
		strings.EqualFold(base.Hostname(), u.Hostname()) &&
		effectivePort(base) == effectivePort(u)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// Slim container images often ship without /etc/mime.types, where Go only
// knows a handful of extensions; cover the formats MinerU accepts explicitly.
var minerUV1MimeTypes = map[string]string{
	".pdf":  "application/pdf",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".bmp":  "image/bmp",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	".html": "text/html",
	".htm":  "text/html",
}

func minerUV1MimeType(fileName string) string {
	ext := strings.ToLower(filepath.Ext(fileName))
	if t, ok := minerUV1MimeTypes[ext]; ok {
		return t
	}
	if ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			if mediaType, _, err := mime.ParseMediaType(t); err == nil {
				return mediaType
			}
			return t
		}
	}
	return "application/octet-stream"
}
