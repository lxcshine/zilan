package handler

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// BackupHandler exposes the backup & recovery admin surface
// (PRD docs/prd/data-backup-recovery.md §6.2). Every route is mounted
// behind RequireSystemAdmin; when the subsystem is disabled the handler
// answers 503 so operators get an explicit signal instead of a silent
// 404.
type BackupHandler struct {
	backupService interfaces.BackupService
	userService   interfaces.UserService
}

// NewBackupHandler constructs the handler. userService may be nil in
// minimal test wiring — restore jobs then record an empty creator.
func NewBackupHandler(backupService interfaces.BackupService, userService interfaces.UserService) *BackupHandler {
	return &BackupHandler{backupService: backupService, userService: userService}
}

// requireEnabled answers 503 when the subsystem is off.
func (h *BackupHandler) requireEnabled(c *gin.Context) bool {
	if h.backupService == nil || !h.backupService.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"success": false,
			"error":   "backup subsystem is disabled (set WEKNORA_BACKUP_ENABLED=true and restart)",
		})
		return false
	}
	return true
}

// RunBackup godoc
// @Summary      手动触发全量备份
// @Description  立即执行一次全量快照（元数据 + 文件层）。同一时刻仅允许一个备份任务运行。
// @Tags         系统管理/备份与恢复
// @Produce      json
// @Success      200  {object}  gin.H
// @Failure      409  {object}  errors.AppError  "已有备份在运行"
// @Failure      503  {object}  errors.AppError  "备份子系统未启用"
// @Security     Bearer
// @Router       /system/backup/run [post]
func (h *BackupHandler) RunBackup(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	ctx := c.Request.Context()
	record, err := h.backupService.RunBackup(ctx, types.BackupTriggerManual)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"trigger": "manual"})
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "already running") {
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"success": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": record})
}

// ListRecords godoc
// @Summary      备份记录列表
// @Description  按开始时间倒序返回备份记录，可选状态过滤与分页。
// @Tags         系统管理/备份与恢复
// @Produce      json
// @Param        status  query  string  false  "running | succeeded | failed"
// @Param        limit   query  int     false  "页大小，默认 50，上限 200"
// @Param        offset  query  int     false  "偏移量"
// @Success      200  {object}  gin.H
// @Security     Bearer
// @Router       /system/backup/records [get]
func (h *BackupHandler) ListRecords(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	ctx := c.Request.Context()
	limit, offset := paginationFromQuery(c)
	records, err := h.backupService.ListRecords(ctx, c.Query("status"), limit, offset)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}

	// The status card's RPO countdown needs the newest success — avoid
	// a second round trip from the frontend.
	latest, _ := h.backupService.GetLatestSucceeded(ctx)
	c.JSON(http.StatusOK, gin.H{
		"success":         true,
		"data":            records,
		"latest_succeeded": latest,
	})
}

// GetRecord godoc
// @Summary      备份记录详情
// @Description  返回记录与其 manifest 摘要（各空间条目、校验和、统计）。
// @Tags         系统管理/备份与恢复
// @Produce      json
// @Param        id  path  string  true  "备份记录ID"
// @Success      200  {object}  gin.H
// @Security     Bearer
// @Router       /system/backup/records/{id} [get]
func (h *BackupHandler) GetRecord(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	ctx := c.Request.Context()
	record, manifest, err := h.backupService.GetRecord(ctx, c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": record, "manifest": manifest})
}

// DeleteRecord godoc
// @Summary      删除备份
// @Description  删除一条备份记录及其全部存储对象（保留策略外的手工清理）。
// @Tags         系统管理/备份与恢复
// @Produce      json
// @Param        id  path  string  true  "备份记录ID"
// @Success      200  {object}  gin.H
// @Security     Bearer
// @Router       /system/backup/records/{id} [delete]
func (h *BackupHandler) DeleteRecord(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	ctx := c.Request.Context()
	if err := h.backupService.DeleteBackup(ctx, c.Param("id")); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"backup_id": c.Param("id")})
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ExportTenant godoc
// @Summary      按空间导出备份包
// @Description  流式产出单个空间的 tar.gz 归档（元数据 jsonl.gz + 全部对象文件），直接下载。
// @Tags         系统管理/备份与恢复
// @Produce      application/gzip
// @Param        id  path  string  true  "空间ID"
// @Success      200  {file}  binary
// @Security     Bearer
// @Router       /system/backup/tenants/{id}/export [post]
func (h *BackupHandler) ExportTenant(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	ctx := c.Request.Context()
	tenantID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || tenantID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "invalid workspace id"})
		return
	}

	c.Header("Content-Disposition",
		`attachment; filename="workspace-`+strconv.FormatUint(tenantID, 10)+`-export.tar.gz"`)
	c.Header("Content-Type", "application/gzip")
	c.Status(http.StatusOK)

	size, err := h.backupService.ExportTenant(ctx, tenantID, c.Writer)
	if err != nil {
		// Headers are already sent — the stream is aborted mid-body.
		// The audit row and server log carry the failure reason.
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"tenant_id": tenantID})
		return
	}
	logger.Infof(ctx, "[Backup] workspace export streamed: tenant=%d bytes=%d", tenantID, size)
}

