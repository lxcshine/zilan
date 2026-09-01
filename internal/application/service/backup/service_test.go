package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// testDBSeq gives every test its own shared-cache in-memory database so
// parallel CREATE TABLE statements don't collide.
var testDBSeq int64

// newTestDB opens a shared-cache in-memory SQLite database with the
// subset of business tables the backup flow exercises.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	name := fmt.Sprintf("backup-svc-test-%d-%d", time.Now().UnixNano(), atomic.AddInt64(&testDBSeq, 1))
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func mustExec(t *testing.T, db *gorm.DB, ddl string) {
	t.Helper()
	if err := db.Exec(ddl).Error; err != nil {
		t.Fatalf("exec ddl %q: %v", ddl, err)
	}
}

// seedBusinessSchema creates minimal business tables with the columns
// the exporter introspects (tenant_id scoping, restore order).
func seedBusinessSchema(t *testing.T, db *gorm.DB) {
	t.Helper()
	mustExec(t, db, `CREATE TABLE tenants (id INTEGER PRIMARY KEY, name TEXT, status TEXT DEFAULT 'active', created_at DATETIME, updated_at DATETIME, deleted_at DATETIME)`)
	mustExec(t, db, `CREATE TABLE users (id TEXT PRIMARY KEY, tenant_id INTEGER, username TEXT, email TEXT, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME)`)
	mustExec(t, db, `CREATE TABLE tenant_members (id INTEGER PRIMARY KEY, tenant_id INTEGER, user_id TEXT, role TEXT, created_at DATETIME)`)
	mustExec(t, db, `CREATE TABLE knowledge_bases (id TEXT PRIMARY KEY, tenant_id INTEGER, name TEXT, created_at DATETIME)`)
	mustExec(t, db, `CREATE TABLE knowledge (id TEXT PRIMARY KEY, tenant_id INTEGER, knowledge_base_id TEXT, title TEXT, file_path TEXT, file_hash TEXT, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME)`)
	if err := db.AutoMigrate(&types.BackupRecord{}, &types.BackupRestoreJob{}); err != nil {
		t.Fatalf("migrate backup tables: %v", err)
	}
}

