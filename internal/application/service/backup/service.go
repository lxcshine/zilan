package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
)

// Service implements interfaces.BackupService — the top-level backup &
// recovery facade (PRD docs/prd/data-backup-recovery.md).
//
// Tier strategy:
//   - metadata: pure-Go logical export to jsonl.gz (full-instance +
//     per-workspace). Works identically on PostgreSQL and SQLite — a
//     deliberate deviation from the PRD's pg_dump/.backup sketch: no
//     external binary dependency, and the restore path is the same code
//     on every deployment flavour including Lite.
//   - files: per-workspace object copy driven by the DB's own file
//     references (knowledge.file_path + chunk image_info + message
//     images), which is provider-uniform — FileService has no prefix
//     listing, but every stored object is referenced from metadata, so
//     the effect equals a prefix walk and is exact.
//   - index: never backed up; restore queues reparse.
type Service struct {
	cfg             *config.Config
	repo            interfaces.BackupRepository
	db              *gorm.DB
	tenantRepo      interfaces.TenantRepository
	storageResolver interfaces.StorageBackendResolver
	resourceCatalog interfaces.ResourceCatalog
	knowledgeSvc    interfaces.KnowledgeService
	auditSvc        interfaces.AuditLogService

	store     BackupStorage
	storeOnce sync.Once
	storeErr  error

	masterKey []byte

	running atomic.Bool
	cron    *cron.Cron
	cronMu  sync.Mutex
}

// NewBackupService constructs the facade. knowledgeSvc may be nil (skip
// reindex orchestration); auditSvc may be nil (audit rows dropped).
func NewBackupService(
	cfg *config.Config,
	db *gorm.DB,
	repo interfaces.BackupRepository,
	tenantRepo interfaces.TenantRepository,
	storageResolver interfaces.StorageBackendResolver,
	resourceCatalog interfaces.ResourceCatalog,
	knowledgeSvc interfaces.KnowledgeService,
	auditSvc interfaces.AuditLogService,
) *Service {
	s := &Service{
		cfg:             cfg,
		repo:            repo,
		db:              db,
		tenantRepo:      tenantRepo,
		storageResolver: storageResolver,
		resourceCatalog: resourceCatalog,
		knowledgeSvc:    knowledgeSvc,
		auditSvc:        auditSvc,
	}
	if raw := secutils.GetAESKey(); raw != nil {
		s.masterKey = deriveMasterKey(raw)
	}
	return s
}

var _ interfaces.BackupService = (*Service)(nil)

// Backup bookkeeping tables — excluded from every snapshot: restoring
// them would resurrect stale lock rows and finished-job bookkeeping.
const (
	backupRecordsTable     = "backup_records"
	backupRestoreJobsTable = "backup_restore_jobs"
)

// Enabled reports whether the subsystem is active.
func (s *Service) Enabled() bool {
	return s.cfg != nil && s.cfg.Backup != nil && s.cfg.Backup.Enabled
}

// getStore lazily builds the backup target. Construction errors are
// cached — a misconfigured store fails every operation with the same
// readable reason instead of retrying on each call.
func (s *Service) getStore() (BackupStorage, error) {
	s.storeOnce.Do(func() {
		if !s.Enabled() {
			s.storeErr = errors.New("backup is disabled")
			return
		}
		if s.cfg.Backup.Encrypt && s.masterKey == nil {
			s.storeErr = errors.New("backup.encrypt is enabled but SYSTEM_AES_KEY is not set to a 32-byte value")
			return
		}
		s.store, s.storeErr = NewBackupStorage(s.cfg.Backup.Storage)
	})
	return s.store, s.storeErr
}

// encryptEnabled reports whether metadata blobs should be sealed.
func (s *Service) encryptEnabled() bool {
	return s.Enabled() && s.cfg.Backup.Encrypt && s.masterKey != nil
}

// ---- scheduling -------------------------------------------------------

// StartScheduler registers the daily cron entry. Failures are logged,
// never fatal to boot (PRD §2.2 B5 — a bad schedule must not take the
// instance down). The returned func stops the runner.
func (s *Service) StartScheduler(ctx context.Context) func() {
	if !s.Enabled() {
		return func() {}
	}
	s.cronMu.Lock()
	defer s.cronMu.Unlock()
	if s.cron != nil {
		return func() {}
	}

	schedule := s.cfg.Backup.Schedule
	c := cron.New(cron.WithSeconds(), cron.WithChain(cron.Recover(cron.DefaultLogger)))
	entryID, err := c.AddFunc(schedule, func() {
		runCtx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
		defer cancel()
		if _, runErr := s.RunBackup(runCtx, types.BackupTriggerScheduled); runErr != nil {
			logger.Errorf(runCtx, "[Backup] scheduled run failed: %v", runErr)
		}
	})
	if err != nil {
		logger.Errorf(ctx, "[Backup] invalid schedule %q, scheduler NOT started: %v", schedule, err)
		return func() {}
	}
	c.Start()
	s.cron = c
	logger.Infof(ctx, "[Backup] scheduler started (schedule=%q entry=%d target=%s)", schedule, entryID, s.storeDescribe())

	return func() {
		s.cronMu.Lock()
		defer s.cronMu.Unlock()
		if s.cron != nil {
			stopCtx := s.cron.Stop()
			<-stopCtx.Done()
			s.cron = nil
		}
	}
}

func (s *Service) storeDescribe() string {
	store, err := s.getStore()
	if err != nil {
		return "unavailable: " + err.Error()
	}
	return store.Describe()
}

// ---- backup -----------------------------------------------------------

// RunBackup executes one full snapshot synchronously.
func (s *Service) RunBackup(ctx context.Context, trigger string) (*types.BackupRecord, error) {
	if !s.Enabled() {
		return nil, errors.New("backup is disabled (WEKNORA_BACKUP_ENABLED=false)")
	}
	store, err := s.getStore()
	if err != nil {
		return nil, err
	}

	// Single-flight: in-process CAS first (cheap), then the DB-level
	// running-row guard that also covers cross-instance overlap.
	if !s.running.CompareAndSwap(false, true) {
		return nil, errors.New("another backup is already running in this process")
	}
	defer s.running.Store(false)

	if running, err := s.repo.HasRunning(ctx); err != nil {
		return nil, fmt.Errorf("check running backup: %w", err)
	} else if running {
		return nil, errors.New("another backup is already running (backup:full:lock)")
	}

	started := time.Now().UTC()
	record := &types.BackupRecord{
		ID:          "bk_" + started.Format("20060102_150405"),
		TriggerType: trigger,
		Status:      types.BackupStatusRunning,
		StartedAt:   started,
		BasePath:    "",
	}
	if err := s.repo.CreateRecord(ctx, record); err != nil {
		// A duplicate snapshot id (same second) collapses to this error,
		// which is the documented same-second race behaviour.
		return nil, fmt.Errorf("create backup record: %w", err)
	}

	logger.Infof(ctx, "[Backup] starting %s trigger=%s", record.ID, trigger)
	rec, err := s.executeBackup(ctx, store, record)
	if err != nil {
		now := time.Now().UTC()
		record.Status = types.BackupStatusFailed
		record.FinishedAt = &now
		record.Error = err.Error()
		if updErr := s.repo.UpdateRecord(ctx, record); updErr != nil {
			logger.Errorf(ctx, "[Backup] failed to persist failure for %s: %v", record.ID, updErr)
		}
		logger.Errorf(ctx, "[Backup] %s FAILED after %s: %v", record.ID, time.Since(started), err)
		return record, err
	}

	s.audit(ctx, types.AuditActionBackupRun, types.AuditOutcomeSuccess, "backup", rec.ID,
		map[string]any{"trigger": trigger, "status": rec.Status})
	logger.Infof(ctx, "[Backup] %s succeeded in %s (workspaces=%d files=%d rows=%d)",
		rec.ID, time.Since(started), statsOf(rec).Workspaces, statsOf(rec).Files, statsOf(rec).Rows)

	// Retention sweep runs after the record is terminal; its failures are
	// logged, not fatal to the backup itself.
	if err := s.applyRetention(ctx); err != nil {
		logger.Warnf(ctx, "[Backup] retention sweep failed: %v", err)
	}
	return rec, nil
}

