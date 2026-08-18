package docparser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

const markerDoclingTimeout = 600 * time.Second

// ---------------------------------------------------------------------------
// Marker — self-hosted Marker (datalab-to/marker) HTTP service
// ---------------------------------------------------------------------------

// MarkerReader calls a self-hosted Marker service (marker serve / DMS) to
// convert PDFs. Marker excels at academic documents: dual-column layout,
// tables, equations, and section detection.
type MarkerReader struct {
	endpoint string // e.g. http://marker:8080 (serving /v1/marker)
	forceOCR bool
}

// NewMarkerReader creates a reader from ParserEngineOverrides.
func NewMarkerReader(overrides map[string]string) *MarkerReader {
	return &MarkerReader{
		endpoint: strings.TrimRight(overrides["marker_endpoint"], "/"),
		forceOCR: parseBoolOr(overrides["marker_force_ocr"], false),
	}
}

func (c *MarkerReader) Read(ctx context.Context, req *types.ReadRequest) (*types.ReadResult, error) {
	if c.endpoint == "" {
		return &types.ReadResult{Error: "Marker endpoint is not configured"}, nil
	}
	if err := validateMinerUOutboundURL(c.endpoint); err != nil {
		return &types.ReadResult{Error: err.Error()}, nil
	}
	if len(req.FileContent) == 0 {
		return &types.ReadResult{Error: "no file content provided"}, nil
	}

	logger.Infof(ctx, "[Marker] Parsing file=%s size=%d via %s", req.FileName, len(req.FileContent), c.endpoint)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", req.FileName)
	if err != nil {
		return nil, fmt.Errorf("marker create form file: %w", err)
	}
	if _, err := part.Write(req.FileContent); err != nil {
		return nil, fmt.Errorf("marker write file content: %w", err)
	}
	_ = writer.WriteField("force_ocr", fmt.Sprintf("%v", c.forceOCR))
	_ = writer.WriteField("output_format", "markdown")
	writer.Close()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/marker", &body)
	if err != nil {
		return nil, fmt.Errorf("marker create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())

	client := utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{
		Timeout:      markerDoclingTimeout,
		MaxRedirects: 5,
	})
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("marker HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("marker API status %d: %s", resp.StatusCode, string(respBody))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("marker read response: %w", err)
	}

	var parsed struct {
		Markdown string `json:"markdown"`
		HTML     string `json:"html"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("marker decode response: %w", err)
	}
	md := parsed.Markdown
	if md == "" && parsed.HTML != "" {
		// Markdown renderers accept embedded HTML (tables etc.), same as the
		// MinerU path which keeps HTML table blocks verbatim.
		md = parsed.HTML
	}
	if md == "" {
		return nil, fmt.Errorf("marker response contains no markdown content")
	}

	imageRefs, md := extractMarkerImages(md)
	md, imageRefs = ensureOriginalImageRef(req, md, imageRefs)

	logger.Infof(ctx, "[Marker] Parsed successfully, markdown=%d chars, images=%d", len(md), len(imageRefs))
	return &types.ReadResult{
		MarkdownContent: md,
		ImageRefs:       imageRefs,
	}, nil
}

// extractMarkerImages strips remote image refs into ImageRef entries.
// Self-hosted marker commonly returns data URIs; those are not storable by
// the image pipeline and are dropped, leaving the alt text.
func extractMarkerImages(md string) ([]types.ImageRef, string) {
	var refs []types.ImageRef
	if !strings.Contains(md, "](data:image") {
		return refs, md
	}
	// Drop inline data-URI images (too large for ImageRef URL field).
	lines := strings.Split(md, "\n")
	for i, ln := range lines {
		if idx := strings.Index(ln, "](data:image"); idx >= 0 {
			lines[i] = ln[:idx+1] + "image)"
		}
	}
	return refs, strings.Join(lines, "\n")
}

// PingMarker checks marker service health.
func PingMarker(endpoint string) (bool, string) {
	client := utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{
		Timeout:      5 * time.Second,
		MaxRedirects: 3,
	})
	resp, err := client.Get(strings.TrimRight(endpoint, "/") + "/health")
	if err != nil {
		return false, fmt.Sprintf("cannot reach Marker service: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return false, fmt.Sprintf("Marker health check returned %d", resp.StatusCode)
	}
	return true, ""
}

// ---------------------------------------------------------------------------
// Docling — self-hosted docling-serve (IBM) HTTP service
// ---------------------------------------------------------------------------

// DoclingReader calls a self-hosted docling-serve instance. Docling provides
// accurate table structure recognition (TableFormer) and reading order for
// technical/financial documents.
type DoclingReader struct {
	endpoint string // e.g. http://docling-serve:5001 (serving /v1/convert/file)
	doOCR    bool
	doTables bool
}

// NewDoclingReader creates a reader from ParserEngineOverrides.
func NewDoclingReader(overrides map[string]string) *DoclingReader {
	return &DoclingReader{
		endpoint: strings.TrimRight(overrides["docling_endpoint"], "/"),
		doOCR:    parseBoolOr(overrides["docling_ocr"], true),
		doTables: parseBoolOr(overrides["docling_tables"], true),
	}
}

func (c *DoclingReader) Read(ctx context.Context, req *types.ReadRequest) (*types.ReadResult, error) {
	if c.endpoint == "" {
		return &types.ReadResult{Error: "Docling endpoint is not configured"}, nil
	}
	if err := validateMinerUOutboundURL(c.endpoint); err != nil {
		return &types.ReadResult{Error: err.Error()}, nil
	}
	if len(req.FileContent) == 0 {
		return &types.ReadResult{Error: "no file content provided"}, nil
	}

	logger.Infof(ctx, "[Docling] Parsing file=%s size=%d via %s", req.FileName, len(req.FileContent), c.endpoint)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", req.FileName)
	if err != nil {
		return nil, fmt.Errorf("docling create form file: %w", err)
	}
	if _, err := part.Write(req.FileContent); err != nil {
		return nil, fmt.Errorf("docling write file content: %w", err)
	}
	_ = writer.WriteField("to_formats", "md")
	_ = writer.WriteField("do_ocr", fmt.Sprintf("%v", c.doOCR))
	_ = writer.WriteField("do_table_structure", fmt.Sprintf("%v", c.doTables))
	writer.Close()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/convert/file", &body)
	if err != nil {
		return nil, fmt.Errorf("docling create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", writer.FormDataContentType())

	client := utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{
		Timeout:      markerDoclingTimeout,
		MaxRedirects: 5,
	})
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("docling HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("docling API status %d: %s", resp.StatusCode, string(respBody))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("docling read response: %w", err)
	}

	// docling-serve response: {"document": {"md_content": "...", ...}, ...}
	var parsed struct {
		Document struct {
			MDContent string `json:"md_content"`
		} `json:"document"`
		MDContent string `json:"md_content"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("docling decode response: %w", err)
	}
	md := parsed.Document.MDContent
	if md == "" {
		md = parsed.MDContent
	}
	if md == "" {
		return nil, fmt.Errorf("docling response contains no md_content")
	}

	imageRefs, md := extractMarkerImages(md)
	md, imageRefs = ensureOriginalImageRef(req, md, imageRefs)

	logger.Infof(ctx, "[Docling] Parsed successfully, markdown=%d chars, images=%d", len(md), len(imageRefs))
	return &types.ReadResult{
		MarkdownContent: md,
		ImageRefs:       imageRefs,
	}, nil
}

// PingDocling checks docling-serve health.
func PingDocling(endpoint string) (bool, string) {
	client := utils.NewSSRFSafeHTTPClient(utils.SSRFSafeHTTPClientConfig{
		Timeout:      5 * time.Second,
		MaxRedirects: 3,
	})
	resp, err := client.Get(strings.TrimRight(endpoint, "/") + "/health")
	if err != nil {
		return false, fmt.Sprintf("cannot reach Docling service: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return false, fmt.Sprintf("Docling health check returned %d", resp.StatusCode)
	}
	return true, ""
}
