package session

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
)

// UploadTemporaryDocument accepts one multipart file and immediately returns
// a session-scoped document ID. Parsing continues in the document worker.
func (h *Handler) UploadTemporaryDocument(c *gin.Context) {
	ctx := c.Request.Context()
	sessionID := c.Param("session_id")
	// Uploading attaches content to the session, so use the strict owner scope:
	// a tenant admin may read an API-key session but must not add attachments.
	if _, err := h.sessionService.GetOwnedSession(ctx, sessionID); err != nil {
		c.Error(apperrors.NewNotFoundError("Session not found"))
		return
	}
	maxBytes := secutils.GetMaxFileSizeMB()*1024*1024 + 1024*1024
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
	fileHeader, err := c.FormFile("file")
	if err != nil {
		c.Error(apperrors.NewBadRequestError(fmt.Sprintf("invalid attachment upload: %v", err)))
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		c.Error(apperrors.NewBadRequestError("failed to open attachment"))
		return
	}
	defer file.Close()

	agent, _ := h.resolveAgent(ctx, c, c.PostForm("agent_id"))
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(fileHeader.Filename)), ".")
	parserEngine := strings.TrimSpace(c.PostForm("parser_engine"))
	if agent != nil {
		if len(agent.Config.SupportedFileTypes) > 0 && !containsFileType(agent.Config.SupportedFileTypes, ext) {
			c.Error(apperrors.NewBadRequestError("file type is not supported by this agent"))
			return
		}
		if isAudioExtension(ext) && (!agent.Config.AudioUploadEnabled || agent.Config.ASRModelID == "") {
			c.Error(apperrors.NewBadRequestError("audio upload is not enabled or no ASR model is configured"))
			return
		}
	}
	document, err := h.temporaryDocuments.Create(
		ctx, c.GetUint64(types.TenantIDContextKey.String()), sessionID,
		fileHeader.Filename, fileHeader.Header.Get("Content-Type"), fileHeader.Size, file,
		temporaryDocumentCreateOptions(agent, ext, parserEngine),
	)
	if err != nil {
		c.Error(apperrors.NewBadRequestError(err.Error()))
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"success": true, "data": document.ForClient()})
}

// temporaryDocumentCreateOptions 依据 Agent 配置推导一份上传解析选项。
// ext 是不带点的文件扩展名；parserEngine 是调用方显式指定的引擎（可为空）。
// 这里只负责组装选项，类型/音频是否允许由调用方自己校验。
func temporaryDocumentCreateOptions(
	agent *types.CustomAgent, ext, parserEngine string,
) types.TemporaryDocumentCreateOptions {
	options := types.TemporaryDocumentCreateOptions{ParserEngine: strings.TrimSpace(parserEngine)}
	if agent == nil {
		return options
	}
	// 只有在 Agent 开了音频上传时才用 ASR。
	if isAudioExtension(ext) && agent.Config.AudioUploadEnabled && agent.Config.ASRModelID != "" {
		options.ASRModelID = agent.Config.ASRModelID
	}
	// 调用方没有显式指定引擎时，用 Agent 的对话解析规则；
	// 租户级规则仍是最终兜底，在解析 worker 里生效。
	if options.ParserEngine == "" || options.ParserEngine == "auto" {
		if engine := agent.Config.ResolveChatParserEngine(ext); engine != "" {
			options.ParserEngine = engine
		}
	}
	// 图片理解（caption/OCR）用 Agent 的 VLM 模型。图片总是受益；
	// 扫描件/纯图文件只在 Agent 打开 AttachmentImageUnderstanding 时才做 OCR。
	if agent.Config.ImageUploadEnabled && agent.Config.VLMModelID != "" {
		options.VLMModelID = agent.Config.VLMModelID
		options.ImageUnderstanding = agent.Config.AttachmentImageUnderstanding
		options.OCRMaxPages = agent.Config.AttachmentOCRMaxPages
	}
	return options
}

// persistAttachmentUploads 把 base64 直传的附件落成 TemporaryDocument 并入队后台解析
// （PRD FR-22 ①）。它不做任何解析，因此不会阻塞对话开始。
//
// 失败语义：只有"确定性的请求级错误"才返回 error（体积超限、base64 解码失败、
// Agent 不支持该类型、连源文件都没存下来）。入队失败时 Create 会返回一份
// Status=Failed 的文档，那属于附件级失败 —— 这里照常收下，交给流式事件告知用户，
// 不再让整轮 HTTP 请求失败。
func (h *Handler) persistAttachmentUploads(
	ctx context.Context,
	tenantID uint64,
	sessionID string,
	uploads []AttachmentUpload,
	agent *types.CustomAgent,
) ([]string, types.MessageAttachments, error) {
	// MAX_FILE_SIZE_MB env (50MB default). See utils/filesize.go for
	// why this is deploy-time-only rather than a runtime setting.
	maxSizeMB := secutils.GetMaxFileSizeMB()
	maxSize := maxSizeMB * 1024 * 1024
	for i, upload := range uploads {
		if upload.FileSize > maxSize {
			return nil, nil, fmt.Errorf("attachment %d exceeds size limit of %dMB", i+1, maxSizeMB)
		}
	}

	ids := make([]string, 0, len(uploads))
	metas := make(types.MessageAttachments, 0, len(uploads))
	for i, upload := range uploads {
		data, err := DecodeBase64Attachment(upload.Data)
		if err != nil {
			return nil, nil, fmt.Errorf("attachment %d decode failed: %w", i+1, err)
		}
		if int64(len(data)) > maxSize {
			return nil, nil, fmt.Errorf("attachment %d exceeds size limit of %dMB", i+1, maxSizeMB)
		}
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(upload.FileName)), ".")
		if agent != nil && len(agent.Config.SupportedFileTypes) > 0 &&
			!containsFileType(agent.Config.SupportedFileTypes, ext) {
			return nil, nil, fmt.Errorf("file type %s is not supported by this agent", ext)
		}
		mimeType, _ := secutils.SafeContentTypeByFilename(upload.FileName)
		document, createErr := h.temporaryDocuments.Create(
			ctx, tenantID, sessionID, upload.FileName, mimeType, int64(len(data)),
			bytes.NewReader(data), temporaryDocumentCreateOptions(agent, ext, ""),
		)
		if createErr != nil && document == nil {
			return nil, nil, fmt.Errorf("attachment %d could not be stored: %w", i+1, createErr)
		}
		if createErr != nil {
			logger.Warnf(ctx, "attachment %d queued as %s: %v", i+1, document.Status, createErr)
		}
		ids = append(ids, document.ID)
		metas = append(metas, types.MessageAttachment{
			ID: document.ID, URL: document.ResourceRef, FileName: document.FileName,
			FileType: document.FileType, FileSize: document.FileSize,
		})
	}
	return ids, metas, nil
}