// executeBackup performs the snapshot phases and persists the terminal
// record state. Every early return has already persisted the failed
// status via the RunBackup wrapper.
func (s *Service) executeBackup(ctx context.Context, store BackupStorage, record *types.BackupRecord) (*types.BackupRecord, error) {
	started := record.StartedAt
	basePath := s.snapshotBasePath(ctx, store, started)
	record.BasePath = basePath

	manifest := &types.BackupManifest{
		BackupID:       record.ID,
		Trigger:        record.TriggerType,
		StartedAt:      started.Format(time.RFC3339),
		InstanceVersion: instanceVersion(),
		Encrypted:      s.encryptEnabled(),
		Tenants:        []*types.BackupManifestTenant{},
		ReindexPlan:    map[string][]string{},
	}

	specs, err := discoverTables(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("discover tables: %w", err)
	}

	// Phase 1: full-instance metadata export.
	fullObj, err := s.exportTableStream(ctx, store, basePath, "metadata/full.jsonl.gz", specs, 0)
	if err != nil {
		return nil, fmt.Errorf("export full metadata: %w", err)
	}
	manifest.FullDump = fullObj

	// Phase 2: per-workspace metadata + file tiers.
	tenants, err := s.tenantRepo.ListTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}

	stats := types.BackupStats{}
	prevFiles := s.loadPreviousFileLists(ctx, store)

	for _, tenant := range tenants {
		if tenant == nil {
			continue
		}
		entry, err := s.backupTenant(ctx, store, basePath, specs, tenant, prevFiles)
		if err != nil {
			// A single broken workspace fails the whole snapshot — a
			// partial snapshot labelled "succeeded" would be a silent
			// RPO hole (DoD §11.1 requires manifest 校验 100%).
			return nil, fmt.Errorf("backup workspace %d: %w", tenant.ID, err)
		}
		manifest.Tenants = append(manifest.Tenants, entry)
		stats.Workspaces++
		if entry.Files != nil {
			stats.Files += entry.Files.Count
			stats.Bytes += entry.Files.Bytes
			stats.SkippedFiles += int64(len(entry.Files.Skipped))
		}
		if entry.Metadata != nil {
			stats.Rows += entry.Metadata.Rows
		}
	}

	// Phase 3: manifest.
	manifest.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	if err := writeManifest(ctx, store, path.Join(basePath, manifestRelPath), manifest); err != nil {
		return nil, fmt.Errorf("write manifest: %w", err)
	}

	finished := time.Now().UTC()
	stats.DurationMS = finished.Sub(started).Milliseconds()
	record.Status = types.BackupStatusSucceeded
	record.FinishedAt = &finished
	record.RetentionTag = assignRetentionTag(started)
	record.Stats = mustMarshalJSON(stats)
	if err := s.repo.UpdateRecord(ctx, record); err != nil {
		return nil, fmt.Errorf("persist backup record: %w", err)
	}
	return record, nil
}

// snapshotBasePath derives the snapshot root. Daily granularity per the
// PRD layout ({YYYYMMDD}); a second snapshot on the same day appends
// _HHMMSS so it can never overwrite the earlier one.
func (s *Service) snapshotBasePath(ctx context.Context, store BackupStorage, started time.Time) string {
	base := started.Format("20060102")
	exists, err := store.Exists(ctx, path.Join(base, manifestRelPath))
	if err == nil && !exists {
		return base
	}
	return base + "_" + started.Format("150405")
}

// backupTenant snapshots one workspace: metadata jsonl.gz, the file tier,
// and the reindex plan contribution.
func (s *Service) backupTenant(
	ctx context.Context,
	store BackupStorage,
	basePath string,
	specs []*tableSpec,
	tenant *types.Tenant,
	prevFiles map[uint64]map[string]*types.BackupFileEntry,
) (*types.BackupManifestTenant, error) {
	entry := &types.BackupManifestTenant{TenantID: tenant.ID}

	// Metadata tier.
	obj, err := s.exportTableStream(ctx, store, basePath,
		fmt.Sprintf("metadata/tenants/%d.jsonl.gz", tenant.ID), specs, tenant.ID)
	if err != nil {
		return nil, fmt.Errorf("export workspace metadata: %w", err)
	}
	entry.Metadata = obj

	// Reindex plan: every KB of the workspace needs a post-restore reparse.
	var kbIDs []string
	if kbSpec := specByName(specs, "knowledge_bases"); kbSpec != nil {
		rows, err := scanRows(s.db,
			fmt.Sprintf(`SELECT id FROM %s WHERE tenant_id = ?`, quoteIdent(s.db.Dialector.Name(), "knowledge_bases")),
			tenant.ID)
		if err == nil {
			for _, r := range rows {
				if id, _ := r["id"].(string); id != "" {
					kbIDs = append(kbIDs, id)
				}
			}
		}
	}
	entry.KnowledgeBases = len(kbIDs)

	// File tier.
	fileList, err := s.copyTenantFiles(ctx, store, basePath, tenant.ID, prevFiles[tenant.ID])
	if err != nil {
		return nil, fmt.Errorf("copy workspace files: %w", err)
	}
	entry.Files = &types.BackupManifestFiles{
		Count:   int64(len(fileList.Entries)),
		Bytes:   sumFileBytes(fileList),
		Skipped: fileListSkipped(fileList),
	}
	return entry, nil
}

// exportTableStream exports a jsonl view of the schema (all rows, or one
// workspace's rows), gzips it, optionally encrypts, stores it, and
// returns the manifest object with the plaintext digest.
func (s *Service) exportTableStream(
	ctx context.Context,
	store BackupStorage,
	basePath, relPath string,
	specs []*tableSpec,
	tenantID uint64,
) (*types.BackupManifestObject, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gz)

	var rows int64
	for _, spec := range sortedForRestore(specs) {
		if spec.Name == backupRecordsTable || spec.Name == backupRestoreJobsTable {
			// Backup bookkeeping tables are excluded from snapshots —
			// restoring them would resurrect stale lock rows.
			continue
		}
		n, err := s.exportOneTable(ctx, spec, tenantID, enc)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", spec.Name, err)
		}
		rows += n
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("close gzip stream: %w", err)
	}

	payload := buf.Bytes()
	if s.encryptEnabled() {
		sealed, err := encryptBlob(s.masterKey, payload)
		if err != nil {
			return nil, fmt.Errorf("encrypt blob: %w", err)
		}
		payload = sealed
	}
	if err := store.Put(ctx, path.Join(basePath, relPath), bytes.NewReader(payload)); err != nil {
		return nil, fmt.Errorf("store %s: %w", relPath, err)
	}
	return &types.BackupManifestObject{
		File:   relPath,
		SHA256: sha256Bytes(buf.Bytes()),
		Rows:   rows,
		Bytes:  int64(buf.Len()),
	}, nil
}

// exportOneTable streams one table's rows. tenantID > 0 selects the
// per-workspace view: tenant-scoped tables are filtered (users gets the
// membership-aware filter per PRD §4.2 仅该空间成员行), non-scoped global
// tables are skipped entirely — they belong to the instance dump only.
func (s *Service) exportOneTable(ctx context.Context, spec *tableSpec, tenantID uint64, enc *json.Encoder) (int64, error) {
	dialect := s.db.Dialector.Name()
	q := quoteIdent
	if tenantID > 0 {
		switch {
		case spec.Name == "tenants":
			// The workspace's own row (no tenant_id column on tenants).
			return exportRowsQuery(ctx, s.db, spec,
				fmt.Sprintf(`SELECT * FROM %s WHERE id = ?`, q(dialect, "tenants")),
				[]any{tenantID}, enc)
		case spec.Name == "users":
			// Include members whose primary workspace differs but who
			// hold membership in this workspace via tenant_members.
			return exportRowsQuery(ctx, s.db, spec,
				fmt.Sprintf(`SELECT * FROM %s WHERE tenant_id = ? OR id IN (SELECT user_id FROM %s WHERE tenant_id = ?)`,
					q(dialect, "users"), q(dialect, "tenant_members")),
				[]any{tenantID, tenantID}, enc)
		case spec.TenantScoped:
			return exportRowsQuery(ctx, s.db, spec,
				fmt.Sprintf(`SELECT * FROM %s WHERE tenant_id = ?`, q(dialect, spec.Name)),
				[]any{tenantID}, enc)
		default:
			// Non-scoped global config belongs to the instance dump only.
			return 0, nil
		}
	}
	return exportRows(ctx, s.db, spec, tenantID, enc)
}