func seedBusinessData(t *testing.T, db *gorm.DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO tenants (id, name, created_at, updated_at) VALUES (1, 'alpha', '2026-01-01', '2026-01-01'), (2, 'beta', '2026-01-02', '2026-01-02')`)
	mustExec(t, db, `INSERT INTO users (id, tenant_id, username, email) VALUES ('u1', 1, 'alice', 'a@x.io'), ('u2', 2, 'bob', 'b@x.io')`)
	mustExec(t, db, `INSERT INTO tenant_members (id, tenant_id, user_id, role) VALUES (1, 1, 'u1', 'owner'), (2, 2, 'u2', 'owner')`)
	mustExec(t, db, `INSERT INTO knowledge_bases (id, tenant_id, name) VALUES ('kb1', 1, 'docs'), ('kb2', 2, 'notes')`)
	mustExec(t, db, `INSERT INTO knowledge (id, tenant_id, knowledge_base_id, title, file_path, file_hash) VALUES ('k1', 1, 'kb1', 'manual', '', ''), ('k2', 2, 'kb2', 'report', '', '')`)
}

// newTestService assembles the service against the temp-dir local
// store, returning the store root so tests can inspect snapshot files.
func newTestService(t *testing.T, db *gorm.DB, mutate func(*config.BackupConfig)) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Backup: &config.BackupConfig{
			Enabled:  true,
			Schedule: "0 0 3 * * *",
			Storage: &config.BackupStorageConfig{
				Provider:  "local",
				LocalPath: dir,
			},
		},
	}
	if mutate != nil {
		mutate(cfg.Backup)
	}
	repo := repository.NewBackupRepository(db)
	tenantRepo := repository.NewTenantRepository(db)
	return NewBackupService(cfg, db, repo, tenantRepo, nil, nil, nil, nil), dir
}

func TestRunBackupAndManifestIntegrity(t *testing.T) {
	db := newTestDB(t)
	seedBusinessSchema(t, db)
	seedBusinessData(t, db)
	svc, storeDir := newTestService(t, db, nil)

	record, err := svc.RunBackup(context.Background(), types.BackupTriggerManual)
	if err != nil {
		t.Fatalf("run backup: %v", err)
	}
	if record.Status != types.BackupStatusSucceeded {
		t.Fatalf("status = %s (error: %s)", record.Status, record.Error)
	}
	if record.BasePath == "" {
		t.Fatal("base path not assigned")
	}

	// The record list + latest-succeeded surface.
	records, err := svc.ListRecords(context.Background(), "", 10, 0)
	if err != nil || len(records) != 1 {
		t.Fatalf("list records: %v (n=%d)", err, len(records))
	}
	latest, err := svc.GetLatestSucceeded(context.Background())
	if err != nil || latest == nil || latest.ID != record.ID {
		t.Fatalf("latest succeeded: %v %+v", err, latest)
	}

	// Manifest must exist on disk with both workspace entries.
	manifestPath := filepath.Join(storeDir, filepath.FromSlash(record.BasePath), "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest types.BackupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.BackupID != record.ID {
		t.Fatalf("manifest backup id %q != %q", manifest.BackupID, record.ID)
	}
	if len(manifest.Tenants) != 2 {
		t.Fatalf("expected 2 workspace entries, got %d", len(manifest.Tenants))
	}
	if manifest.FullDump == nil || manifest.FullDump.Rows == 0 {
		t.Fatalf("full dump missing or empty: %+v", manifest.FullDump)
	}

	// Stats reflect the export.
	var stats types.BackupStats
	_ = json.Unmarshal([]byte(record.Stats), &stats)
	if stats.Workspaces != 2 || stats.Rows == 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	// In-process single flight: a concurrent trigger is rejected.
	svc.running.Store(true)
	if _, err := svc.RunBackup(context.Background(), types.BackupTriggerManual); err == nil {
		t.Fatal("expected single-flight rejection while running")
	}
	svc.running.Store(false)
}

func TestRunBackupDisabled(t *testing.T) {
	db := newTestDB(t)
	svc, _ := newTestService(t, db, func(b *config.BackupConfig) { b.Enabled = false })
	if _, err := svc.RunBackup(context.Background(), types.BackupTriggerManual); err == nil {
		t.Fatal("expected disabled error")
	}
}

// TestTenantExportImportRoundTrip exercises the per-workspace jsonl path:
// export → decode → import into a fresh DB, including the users
// membership filter and the topological restore order.
func TestTenantExportImportRoundTrip(t *testing.T) {
	db := newTestDB(t)
	seedBusinessSchema(t, db)
	seedBusinessData(t, db)

	specs, err := discoverTables(context.Background(), db)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	index := specIndex(specs)

	// Tenant 1's view: tenants row + member user + membership + KB +
	// knowledge; NOT tenant 2's rows.
	var buf bytesBuffer
	enc := json.NewEncoder(&buf)
	svcLike := &Service{db: db}
	var total int64
	for _, spec := range sortedForRestore(specs) {
		if spec.Name == backupRecordsTable || spec.Name == backupRestoreJobsTable {
			continue
		}
		n, err := svcLike.exportOneTable(context.Background(), spec, 1, enc)
		if err != nil {
			t.Fatalf("export %s: %v", spec.Name, err)
		}
		total += n
	}
	if total != 5 { // tenants, users, tenant_members, knowledge_bases, knowledge — one row each
		t.Fatalf("expected 5 rows for workspace 1, got %d", total)
	}

	var records []*jsonlRecord
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for {
		var rec jsonlRecord
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode jsonl: %v", err)
		}
		records = append(records, &rec)
	}
	// Every row belongs to workspace 1 (or is its tenant row).
	for _, rec := range records {
		if rec.Table != "tenants" && rec.Table != "users" {
			if id, _ := rec.Row["tenant_id"].(float64); id != 1 {
				t.Fatalf("row from wrong workspace: %s %+v", rec.Table, rec.Row)
			}
		}
	}

	// Import into a fresh database: same schema, empty.
	db2 := newTestDB(t)
	seedBusinessSchema(t, db2)
	specs2, err := discoverTables(context.Background(), db2)
	if err != nil {
		t.Fatalf("discover 2: %v", err)
	}
	report := (&Service{db: db2}).importRows(context.Background(), records, specIndex(specs2), 0)
	if report.RowsRestored != 5 || report.RowsSkipped != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}

	// Re-import the same rows: all conflict-skipped, zero duplicates.
	report = (&Service{db: db2}).importRows(context.Background(), records, specIndex(specs2), 0)
	if report.RowsRestored != 0 || report.RowsSkipped != 5 {
		t.Fatalf("idempotent re-import report: %+v", report)
	}
	var count int64
	db2.Raw(`SELECT COUNT(*) FROM knowledge`).Scan(&count)
	if count != 1 {
		t.Fatalf("duplicate rows after re-import: %d", count)
	}
	_ = index
}

// newRestoreJob builds and persists a restore job the way StartRestore
// does, so doRestore's phase persistence has a row to update.
func newRestoreJob(t *testing.T, svc *Service, backupID string, dryRun bool) *types.BackupRestoreJob {
	t.Helper()
	job := &types.BackupRestoreJob{
		ID:           "rs_test_" + fmt.Sprintf("%d", time.Now().UnixNano()),
		BackupID:     backupID,
		Scope:        types.RestoreScopeTenant,
		TenantID:     1,
		ConflictMode: types.RestoreConflictNew,
		Status:       types.RestoreStatusPending,
		Progress:     types.JSON("{}"),
		CreatedBy:    "admin",
	}
	if dryRun {
		job.Status = types.RestoreStatusDryRun
	}
	if err := svc.repo.CreateRestoreJob(context.Background(), job); err != nil {
		t.Fatalf("create restore job: %v", err)
	}
	return job
}

// TestRestoreTenantNewMode covers the "new" conflict strategy in the
// 误删 (accidental deletion) scenario: the workspace's data is wiped,
// then restored into a fresh workspace with remapped ids and the
// -restored- name suffix.
func TestRestoreTenantNewMode(t *testing.T) {
	db := newTestDB(t)
	seedBusinessSchema(t, db)
	seedBusinessData(t, db)
	svc, _ := newTestService(t, db, nil)

	record, err := svc.RunBackup(context.Background(), types.BackupTriggerManual)
	if err != nil {
		t.Fatalf("run backup: %v", err)
	}

	// Simulate the accident: workspace 1's data is gone.
	mustExec(t, db, `DELETE FROM knowledge WHERE tenant_id = 1`)
	mustExec(t, db, `DELETE FROM knowledge_bases WHERE tenant_id = 1`)
	mustExec(t, db, `DELETE FROM tenant_members WHERE tenant_id = 1`)
	mustExec(t, db, `DELETE FROM users WHERE tenant_id = 1`)
	mustExec(t, db, `DELETE FROM tenants WHERE id = 1`)

	job := newRestoreJob(t, svc, record.ID, false)
	if err := svc.doRestore(context.Background(), job, false); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if job.Status != types.RestoreStatusSucceeded {
		t.Fatalf("job status: %s", job.Status)
	}
	var report types.RestoreReport
	_ = json.Unmarshal([]byte(job.Report), &report)
	if report.NewTenantID == 0 || report.NewTenantID == 1 {
		t.Fatalf("expected remapped workspace id, got %d", report.NewTenantID)
	}
	if report.RowsRestored == 0 {
		t.Fatalf("no rows restored: %+v", report)
	}

	// The cloned workspace exists with the remapped id and the
	// -restored-{date} name suffix; workspace 2 is untouched.
	var names []string
	db.Raw(`SELECT name FROM tenants ORDER BY id`).Scan(&names)
	if len(names) != 2 || names[0] != "beta" || names[1] != "alpha-restored-"+time.Now().UTC().Format("20060102") {
		t.Fatalf("unexpected workspaces after clone restore: %v", names)
	}

	// Knowledge rows remapped to the new workspace.
	var kCount int64
	db.Raw(`SELECT COUNT(*) FROM knowledge WHERE tenant_id = ?`, report.NewTenantID).Scan(&kCount)
	if kCount != 1 {
		t.Fatalf("expected 1 remapped knowledge row, got %d", kCount)
	}
}

// TestRestoreTenantDryRun covers the verification-only path.
func TestRestoreTenantDryRun(t *testing.T) {
	db := newTestDB(t)
	seedBusinessSchema(t, db)
	seedBusinessData(t, db)
	svc, _ := newTestService(t, db, nil)

	record, err := svc.RunBackup(context.Background(), types.BackupTriggerManual)
	if err != nil {
		t.Fatalf("run backup: %v", err)
	}

	job := newRestoreJob(t, svc, record.ID, true)
	if err := svc.doRestore(context.Background(), job, true); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if job.Status != types.RestoreStatusDryRun {
		t.Fatalf("dry-run status: %s", job.Status)
	}
	var report types.RestoreReport
	_ = json.Unmarshal([]byte(job.Report), &report)
	if !report.DryRun || report.WouldRestoreRows == 0 {
		t.Fatalf("dry-run report: %+v", report)
	}
	// Nothing was actually written.
	var kCount int64
	db.Raw(`SELECT COUNT(*) FROM knowledge`).Scan(&kCount)
	if kCount != 2 {
		t.Fatalf("dry-run must not write, knowledge rows = %d", kCount)
	}
}

// TestRestoreDetectsDamagedSnapshot flips a byte in the stored metadata
// blob and expects the restore to refuse with the entry named.
func TestRestoreDetectsDamagedSnapshot(t *testing.T) {
	db := newTestDB(t)
	seedBusinessSchema(t, db)
	seedBusinessData(t, db)
	svc, storeDir := newTestService(t, db, nil)

	record, err := svc.RunBackup(context.Background(), types.BackupTriggerManual)
	if err != nil {
		t.Fatalf("run backup: %v", err)
	}

	blobPath := filepath.Join(storeDir, filepath.FromSlash(record.BasePath), "metadata", "tenants", "1.jsonl.gz")
	raw, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	raw[len(raw)-1] ^= 0xFF
	if err := os.WriteFile(blobPath, raw, 0o644); err != nil {
		t.Fatalf("damage blob: %v", err)
	}

	job := newRestoreJob(t, svc, record.ID, false)
	err = svc.doRestore(context.Background(), job, false)
	if err == nil {
		t.Fatal("expected integrity failure to abort restore")
	}
	if !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("error should name the damage, got: %v", err)
	}
}

// bytesBuffer is a tiny io.Writer for the jsonl encoder.
type bytesBuffer struct{ b []byte }

func (w *bytesBuffer) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

func (w *bytesBuffer) Bytes() []byte { return w.b }