// restoreRequest is the POST /system/backup/restore body.
type restoreRequest struct {
	BackupID     string `json:"backup_id"      binding:"required"`
	Scope        string `json:"scope"          binding:"required,oneof=instance tenant"`
	TenantID     uint64 `json:"tenant_id"`
	ConflictMode string `json:"conflict_mode" binding:"omitempty,oneof=overwrite new"`
	DryRun       bool   `json:"dry_run"`
}

// StartRestore godoc
// @Summary      发起恢复
// @Description  校验快照后异步执行恢复。dry_run=true 仅做校验与冲突分析。scope=tenant 需要 conflict_mode（默认 new）。
// @Tags         系统管理/备份与恢复
// @Accept       json
// @Produce      json
// @Param        body  body  restoreRequest  true  "恢复请求"
// @Success      202  {object}  gin.H
// @Security     Bearer
// @Router       /system/backup/restore [post]
func (h *BackupHandler) StartRestore(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	ctx := c.Request.Context()
	var req restoreRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	if req.Scope == types.RestoreScopeTenant && req.TenantID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false,
			"error": "tenant_id is required for scope=tenant"})
		return
	}
	if req.Scope == types.RestoreScopeTenant && req.ConflictMode == "" {
		req.ConflictMode = types.RestoreConflictNew
	}

	job := &types.BackupRestoreJob{
		BackupID:     req.BackupID,
		Scope:        req.Scope,
		TenantID:     req.TenantID,
		ConflictMode: req.ConflictMode,
		CreatedBy:    h.currentUserID(ctx),
	}
	if req.DryRun {
		job.Status = types.RestoreStatusDryRun
	}

	created, err := h.backupService.StartRestore(ctx, job)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"backup_id": req.BackupID})
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"success": true, "data": created})
}

// GetRestoreJob godoc
// @Summary      恢复任务进度
// @Description  前端轮询恢复任务状态（校验/恢复/索引重建各阶段）。
// @Tags         系统管理/备份与恢复
// @Produce      json
// @Param        jobId  path  string  true  "恢复任务ID"
// @Success      200  {object}  gin.H
// @Security     Bearer
// @Router       /system/backup/restore/{jobId} [get]
func (h *BackupHandler) GetRestoreJob(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	job, err := h.backupService.GetRestoreJob(c.Request.Context(), c.Param("jobId"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": job})
}

// GetConfig godoc
// @Summary      读取备份配置
// @Description  返回备份子系统的生效配置（凭证脱敏，仅返回目标是否已配置）。
// @Tags         系统管理/备份与恢复
// @Produce      json
// @Success      200  {object}  gin.H
// @Security     Bearer
// @Router       /system/backup/config [get]
func (h *BackupHandler) GetConfig(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	info, err := h.backupService.GetConfig(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": info})
}

// UpdateConfig godoc
// @Summary      更新备份配置
// @Description  运行时调整保留策略与备份窗口；窗口变更后调度器立即按新表达式重载，无需重启。
// @Tags         系统管理/备份与恢复
// @Accept       json
// @Produce      json
// @Param        body  body  types.BackupConfigUpdate  true  "配置更新（nil 字段保持不变）"
// @Success      200  {object}  gin.H
// @Security     Bearer
// @Router       /system/backup/config [put]
func (h *BackupHandler) UpdateConfig(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	ctx := c.Request.Context()
	var update types.BackupConfigUpdate
	if err := c.ShouldBindJSON(&update); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	info, err := h.backupService.UpdateConfig(ctx, &update)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": info})
}

// paginationFromQuery parses limit/offset with sane clamps.
func paginationFromQuery(c *gin.Context) (limit, offset int) {
	limit = 50
	if raw := c.Query("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}
	if limit > 200 {
		limit = 200
	}
	if raw := c.Query("offset"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			offset = v
		}
	}
	return limit, offset
}

// currentUserID resolves the acting admin's user id (empty when the
// user service is unwired — the audit row still records the action).
func (h *BackupHandler) currentUserID(ctx context.Context) string {
	if h.userService == nil {
		return ""
	}
	if user, err := h.userService.GetCurrentUser(ctx); err == nil && user != nil {
		return user.ID
	}
	return ""
}