// ---- file tier --------------------------------------------------------

// providerSchemes are the storage references worth copying. http(s) URLs
// in chunk image_info are remote originals, not stored objects.
var providerSchemes = []string{
	"local://", "minio://", "s3://", "cos://", "tos://", "oss://", "ks3://", "obs://", "resource://", "backend://",
}

func isStorageRef(p string) bool {
	for _, s := range providerSchemes {
		if strings.HasPrefix(p, s) {
			return true
		}
	}
	return false
}

// tenantFileRefs is the workspace's object-reference census: the ref →
// business-side content fingerprint (knowledge.file_hash) mapping used
// for incremental change detection.
type tenantFileRefs struct {
	// refs in stable (sorted) order.
	refs []string
	// sourceHash maps ref → knowledge.file_hash (empty for image refs,
	// which carry no business fingerprint and are re-verified each run).
	sourceHash map[string]string
}

// collectTenantFileRefs gathers every stored-object reference the
// workspace's metadata points at. Soft-deleted knowledge rows are
// included — a restore must be able to un-delete.
func (s *Service) collectTenantFileRefs(ctx context.Context, tenantID uint64) (*tenantFileRefs, error) {
	dialect := s.db.Dialector.Name()
	q := quoteIdent
	out := &tenantFileRefs{sourceHash: map[string]string{}}
	refs := map[string]struct{}{}

	// 1. Knowledge source documents (file_path + the business content
	// fingerprint for the incremental skip).
	rows, err := scanRows(s.db,
		fmt.Sprintf(`SELECT file_path, file_hash FROM %s WHERE tenant_id = ? AND COALESCE(file_path, '') <> ''`,
			q(dialect, "knowledge")), tenantID)
	if err != nil {
		return nil, fmt.Errorf("scan knowledge file paths: %w", err)
	}
	for _, r := range rows {
		if p, _ := r["file_path"].(string); p != "" {
			refs[p] = struct{}{}
			if h, _ := r["file_hash"].(string); h != "" {
				out.sourceHash[p] = h
			}
		}
	}

	// 2. Chunk-embedded images (image_info JSON array with url /
	// original_url fields, both possibly provider refs).
	rows, err = scanRows(s.db,
		fmt.Sprintf(`SELECT image_info FROM %s WHERE tenant_id = ? AND COALESCE(image_info, '') <> ''`,
			q(dialect, "knowledge_chunks")), tenantID)
	if err == nil {
		for _, r := range rows {
			raw, _ := r["image_info"].(string)
			for _, u := range extractImageURLs(raw) {
				refs[u] = struct{}{}
			}
		}
	}

	// 3. Chat message images (images JSON array with url fields).
	if msgSpecExists(s.db, "messages") {
		rows, err = scanRows(s.db,
			fmt.Sprintf(`SELECT images FROM %s WHERE tenant_id = ? AND COALESCE(images, '') <> '' AND images <> '[]'`,
				q(dialect, "messages")), tenantID)
		if err == nil {
			for _, r := range rows {
				raw, _ := r["images"].(string)
				for _, u := range extractImageURLs(raw) {
					refs[u] = struct{}{}
				}
			}
		}
	}

	out.refs = make([]string, 0, len(refs))
	for r := range refs {
		out.refs = append(out.refs, r)
	}
	sort.Strings(out.refs)
	return out, nil
}

// extractImageURLs pulls url/original_url values out of a JSON array of
// image objects, keeping only provider storage refs.
func extractImageURLs(raw string) []string {
	var imgs []map[string]any
	if err := json.Unmarshal([]byte(raw), &imgs); err != nil {
		return nil
	}
	var out []string
	for _, img := range imgs {
		for _, key := range []string{"url", "original_url"} {
			if u, _ := img[key].(string); u != "" && isStorageRef(u) {
				out = append(out, u)
			}
		}
	}
	return out
}