func (h *Handler) ListTemporaryDocuments(c *gin.Context) {
	ctx := c.Request.Context()
	sessionID := sessionIDParam(c)
	if _, err := h.sessionService.GetSession(ctx, sessionID); err != nil {
		c.Error(apperrors.NewNotFoundError("Session not found"))
		return
	}
	documents, err := h.temporaryDocuments.List(ctx, c.GetUint64(types.TenantIDContextKey.String()), sessionID)
	if err != nil {
		c.Error(apperrors.NewInternalServerError(err.Error()))
		return
	}
	// Metadata 里可能带有服务端的图片理解缓存，对客户端输出前必须剥离。
	out := make([]*types.TemporaryDocument, 0, len(documents))
	for _, document := range documents {
		out = append(out, document.ForClient())
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": out})
}

func (h *Handler) GetTemporaryDocument(c *gin.Context) {
	ctx := c.Request.Context()
	sessionID := sessionIDParam(c)
	if _, err := h.sessionService.GetSession(ctx, sessionID); err != nil {
		c.Error(apperrors.NewNotFoundError("Session not found"))
		return
	}
	document, err := h.temporaryDocuments.Get(ctx, c.GetUint64(types.TenantIDContextKey.String()), sessionID, c.Param("attachment_id"))
	if err != nil {
		c.Error(apperrors.NewInternalServerError(err.Error()))
		return
	}
	if document == nil {
		c.Error(apperrors.NewNotFoundError("Attachment not found"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": document.ForClient()})
}

func (h *Handler) PreviewTemporaryDocument(c *gin.Context) {
	ctx := c.Request.Context()
	sessionID := sessionIDParam(c)
	if _, err := h.sessionService.GetSession(ctx, sessionID); err != nil {
		c.Error(apperrors.NewNotFoundError("Session not found"))
		return
	}
	attachmentID := secutils.SanitizeForLog(c.Param("attachment_id"))
	if attachmentID == "" {
		c.Error(apperrors.NewBadRequestError("Attachment ID cannot be empty"))
		return
	}
	file, filename, err := h.temporaryDocuments.OpenFile(
		ctx, c.GetUint64(types.TenantIDContextKey.String()), sessionID, attachmentID,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			c.Error(apperrors.NewNotFoundError("Attachment not found"))
			return
		}
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(apperrors.NewInternalServerError("Failed to retrieve attachment").WithDetails(err.Error()))
		return
	}
	defer file.Close()

	contentType, inline := secutils.SafeContentTypeByFilename(filename)
	c.Header("Content-Type", contentType)
	c.Header("X-Content-Type-Options", "nosniff")
	disposition := "inline"
	if !inline {
		disposition = "attachment"
	}
	c.Header("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": filename}))
	c.Header("Cache-Control", "private, max-age=3600")

	c.Stream(func(w io.Writer) bool {
		if _, err := io.Copy(w, file); err != nil {
			logger.Errorf(ctx, "Failed to stream attachment preview: %v", err)
			return false
		}
		return false
	})
}

func (h *Handler) DeleteTemporaryDocument(c *gin.Context) {
	ctx := c.Request.Context()
	sessionID := sessionIDParam(c)
	// Deleting mutates the session's attachments, so use the strict owner scope:
	// a tenant admin may read an API-key session but must not remove attachments.
	if _, err := h.sessionService.GetOwnedSession(ctx, sessionID); err != nil {
		c.Error(apperrors.NewNotFoundError("Session not found"))
		return
	}
	if err := h.temporaryDocuments.Delete(ctx, c.GetUint64(types.TenantIDContextKey.String()), sessionID, c.Param("attachment_id")); err != nil {
		c.Error(apperrors.NewInternalServerError(err.Error()))
		return
	}
	c.Status(http.StatusNoContent)
}

func sessionIDParam(c *gin.Context) string {
	if value := c.Param("session_id"); value != "" {
		return value
	}
	return c.Param("id")
}

func containsFileType(supported []string, ext string) bool {
	for _, item := range supported {
		if strings.TrimPrefix(strings.ToLower(strings.TrimSpace(item)), ".") == ext {
			return true
		}
	}
	return false
}

func isAudioExtension(ext string) bool {
	switch ext {
	case "mp3", "wav", "m4a", "flac", "ogg", "aac":
		return true
	default:
		return false
	}
}
