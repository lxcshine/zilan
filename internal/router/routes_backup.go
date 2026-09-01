package router

import (
	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/handler"
)

// RegisterBackupRoutes wires the backup & recovery admin surface
// (PRD docs/prd/data-backup-recovery.md §6.2).
//
// The whole group sits behind g.SystemAdmin() — backup and restore are
// platform-scope operations even when they target a single workspace,
// and every mutation is audited inside the service layer. Mounting the
// guard at the group level means adding an endpoint here can never
// accidentally drop the gate.
//
// The handler itself answers 503 when the subsystem is disabled, so a
// non-backup deployment still gets a clean signal instead of 404s.
func RegisterBackupRoutes(r *gin.RouterGroup, backupHandler *handler.BackupHandler, g *rbacGuards) {
	if backupHandler == nil {
		return
	}
	backup := r.Group("/system/backup", g.SystemAdmin())
	{
		backup.POST("/run", backupHandler.RunBackup)
		backup.GET("/records", backupHandler.ListRecords)
		backup.GET("/records/:id", backupHandler.GetRecord)
		backup.DELETE("/records/:id", backupHandler.DeleteRecord)
		backup.POST("/tenants/:id/export", backupHandler.ExportTenant)
		backup.POST("/restore", backupHandler.StartRestore)
		backup.GET("/restore/:jobId", backupHandler.GetRestoreJob)
		backup.GET("/config", backupHandler.GetConfig)
		backup.PUT("/config", backupHandler.UpdateConfig)
	}
}