// copyTenantFiles copies every referenced object into
// files/{tenantID}/…, writes the _filelist.json ledger, and returns it.
// prev maps source-path → previous entry for the incremental skip.
func (s *Service) copyTenantFiles(
	ctx context.Context,
	store BackupStorage,
	basePath string,
	tenantID uint64,
	prev map[string]*types.BackupFileEntry,
) (*types.BackupFileList, error) {
	census, err := s.collectTenantFileRefs(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	fileList := &types.BackupFileList{TenantID: tenantID, Entries: []*types.BackupFileEntry{}}
	for _, ref := range census.refs {
		entry, err := s.copyOneFile(ctx, store, basePath, ref, census.sourceHash[ref], prev)
		if err != nil {
			// Unreadable source objects are recorded, not fatal (PRD
			// §5.1 graceful degradation — a deleted-in-flight file
			// must not void the snapshot).
			logger.Warnf(ctx, "[Backup] skip unreadable object %q: %v", ref, err)
			fileList.Entries = append(fileList.Entries, &types.BackupFileEntry{Path: ref, SHA256: "", Bytes: 0})
			continue
		}
		fileList.Entries = append(fileList.Entries, entry)
	}

	ledger, err := json.Marshal(fileList)
	if err != nil {
		return nil, err
	}
	if err := store.Put(ctx, path.Join(basePath, fmt.Sprintf("files/%d/_filelist.json", tenantID)),
		bytes.NewReader(ledger)); err != nil {
		return nil, fmt.Errorf("store file list: %w", err)
	}
	return fileList, nil
}

// copyOneFile stores one source object under files/{tenantID}/….
//
// Incremental sync (PRD §5.1 step 3): when the previous snapshot recorded
// the SAME business fingerprint (knowledge.file_hash) for this ref and
// still holds the object, the bytes are streamed store→store from the
// previous snapshot instead of re-read from the primary storage. A
// missing fingerprint (image refs) or a changed one falls back to the
// full primary read + digest + upload path.
func (s *Service) copyOneFile(
	ctx context.Context,
	store BackupStorage,
	basePath, ref, sourceHash string,
	prev map[string]*types.BackupFileEntry,
) (*types.BackupFileEntry, error) {
	key := path.Join(basePath, fileKey(ref))

	if sourceHash != "" {
		if prevEntry := prev[ref]; prevEntry != nil && prevEntry.SourceHash == sourceHash && prevEntry.SHA256 != "" {
			if ok, err := store.Exists(ctx, prevEntry.Key); err == nil && ok {
				if err := s.copyStoreObject(ctx, store, prevEntry.Key, key); err == nil {
					return &types.BackupFileEntry{
						Path: ref, Key: key, SHA256: prevEntry.SHA256,
						Bytes: prevEntry.Bytes, SourceHash: sourceHash,
					}, nil
				}
				// Store-to-store copy failed — fall through to the
				// primary read below.
				logger.Warnf(ctx, "[Backup] store-to-store copy %s → %s failed, re-reading source", prevEntry.Key, key)
			}
		}
	}

	svc, err := s.resolveFileServiceForRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	rc, err := svc.GetFile(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	// Buffer the object so the digest and the stored copy are computed
	// from identical bytes (object stores dislike double reads).
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read source: %w", err)
	}
	digest := sha256Bytes(data)
	if err := store.Put(ctx, key, bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("store object: %w", err)
	}
	return &types.BackupFileEntry{
		Path: ref, Key: key, SHA256: digest, Bytes: int64(len(data)), SourceHash: sourceHash,
	}, nil
}

// copyStoreObject streams an object from one key to another inside the
// backup store (no primary storage involved).
func (s *Service) copyStoreObject(ctx context.Context, store BackupStorage, fromKey, toKey string) error {
	rc, err := store.Get(ctx, fromKey)
	if err != nil {
		return err
	}
	defer rc.Close()
	return store.Put(ctx, toKey, rc)
}

// fileKey maps a source storage reference to a flat snapshot key:
// "minio://bucket/1/kb/a.pdf" → "files/1/minio/bucket/1/kb/a.pdf"
// (scheme separators flattened to slashes; collision-free per ref).
func fileKey(ref string) string {
	flat := strings.ReplaceAll(ref, "://", "/")
	flat = strings.Trim(flat, "/")
	tenant := "0"
	if id := secutils.ParseTenantIDFromStoragePath(ref); id > 0 {
		tenant = fmt.Sprintf("%d", id)
	}
	return path.Join("files", tenant, flat)
}

// resolveFileServiceForRef rebuilds the FileService owning a storage
// reference — the same resolution chain the chat image resolver uses
// (catalog → tenant → backend), so cross-backend refs land correctly.
func (s *Service) resolveFileServiceForRef(ctx context.Context, ref string) (interfaces.FileService, error) {
	if s.storageResolver == nil || s.tenantRepo == nil {
		return nil, errors.New("storage resolver not wired")
	}
	physical := ref
	tenantID := uint64(0)
	if s.resourceCatalog != nil {
		if resolved, resource, err := s.resourceCatalog.ResolvePath(ctx, ref); err == nil {
			physical = resolved
			if resource != nil {
				tenantID = resource.TenantID
			}
		}
	}
	if tenantID == 0 {
		tenantID = secutils.ParseTenantIDFromStoragePath(physical)
	}

	backendID, inner, scoped := types.ParseStorageBackendPath(physical)
	providerPath := physical
	if scoped {
		providerPath = inner
	}
	provider := types.ParseProviderScheme(providerPath)
	if provider == "" {
		provider = "local"
	}

	tenant, err := s.tenantRepo.GetTenantByID(ctx, tenantID)
	if err != nil || tenant == nil {
		// A ref whose owning workspace no longer exists: fall back to
		// the platform default service — still readable when the
		// default backend hosts the object.
		tenant = &types.Tenant{}
	}
	baseDir := strings.TrimSpace(os.Getenv("LOCAL_STORAGE_BASE_DIR"))
	svc, _, err := s.storageResolver.ResolveFileService(ctx, tenant, backendID, provider, baseDir)
	if err != nil {
		return nil, fmt.Errorf("resolve storage for %q: %w", ref, err)
	}
	return svc, nil
}

// loadPreviousFileLists fetches the last succeeded snapshot's per-workspace
// ledgers — the incremental baseline.
func (s *Service) loadPreviousFileLists(ctx context.Context, store BackupStorage) map[uint64]map[string]*types.BackupFileEntry {
	out := map[uint64]map[string]*types.BackupFileEntry{}
	prev, err := s.repo.GetLatestSucceeded(ctx)
	if err != nil || prev == nil {
		return out
	}
	// Walk the previous snapshot root and read every _filelist.json.
	keys, err := store.List(ctx, prev.BasePath+"/files/")
	if err != nil {
		return out
	}
	for _, key := range keys {
		if !strings.HasSuffix(key, "_filelist.json") {
			continue
		}
		rc, err := store.Get(ctx, key)
		if err != nil {
			continue
		}
		var list types.BackupFileList
		decodeErr := json.NewDecoder(rc).Decode(&list)
		rc.Close()
		if decodeErr != nil {
			continue
		}
		m := map[string]*types.BackupFileEntry{}
		for _, e := range list.Entries {
			if e != nil && e.Path != "" {
				m[e.Path] = e
			}
		}
		out[list.TenantID] = m
	}
	return out
}

// ---- retention --------------------------------------------------------

// applyRetention prunes snapshots past their GFS horizon.
func (s *Service) applyRetention(ctx context.Context) error {
	store, err := s.getStore()
	if err != nil {
		return err
	}
	b := s.cfg.Backup
	records, err := s.repo.ListSucceededBefore(ctx, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("list records for retention: %w", err)
	}
	for _, rec := range SelectExpired(records, b.RetentionDaily, b.RetentionWeekly, b.RetentionMonthly) {
		if err := s.DeleteBackup(ctx, rec.ID); err != nil {
			logger.Warnf(ctx, "[Backup] retention delete %s failed: %v", rec.ID, err)
			continue
		}
		logger.Infof(ctx, "[Backup] retention pruned %s (tag=%s)", rec.ID, rec.RetentionTag)
	}
	_ = store
	return nil
}

// ---- public API surface ----------------------------------------------

// ListRecords returns records newest-first.
func (s *Service) ListRecords(ctx context.Context, status string, limit, offset int) ([]*types.BackupRecord, error) {
	return s.repo.ListRecords(ctx, status, limit, offset)
}

// GetRecord returns one record plus its manifest (nil manifest when the
// snapshot objects are unavailable — the record row is still useful).
func (s *Service) GetRecord(ctx context.Context, id string) (*types.BackupRecord, *types.BackupManifest, error) {
	record, err := s.repo.GetRecord(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if record == nil {
		return nil, nil, fmt.Errorf("backup record %s not found", id)
	}
	manifest, err := s.readSnapshotManifest(ctx, record)
	if err != nil {
		logger.Warnf(ctx, "[Backup] manifest unavailable for %s: %v", id, err)
	}
	return record, manifest, nil
}

func (s *Service) readSnapshotManifest(ctx context.Context, record *types.BackupRecord) (*types.BackupManifest, error) {
	store, err := s.getStore()
	if err != nil {
		return nil, err
	}
	if record.BasePath == "" {
		return nil, errors.New("snapshot has no base path (failed run)")
	}
	m, err := readManifest(ctx, store, path.Join(record.BasePath, manifestRelPath))
	if err != nil {
		return nil, err
	}
	if m.Encrypted && s.masterKey != nil {
		// The manifest itself is stored in plaintext (it must remain
		// readable to enumerate damage); only the blobs are sealed.
		return m, nil
	}
	return m, nil
}

// GetLatestSucceeded returns the newest succeeded record.
func (s *Service) GetLatestSucceeded(ctx context.Context) (*types.BackupRecord, error) {
	return s.repo.GetLatestSucceeded(ctx)
}

// DeleteBackup prunes a snapshot: record row + every storage object
// under its base path.
func (s *Service) DeleteBackup(ctx context.Context, id string) error {
	record, err := s.repo.GetRecord(ctx, id)
	if err != nil {
		return err
	}
	if record == nil {
		return fmt.Errorf("backup record %s not found", id)
	}
	store, err := s.getStore()
	if err != nil {
		return err
	}
	if record.BasePath != "" {
		keys, listErr := store.List(ctx, record.BasePath+"/")
		if listErr == nil {
			for _, key := range keys {
				if delErr := store.Delete(ctx, key); delErr != nil {
					logger.Warnf(ctx, "[Backup] delete object %s: %v", key, delErr)
				}
			}
		}
	}
	if err := s.repo.DeleteRecord(ctx, id); err != nil {
		return fmt.Errorf("delete record: %w", err)
	}
	s.audit(ctx, types.AuditActionBackupDeleted, types.AuditOutcomeSuccess, "backup", id, nil)
	return nil
}

// ---- workspace export -------------------------------------------------

// ExportTenant streams a live single-workspace archive (tar.gz of the
// jsonl.gz metadata export plus every referenced object) to w. The
// archive is produced from CURRENT data — it is the "take my data with
// me" path (PRD §5.2), not a snapshot read.
func (s *Service) ExportTenant(ctx context.Context, tenantID uint64, w io.Writer) (int64, error) {
	if !s.Enabled() {
		return 0, errors.New("backup is disabled")
	}
	_, err := s.getStore()
	if err != nil {
		return 0, err
	}
	specs, err := discoverTables(ctx, s.db)
	if err != nil {
		return 0, fmt.Errorf("discover tables: %w", err)
	}

	tenant, err := s.tenantRepo.GetTenantByID(ctx, tenantID)
	if err != nil || tenant == nil {
		return 0, fmt.Errorf("workspace %d not found", tenantID)
	}

	// Metadata jsonl.gz into memory (bounded: metadata tier is small).
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gz)
	var rows int64
	for _, spec := range sortedForRestore(specs) {
		if spec.Name == backupRecordsTable || spec.Name == backupRestoreJobsTable {
			continue
		}
		n, err := s.exportOneTable(ctx, spec, tenantID, enc)
		if err != nil {
			return 0, fmt.Errorf("export %s: %w", spec.Name, err)
		}
		rows += n
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}

	counter := &countingWriter{w: w}
	tw := tar.NewWriter(gzip.NewWriter(counter))
	now := time.Now().UTC()

	writeTarEntry := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: now,
		}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}

	if err := writeTarEntry(fmt.Sprintf("metadata/tenants/%d.jsonl.gz", tenantID), buf.Bytes()); err != nil {
		return 0, err
	}

	// File tier streamed object-by-object (large, never fully buffered).
	census, err := s.collectTenantFileRefs(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	refs := census.refs
	for _, ref := range refs {
		svc, err := s.resolveFileServiceForRef(ctx, ref)
		if err != nil {
			logger.Warnf(ctx, "[Backup] export skip unreadable %q: %v", ref, err)
			continue
		}
		rc, err := svc.GetFile(ctx, ref)
		if err != nil {
			logger.Warnf(ctx, "[Backup] export skip unreadable %q: %v", ref, err)
			continue
		}
		data, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			logger.Warnf(ctx, "[Backup] export skip unreadable %q: %v", ref, readErr)
			continue
		}
		if err := writeTarEntry(fileKey(ref), data); err != nil {
			return 0, err
		}
	}

	if err := tw.Close(); err != nil {
		return 0, err
	}
	s.audit(ctx, types.AuditActionBackupTenantExported, types.AuditOutcomeSuccess, "tenant", fmt.Sprintf("%d", tenantID),
		map[string]any{"rows": rows, "files": len(refs)})
	logger.Infof(ctx, "[Backup] exported workspace %d (rows=%d files=%d bytes=%d)", tenantID, rows, len(refs), counter.n)
	return counter.n, nil
}

// ---- restore ----------------------------------------------------------

// StartRestore validates the request and launches the restore job in the
// background; the returned job is in its initial (pending) state.
func (s *Service) StartRestore(ctx context.Context, req *types.BackupRestoreJob) (*types.BackupRestoreJob, error) {
	if !s.Enabled() {
		return nil, errors.New("backup is disabled")
	}
	if _, err := s.getStore(); err != nil {
		return nil, err
	}
	record, err := s.repo.GetRecord(ctx, req.BackupID)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, fmt.Errorf("backup %s not found", req.BackupID)
	}
	if record.Status != types.BackupStatusSucceeded {
		return nil, fmt.Errorf("backup %s is %s — only succeeded snapshots can be restored", req.BackupID, record.Status)
	}

	dryRun := req.Status == types.RestoreStatusDryRun
	now := time.Now().UTC()
	job := &types.BackupRestoreJob{
		ID:            "rs_" + now.Format("20060102_150405") + fmt.Sprintf("_%04d", now.Nanosecond()%10000),
		BackupID:      req.BackupID,
		Scope:         req.Scope,
		TenantID:      req.TenantID,
		ConflictMode:  req.ConflictMode,
		Status:        types.RestoreStatusPending,
		Progress:      mustMarshalJSON(types.RestoreProgress{Phase: types.RestoreStatusPending}),
		CreatedBy:     req.CreatedBy,
		CreatedAt:     now,
	}
	if dryRun {
		job.Status = types.RestoreStatusVerifying
	}
	if err := s.repo.CreateRestoreJob(ctx, job); err != nil {
		return nil, fmt.Errorf("create restore job: %w", err)
	}

	s.audit(ctx, types.AuditActionBackupRestoreStarted, types.AuditOutcomeAccepted, "restore", job.ID,
		map[string]any{"backup_id": req.BackupID, "scope": req.Scope, "tenant_id": req.TenantID,
			"conflict_mode": req.ConflictMode, "dry_run": dryRun})

	go s.runRestore(job, dryRun)
	return job, nil
}

// GetRestoreJob returns one job for progress polling.
func (s *Service) GetRestoreJob(ctx context.Context, id string) (*types.BackupRestoreJob, error) {
	job, err := s.repo.GetRestoreJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, fmt.Errorf("restore job %s not found", id)
	}
	return job, nil
}

// runRestore executes the restore pipeline for one job.
func (s *Service) runRestore(job *types.BackupRestoreJob, dryRun bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			s.failRestore(ctx, job, fmt.Errorf("panic: %v", r))
		}
	}()

	if err := s.doRestore(ctx, job, dryRun); err != nil {
		s.failRestore(ctx, job, err)
		return
	}
	s.audit(ctx, types.AuditActionBackupRestoreDone, types.AuditOutcomeSuccess, "restore", job.ID,
		map[string]any{"backup_id": job.BackupID, "scope": job.Scope, "dry_run": dryRun})
}

func (s *Service) failRestore(ctx context.Context, job *types.BackupRestoreJob, err error) {
	now := time.Now().UTC()
	job.Status = types.RestoreStatusFailed
	job.FinishedAt = &now
	job.Report = mustMarshalJSON(types.RestoreReport{})
	setProgressMessage(job, "failed: "+err.Error())
	if updErr := s.repo.UpdateRestoreJob(ctx, job); updErr != nil {
		logger.Errorf(ctx, "[Backup] persist restore failure %s: %v (cause: %v)", job.ID, updErr, err)
	}
	logger.Errorf(ctx, "[Backup] restore %s FAILED: %v", job.ID, err)
	s.audit(ctx, types.AuditActionBackupRestoreDone, types.AuditOutcomeFailed, "restore", job.ID,
		map[string]any{"backup_id": job.BackupID, "error": err.Error()})
}

// doRestore is the phased pipeline: verify → (dry-run stop) → metadata
// import → file write-back → reindex.
func (s *Service) doRestore(ctx context.Context, job *types.BackupRestoreJob, dryRun bool) error {
	store, err := s.getStore()
	if err != nil {
		return err
	}
	record, err := s.repo.GetRecord(ctx, job.BackupID)
	if err != nil || record == nil {
		return fmt.Errorf("load backup record %s", job.BackupID)
	}

	setPhase(ctx, s.repo, job, types.RestoreStatusVerifying, "verifying snapshot integrity")
	manifest, fileLists, err := s.verifySnapshot(ctx, store, record, job)
	if err != nil {
		return err
	}
	if dryRun {
		report := types.RestoreReport{DryRun: true}
		for _, t := range manifest.Tenants {
			if job.Scope == types.RestoreScopeTenant && t.TenantID != job.TenantID {
				continue
			}
			if t.Metadata != nil {
				report.WouldRestoreRows += t.Metadata.Rows
			}
			if t.Files != nil {
				report.WouldRestoreFiles += t.Files.Count
			}
		}
		now := time.Now().UTC()
		job.Status = types.RestoreStatusDryRun
		job.FinishedAt = &now
		job.Report = mustMarshalJSON(report)
		setProgressMessage(job, "dry-run verification complete")
		return s.repo.UpdateRestoreJob(ctx, job)
	}

	// Metadata import.
	setPhase(ctx, s.repo, job, types.RestoreStatusRestoring, "importing metadata")
	report := types.RestoreReport{}
	switch job.Scope {
	case types.RestoreScopeInstance:
		if manifest.FullDump == nil {
			return errors.New("snapshot has no full-instance dump")
		}
		report, err = s.restoreFullMetadata(ctx, store, record, manifest)
	default:
		report, err = s.restoreTenantMetadata(ctx, store, record, manifest, job)
	}
	if err != nil {
		return err
	}

	// File write-back. The tenant remap (new mode) rewrites file paths
	// onto the cloned workspace.
	remap := map[uint64]uint64{}
	if job.Scope == types.RestoreScopeTenant && report.NewTenantID != 0 {
		remap[job.TenantID] = report.NewTenantID
	}
	fileReport, err := s.restoreFiles(ctx, store, record, fileLists, remap)
	if err != nil {
		return err
	}
	report.FilesRestored = fileReport

	// Reindex orchestration.
	setPhase(ctx, s.repo, job, types.RestoreStatusReindexing, "queueing index rebuild")
	reindexQueued, err := s.queueReindex(ctx, manifest, job, remap)
	if err != nil {
		return err
	}
	report.ReindexQueued = reindexQueued

	now := time.Now().UTC()
	job.Status = types.RestoreStatusSucceeded
	job.FinishedAt = &now
	job.Report = mustMarshalJSON(report)
	setProgressMessage(job, "restore complete")
	return s.repo.UpdateRestoreJob(ctx, job)
}

// verifySnapshot checks every manifest digest (metadata blobs) and every
// file ledger digest, returning the manifest and the per-workspace file
// ledgers. Any mismatch aborts the restore with the damaged entry named
// (PRD §4.3: 任一不符即拒绝恢复并明示受损条目).
func (s *Service) verifySnapshot(
	ctx context.Context, store BackupStorage, record *types.BackupRecord, job *types.BackupRestoreJob,
) (*types.BackupManifest, map[uint64]*types.BackupFileList, error) {
	manifest, err := readManifest(ctx, store, path.Join(record.BasePath, manifestRelPath))
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}

	verifyBlob := func(obj *types.BackupManifestObject, label string) error {
		if obj == nil {
			return nil
		}
		rc, err := store.Get(ctx, path.Join(record.BasePath, obj.File))
		if err != nil {
			return fmt.Errorf("%s: open %s: %w", label, obj.File, err)
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return fmt.Errorf("%s: read %s: %w", label, obj.File, err)
		}
		if manifest.Encrypted {
			if s.masterKey == nil {
				return fmt.Errorf("%s: snapshot is encrypted but SYSTEM_AES_KEY is unavailable", label)
			}
			data, err = decryptBlob(s.masterKey, data)
			if err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}
		}
		if digest := sha256Bytes(data); digest != obj.SHA256 {
			return fmt.Errorf("%s: SHA-256 mismatch for %s (manifest %s, actual %s) — snapshot damaged",
				label, obj.File, obj.SHA256, digest)
		}
		return nil
	}

	if err := verifyBlob(manifest.FullDump, "full dump"); err != nil {
		return nil, nil, err
	}
	for _, t := range manifest.Tenants {
		if job.Scope == types.RestoreScopeTenant && t.TenantID != job.TenantID {
			continue
		}
		if err := verifyBlob(t.Metadata, fmt.Sprintf("workspace %d metadata", t.TenantID)); err != nil {
			return nil, nil, err
		}
	}

	// File ledgers.
	fileLists := map[uint64]*types.BackupFileList{}
	for _, t := range manifest.Tenants {
		if job.Scope == types.RestoreScopeTenant && t.TenantID != job.TenantID {
			continue
		}
		rc, err := store.Get(ctx, path.Join(record.BasePath, fmt.Sprintf("files/%d/_filelist.json", t.TenantID)))
		if err != nil {
			if t.Files == nil || t.Files.Count == 0 {
				continue // no files for this workspace
			}
			return nil, nil, fmt.Errorf("workspace %d: open file ledger: %w", t.TenantID, err)
		}
		var list types.BackupFileList
		decodeErr := json.NewDecoder(rc).Decode(&list)
		rc.Close()
		if decodeErr != nil {
			return nil, nil, fmt.Errorf("workspace %d: decode file ledger: %w", t.TenantID, decodeErr)
		}
		for _, e := range list.Entries {
			if e == nil || e.SHA256 == "" {
				continue // skipped-at-backup object
			}
			frc, err := store.Get(ctx, e.Key)
			if err != nil {
				return nil, nil, fmt.Errorf("workspace %d: open file %s: %w", t.TenantID, e.Path, err)
			}
			digest, _, hashErr := sha256Hex(frc)
			frc.Close()
			if hashErr != nil {
				return nil, nil, fmt.Errorf("workspace %d: hash file %s: %w", t.TenantID, e.Path, hashErr)
			}
			if digest != e.SHA256 {
				return nil, nil, fmt.Errorf("workspace %d: file %s SHA-256 mismatch (ledger %s, actual %s) — snapshot damaged",
					t.TenantID, e.Path, e.SHA256, digest)
			}
		}
		fileLists[t.TenantID] = &list
	}
	return manifest, fileLists, nil
}

// restoreFullMetadata replays the full-instance jsonl: business tables
// are cleared in reverse topological order first (the pure-Go equivalent
// of pg_dump --clean), then rows are inserted in forward order.
func (s *Service) restoreFullMetadata(
	ctx context.Context, store BackupStorage, record *types.BackupRecord, manifest *types.BackupManifest,
) (types.RestoreReport, error) {
	report := types.RestoreReport{}
	rows, err := s.readJSONL(ctx, store, record, manifest.FullDump)
	if err != nil {
		return report, err
	}

	specs, err := discoverTables(ctx, s.db)
	if err != nil {
		return report, err
	}
	ordered := sortedForRestore(specs)
	present := map[string]struct{}{}
	for _, rec := range rows {
		present[rec.Table] = struct{}{}
	}

	// Clear in reverse order (children before parents).
	dialect := s.db.Dialector.Name()
	for i := len(ordered) - 1; i >= 0; i-- {
		spec := ordered[i]
		if _, ok := present[spec.Name]; !ok {
			continue
		}
		if err := s.db.WithContext(ctx).Exec(
			fmt.Sprintf(`DELETE FROM %s`, quoteIdent(dialect, spec.Name))).Error; err != nil {
			return report, fmt.Errorf("clear %s: %w", spec.Name, err)
		}
	}

	// Insert in forward order.
	index := specIndex(specs)
	report = s.importRows(ctx, rows, index, 0)
	return report, nil
}

// restoreTenantMetadata replays one workspace's jsonl. conflict_mode
// "new" clones into a fresh workspace (tenant id remapped); "overwrite"
// clears the workspace's rows first, then replays with original ids.
func (s *Service) restoreTenantMetadata(
	ctx context.Context, store BackupStorage, record *types.BackupRecord,
	manifest *types.BackupManifest, job *types.BackupRestoreJob,
) (types.RestoreReport, error) {
	report := types.RestoreReport{}
	var entry *types.BackupManifestTenant
	for _, t := range manifest.Tenants {
		if t.TenantID == job.TenantID {
			entry = t
			break
		}
	}
	if entry == nil || entry.Metadata == nil {
		return report, fmt.Errorf("snapshot has no metadata for workspace %d", job.TenantID)
	}

	rows, err := s.readJSONL(ctx, store, record, entry.Metadata)
	if err != nil {
		return report, err
	}

	specs, err := discoverTables(ctx, s.db)
	if err != nil {
		return report, err
	}
	index := specIndex(specs)
	targetTenant := job.TenantID
	newTenantID := uint64(0)

	if job.ConflictMode == types.RestoreConflictNew {
		newTenant, err := s.createRestoredTenant(ctx, job.TenantID)
		if err != nil {
			return report, err
		}
		targetTenant = newTenant.ID
		newTenantID = newTenant.ID
	} else {
		// overwrite: clear the workspace's rows, children first.
		ordered := sortedForRestore(specs)
		dialect := s.db.Dialector.Name()
		for i := len(ordered) - 1; i >= 0; i-- {
			spec := ordered[i]
			if !spec.TenantScoped {
				continue
			}
			if err := s.db.WithContext(ctx).Exec(
				fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ?`, quoteIdent(dialect, spec.Name)),
				job.TenantID).Error; err != nil {
				return report, fmt.Errorf("clear %s: %w", spec.Name, err)
			}
		}
	}

	report = s.importRows(ctx, rows, index, targetTenant)
	report.NewTenantID = newTenantID
	return report, nil
}

// createRestoredTenant provisions the "-restored-{date}" clone target.
func (s *Service) createRestoredTenant(ctx context.Context, srcID uint64) (*types.Tenant, error) {
	src, err := s.tenantRepo.GetTenantByID(ctx, srcID)
	if err != nil || src == nil {
		// Source workspace deleted (the 误删 scenario) — synthesize a
		// name; the tenants row itself is replayed from the snapshot.
		src = &types.Tenant{ID: srcID}
	}

	// The tenants row arrives via the jsonl replay (remapped); here we
	// only allocate the next id by inserting a placeholder that the
	// replay's ON CONFLICT DO NOTHING will skip in favour of… no — the
	// replay remaps tenant_id, so it inserts a NEW tenants row with the
	// new id. We must not pre-insert. Instead, allocate the id by
	// peeking the sequence-free max(id)+1 the same way the jsonl row
	// will use.
	next, err := s.nextTenantID(ctx)
	if err != nil {
		return nil, err
	}
	clone := &types.Tenant{
		ID:   next,
		Name: fmt.Sprintf("%s-restored-%s", src.Name, time.Now().Format("20060102")),
	}
	// The remapped tenants row from the jsonl will carry this id, so no
	// insert here — return the allocated identity only.
	return clone, nil
}

// nextTenantID derives max(id)+1 without inserting (PG sequences may
// skip values — restored workspaces get a fresh, collision-free id).
func (s *Service) nextTenantID(ctx context.Context) (uint64, error) {
	rows, err := scanRows(s.db, `SELECT COALESCE(MAX(id), 0) AS max_id FROM `+quoteIdent(s.db.Dialector.Name(), "tenants"))
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 1, nil
	}
	switch v := rows[0]["max_id"].(type) {
	case int64:
		return uint64(v + 1), nil
	case uint64:
		return v + 1, nil
	case float64:
		return uint64(v + 1), nil
	default:
		return 1, nil
	}
}

// importRows inserts jsonl records in table batches, remapping tenant_id
// to targetTenant when it differs from the source (the "new" conflict
// mode). Rows conflicting on primary key are skipped and reported.
func (s *Service) importRows(
	ctx context.Context, rows []*jsonlRecord, index map[string]*tableSpec, targetTenant uint64,
) types.RestoreReport {
	report := types.RestoreReport{}
	dialect := s.db.Dialector.Name()

	// Group by table, restoring in curated topological order.
	byTable := map[string][]map[string]any{}
	for _, rec := range rows {
		byTable[rec.Table] = append(byTable[rec.Table], rec.Row)
	}
	var tableNames []string
	for name := range byTable {
		tableNames = append(tableNames, name)
	}
	// Sort by restore rank.
	for i := 1; i < len(tableNames); i++ {
		for j := i; j > 0; j-- {
			rj, okj := restoreOrder[tableNames[j]]
			if !okj {
				rj = 900
			}
			rprev, okp := restoreOrder[tableNames[j-1]]
			if !okp {
				rprev = 900
			}
			if rj < rprev || (rj == rprev && tableNames[j] < tableNames[j-1]) {
				tableNames[j], tableNames[j-1] = tableNames[j-1], tableNames[j]
			} else {
				break
			}
		}
	}

	for _, name := range tableNames {
		spec := index[name]
		if spec == nil {
			spec = &tableSpec{Name: name, Order: 900}
		}
		for _, row := range byTable[name] {
			if targetTenant > 0 {
				if _, has := row["tenant_id"]; has && name != "tenants" {
					row["tenant_id"] = targetTenant
				}
				if name == "tenants" {
					row["id"] = targetTenant
					// PRD §5.4.B: the clone keeps the original name with
					// a -restored-{date} suffix so it can't collide with
					// the source workspace.
					if orig, ok := row["name"].(string); ok && orig != "" {
						row["name"] = orig + "-restored-" + time.Now().UTC().Format("20060102")
					}
				}
			}
			inserted, err := importRow(ctx, s.db, spec, row)
			if err != nil {
				report.RowsSkipped++
				if len(report.ConflictDetails) < 50 {
					report.ConflictDetails = append(report.ConflictDetails,
						fmt.Sprintf("%s: %v", name, err))
				}
				continue
			}
			if inserted {
				report.RowsRestored++
			} else {
				report.RowsSkipped++
				if len(report.ConflictDetails) < 50 {
					report.ConflictDetails = append(report.ConflictDetails,
						fmt.Sprintf("%s: primary-key conflict skipped", name))
				}
			}
		}
	}
	_ = dialect
	return report
}

// restoreFiles writes snapshot objects back to their referenced paths.
// remap (non-empty only in the tenant "new" mode) rewrites file paths
// onto the cloned workspace's namespace. Returns the count written.
func (s *Service) restoreFiles(
	ctx context.Context, store BackupStorage, record *types.BackupRecord,
	fileLists map[uint64]*types.BackupFileList, remap map[uint64]uint64,
) (int64, error) {
	var restored int64
	for _, list := range fileLists {
		if list == nil {
			continue
		}
		for _, e := range list.Entries {
			if e == nil || e.SHA256 == "" {
				continue // skipped-at-backup object
			}
			destRef := e.Path
			if len(remap) > 0 {
				destRef = remapTenantInPath(destRef, remap)
			}
			svc, err := s.resolveFileServiceForRef(ctx, destRef)
			if err != nil {
				logger.Warnf(ctx, "[Backup] restore skip %q: %v", destRef, err)
				continue
			}
			rc, err := store.Get(ctx, e.Key)
			if err != nil {
				return restored, fmt.Errorf("open snapshot object %s: %w", e.Key, err)
			}
			err = svc.WriteFileToPath(ctx, destRef, rc)
			rc.Close()
			if err != nil {
				logger.Warnf(ctx, "[Backup] restore write %q failed: %v", destRef, err)
				continue
			}
			restored++
		}
	}
	return restored, nil
}

// remapTenantInPath rewrites the leading tenant segment of a storage
// reference ("local://7/kb/a.pdf" → "local://9/kb/a.pdf" for 7→9).
func remapTenantInPath(ref string, remap map[uint64]uint64) string {
	idx := strings.Index(ref, "://")
	if idx < 0 {
		return ref
	}
	rest := ref[idx+3:]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return ref
	}
	head, tail := rest[:slash], rest[slash:]
	for from, to := range remap {
		if head == fmt.Sprintf("%d", from) {
			return ref[:idx+3] + fmt.Sprintf("%d", to) + tail
		}
	}
	return ref
}

// queueReindex triggers reparse for every knowledge row of the restored
// scope, feeding the existing async parse pipeline.
func (s *Service) queueReindex(
	ctx context.Context, manifest *types.BackupManifest, job *types.BackupRestoreJob, remap map[uint64]uint64,
) (int, error) {
	if s.knowledgeSvc == nil {
		logger.Warnf(ctx, "[Backup] knowledge service not wired — index rebuild skipped; trigger reparse manually")
		return 0, nil
	}
	dialect := s.db.Dialector.Name()
	var ids []string
	var args []any
	query := fmt.Sprintf(`SELECT id FROM %s`, quoteIdent(dialect, "knowledge"))
	if job.Scope == types.RestoreScopeTenant {
		tid := job.TenantID
		if to, ok := remap[job.TenantID]; ok {
			tid = to
		}
		query += ` WHERE tenant_id = ?`
		args = append(args, tid)
	}
	rows, err := scanRows(s.db, query, args...)
	if err != nil {
		return 0, fmt.Errorf("list knowledge for reindex: %w", err)
	}
	for _, r := range rows {
		if id, _ := r["id"].(string); id != "" {
			ids = append(ids, id)
		}
	}

	queued := 0
	for _, id := range ids {
		if _, err := s.knowledgeSvc.ReparseKnowledge(ctx, id, nil); err != nil {
			logger.Warnf(ctx, "[Backup] reparse %s failed: %v", id, err)
			continue
		}
		queued++
	}
	return queued, nil
}

// ---- pre-delete hook --------------------------------------------------

// SnapshotTenantBeforeDelete captures the workspace before deletion.
// Failures are logged but never block the deletion (PRD §7).
func (s *Service) SnapshotTenantBeforeDelete(ctx context.Context, tenantID uint64) {
	if !s.Enabled() || !s.cfg.Backup.IsPreDeleteSnapshotEnabled() {
		return
	}
	tenant, err := s.tenantRepo.GetTenantByID(ctx, tenantID)
	if err != nil || tenant == nil {
		logger.Warnf(ctx, "[Backup] pre-delete snapshot skipped: workspace %d not found", tenantID)
		return
	}
	go func() {
		snapCtx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		if _, err := s.RunBackup(snapCtx, types.BackupTriggerPreDelete); err != nil {
			logger.Warnf(snapCtx, "[Backup] pre-delete snapshot failed for workspace %d: %v", tenantID, err)
			return
		}
		logger.Infof(snapCtx, "[Backup] pre-delete snapshot captured (workspace %d)", tenantID)
	}()
}

// ---- runtime configuration -------------------------------------------

// GetConfig returns the admin-facing configuration projection.
// Credentials are never echoed — only whether the target is configured.
func (s *Service) GetConfig(ctx context.Context) (*types.BackupConfigInfo, error) {
	if s.cfg == nil || s.cfg.Backup == nil {
		return &types.BackupConfigInfo{}, nil
	}
	b := s.cfg.Backup
	info := &types.BackupConfigInfo{
		Enabled:           b.Enabled,
		Schedule:          b.Schedule,
		RetentionDaily:    b.RetentionDaily,
		RetentionWeekly:   b.RetentionWeekly,
		RetentionMonthly:  b.RetentionMonthly,
		Compression:       b.Compression,
		Encrypt:           b.Encrypt,
		PreDeleteSnapshot: b.IsPreDeleteSnapshotEnabled(),
	}
	if b.Storage != nil {
		info.Provider = b.Storage.Provider
		info.LocalPath = b.Storage.LocalPath
		info.Endpoint = b.Storage.Endpoint
		info.Bucket = b.Storage.Bucket
		info.PathPrefix = b.Storage.PathPrefix
		switch strings.ToLower(b.Storage.Provider) {
		case "local":
			info.TargetConfigured = strings.TrimSpace(b.Storage.LocalPath) != ""
		case "minio", "s3":
			info.TargetConfigured = strings.TrimSpace(b.Storage.Endpoint) != "" &&
				strings.TrimSpace(b.Storage.Bucket) != "" &&
				strings.TrimSpace(b.Storage.AccessKey) != "" &&
				strings.TrimSpace(b.Storage.SecretKey) != ""
		}
	}
	return info, nil
}

// UpdateConfig applies the runtime-adjustable subset (retention tiers +
// schedule window). Changing the schedule re-arms the cron runner so the
// new window takes effect without a restart.
func (s *Service) UpdateConfig(ctx context.Context, update *types.BackupConfigUpdate) (*types.BackupConfigInfo, error) {
	if !s.Enabled() {
		return nil, errors.New("backup is disabled")
	}
	b := s.cfg.Backup
	scheduleChanged := false

	if update == nil {
		return s.GetConfig(ctx)
	}
	if update.Schedule != nil && *update.Schedule != b.Schedule {
		schedule := strings.TrimSpace(*update.Schedule)
		if schedule != "" {
			parser := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
			if _, err := parser.Parse(schedule); err != nil {
				return nil, fmt.Errorf("invalid schedule %q: %w", schedule, err)
			}
			b.Schedule = schedule
			scheduleChanged = true
		}
	}
	for _, tier := range []struct {
		value  *int
		target *int
		name   string
	}{
		{update.RetentionDaily, &b.RetentionDaily, "retention_daily"},
		{update.RetentionWeekly, &b.RetentionWeekly, "retention_weekly"},
		{update.RetentionMonthly, &b.RetentionMonthly, "retention_monthly"},
	} {
		if tier.value != nil {
			if *tier.value < 0 {
				return nil, fmt.Errorf("backup.%s must be >= 0 (got %d)", tier.name, *tier.value)
			}
			*tier.target = *tier.value
		}
	}

	if scheduleChanged {
		// Stop the old runner, then re-arm with the new window. The stop
		// func is idempotent; StartScheduler replaces s.cron.
		s.cronMu.Lock()
		if s.cron != nil {
			stopCtx := s.cron.Stop()
			<-stopCtx.Done()
			s.cron = nil
		}
		s.cronMu.Unlock()
		s.StartScheduler(ctx)
	}

	s.audit(ctx, types.AuditActionSystemSettingChanged, types.AuditOutcomeSuccess, "backup", "config",
		map[string]any{"schedule": b.Schedule, "retention_daily": b.RetentionDaily,
			"retention_weekly": b.RetentionWeekly, "retention_monthly": b.RetentionMonthly})
	return s.GetConfig(ctx)
}

// ---- helpers ----------------------------------------------------------

// readJSONL loads, decrypts, gunzips and parses a metadata blob.
func (s *Service) readJSONL(
	ctx context.Context, store BackupStorage, record *types.BackupRecord, obj *types.BackupManifestObject,
) ([]*jsonlRecord, error) {
	if obj == nil {
		return nil, errors.New("no metadata object")
	}
	rc, err := store.Get(ctx, path.Join(record.BasePath, obj.File))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", obj.File, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", obj.File, err)
	}
	if obj.SHA256 != "" {
		if digest := sha256Bytes(data); digest != obj.SHA256 {
			return nil, fmt.Errorf("%s SHA-256 mismatch — snapshot damaged", obj.File)
		}
	}
	if len(data) > len(encryptionMagic) && string(data[:len(encryptionMagic)]) == encryptionMagic {
		if s.masterKey == nil {
			return nil, errors.New("snapshot is encrypted but SYSTEM_AES_KEY is unavailable")
		}
		if data, err = decryptBlob(s.masterKey, data); err != nil {
			return nil, err
		}
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("gunzip %s: %w", obj.File, err)
	}
	defer zr.Close()

	var out []*jsonlRecord
	dec := json.NewDecoder(zr)
	for {
		var rec jsonlRecord
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode %s: %w", obj.File, err)
		}
		out = append(out, &rec)
	}
	return out, nil
}

// exportRowsQuery is exportRows with a custom WHERE (users membership).
func exportRowsQuery(
	ctx context.Context, db *gorm.DB, spec *tableSpec, query string, args []any, enc *json.Encoder,
) (int64, error) {
	rows, err := db.WithContext(ctx).Raw(query, args...).Rows()
	if err != nil {
		return 0, fmt.Errorf("select %s: %w", spec.Name, err)
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	var count int64
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return count, fmt.Errorf("scan %s: %w", spec.Name, err)
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			if b, ok := values[i].([]byte); ok {
				row[c] = string(b)
			} else {
				row[c] = values[i]
			}
		}
		if err := enc.Encode(jsonlRecord{Table: spec.Name, Row: row}); err != nil {
			return count, fmt.Errorf("encode %s row: %w", spec.Name, err)
		}
		count++
	}
	return count, rows.Err()
}

// msgSpecExists reports whether a table exists (best-effort column probe).
func msgSpecExists(db *gorm.DB, table string) bool {
	var one int
	if err := db.Raw(fmt.Sprintf(`SELECT 1 FROM %s LIMIT 1`, quoteIdent(db.Dialector.Name(), table))).
		Scan(&one).Error; err != nil {
		return false
	}
	return true
}

func specByName(specs []*tableSpec, name string) *tableSpec {
	for _, s := range specs {
		if s.Name == name {
			return s
		}
	}
	return nil
}

func statsOf(r *types.BackupRecord) types.BackupStats {
	var stats types.BackupStats
	_ = json.Unmarshal([]byte(r.Stats), &stats)
	return stats
}

func jobReport(job *types.BackupRestoreJob) types.RestoreReport {
	var report types.RestoreReport
	_ = json.Unmarshal([]byte(job.Report), &report)
	return report
}

func sumFileBytes(list *types.BackupFileList) int64 {
	var sum int64
	for _, e := range list.Entries {
		if e != nil {
			sum += e.Bytes
		}
	}
	return sum
}

func fileListSkipped(list *types.BackupFileList) []string {
	var out []string
	for _, e := range list.Entries {
		if e != nil && e.SHA256 == "" {
			out = append(out, e.Path)
		}
	}
	return out
}

// setPhase persists a job phase transition.
func setPhase(ctx context.Context, repo interfaces.BackupRepository, job *types.BackupRestoreJob, phase, message string) {
	job.Status = phase
	setProgressMessage(job, message)
	if err := repo.UpdateRestoreJob(ctx, job); err != nil {
		logger.Warnf(ctx, "[Backup] persist restore phase %s: %v", phase, err)
	}
}

func setProgressMessage(job *types.BackupRestoreJob, message string) {
	var p types.RestoreProgress
	_ = json.Unmarshal([]byte(job.Progress), &p)
	p.Phase = job.Status
	p.Message = message
	job.Progress = mustMarshalJSON(p)
}

// countingWriter tracks bytes written to the export stream.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// instanceVersion reports the build version for the manifest header.
func instanceVersion() string {
	if v := os.Getenv("APP_VERSION"); v != "" {
		return v
	}
	return "dev"
}

// mustMarshalJSON never fails for these payload shapes.
func mustMarshalJSON(v any) types.JSON {
	data, err := json.Marshal(v)
	if err != nil {
		return types.JSON("{}")
	}
	return types.JSON(data)
}

// audit writes a platform-scope audit row (best-effort).
func (s *Service) audit(
	ctx context.Context, action types.AuditAction, outcome types.AuditOutcome,
	scopeType, scopeID string, details map[string]any,
) {
	if s.auditSvc == nil {
		return
	}
	entry := &types.AuditLog{
		TenantID:   0,
		Action:     action,
		ScopeType:  scopeType,
		ScopeID:    scopeID,
		Outcome:    outcome,
		Details:    mustMarshalJSON(details),
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.auditSvc.Log(ctx, entry); err != nil {
		logger.Warnf(ctx, "[Backup] audit write failed for %s: %v", action, err)
	}
}
