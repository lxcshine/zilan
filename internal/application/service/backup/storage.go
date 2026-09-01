// Package backup implements the tiered user-data backup & recovery
// subsystem described in docs/prd/data-backup-recovery.md:
//
//   - metadata tier: full-instance + per-workspace jsonl.gz logical export
//     (works uniformly on PostgreSQL and SQLite — no external dump binary)
//   - file tier:     per-workspace object copy from the primary storage
//     into an independently configured backup store
//   - index tier:    never backed up; rebuilt via the existing reparse
//     pipeline after restore
//
// Snapshots are integrity-tracked by a SHA-256 manifest, optionally
// AES-256-GCM envelope-encrypted, and pruned by a simplified GFS
// retention policy.
package backup

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// BackupStorage is the write target for snapshots. Paths are
// forward-slash-relative object keys rooted at the configured prefix;
// implementations must be safe for concurrent Put calls on distinct keys.
type BackupStorage interface {
	// Put streams an object to the given relative path, creating parent
	// directories / implicit prefixes as needed.
	Put(ctx context.Context, relPath string, r io.Reader) error
	// Get opens an object for reading.
	Get(ctx context.Context, relPath string) (io.ReadCloser, error)
	// Exists checks object presence.
	Exists(ctx context.Context, relPath string) (bool, error)
	// List returns the relative paths of all objects under prefix
	// (non-recursive semantics are not needed — full listing).
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete removes one object. Missing objects are not an error.
	Delete(ctx context.Context, relPath string) error
	// Describe returns a human-readable target description for logs.
	Describe() string
}

// NewBackupStorage builds the target store from the config section.
// Supported providers: local (a directory) and minio/s3 (any
// S3-compatible endpoint).
func NewBackupStorage(cfg *config.BackupStorageConfig) (BackupStorage, error) {
	if cfg == nil {
		return nil, fmt.Errorf("backup storage config is nil")
	}
	switch strings.ToLower(cfg.Provider) {
	case "local":
		base := strings.TrimSpace(cfg.LocalPath)
		if base == "" {
			return nil, fmt.Errorf("backup.storage.local_path is empty")
		}
		if err := os.MkdirAll(base, 0o755); err != nil {
			return nil, fmt.Errorf("create backup dir %s: %w", base, err)
		}
		return &localBackupStorage{base: base}, nil

	case "minio", "s3":
		endpoint := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(cfg.Endpoint), "https://"), "http://")
		if endpoint == "" || strings.TrimSpace(cfg.AccessKey) == "" ||
			strings.TrimSpace(cfg.SecretKey) == "" || strings.TrimSpace(cfg.Bucket) == "" {
			return nil, fmt.Errorf("backup.storage endpoint/access_key/secret_key/bucket are required for provider=%s", cfg.Provider)
		}
		useSSL := true
		if cfg.UseSSL != nil {
			useSSL = *cfg.UseSSL
		} else if strings.HasPrefix(strings.TrimSpace(cfg.Endpoint), "http://") {
			useSSL = false
		}
		client, err := minio.New(endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
			Secure: useSSL,
		})
		if err != nil {
			return nil, fmt.Errorf("init backup storage client: %w", err)
		}
		prefix := strings.Trim(strings.TrimSpace(cfg.PathPrefix), "/")
		if prefix == "" {
			prefix = "backups"
		}
		return &s3BackupStorage{
			client: client,
			bucket: cfg.Bucket,
			prefix: prefix,
		}, nil

	default:
		return nil, fmt.Errorf("unsupported backup storage provider %q (use local, minio, or s3)", cfg.Provider)
	}
}

// ---- local filesystem target ----

type localBackupStorage struct {
	base string
}

func (s *localBackupStorage) resolve(rel string) (string, error) {
	clean := path.Clean("/" + strings.TrimPrefix(rel, "/"))
	full := filepath.Join(s.base, filepath.FromSlash(clean))
	// Defense against path traversal in snapshot ids.
	if !strings.HasPrefix(full, filepath.Clean(s.base)+string(os.PathSeparator)) && full != filepath.Clean(s.base) {
		return "", fmt.Errorf("backup path escapes base dir: %s", rel)
	}
	return full, nil
}

func (s *localBackupStorage) Put(_ context.Context, rel string, r io.Reader) error {
	full, err := s.resolve(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(full), err)
	}
	tmp := full + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	return os.Rename(tmp, full)
}

func (s *localBackupStorage) Get(_ context.Context, rel string) (io.ReadCloser, error) {
	full, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	return os.Open(full)
}

func (s *localBackupStorage) Exists(_ context.Context, rel string) (bool, error) {
	full, err := s.resolve(rel)
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(full)
	if os.IsNotExist(statErr) {
		return false, nil
	}
	return statErr == nil, statErr
}

func (s *localBackupStorage) List(_ context.Context, prefix string) ([]string, error) {
	root, err := s.resolve(prefix)
	if err != nil {
		return nil, err
	}
	var out []string
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(s.base, p)
		if relErr != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	return out, err
}

func (s *localBackupStorage) Delete(_ context.Context, rel string) error {
	full, err := s.resolve(rel)
	if err != nil {
		return err
	}
	if rmErr := os.Remove(full); rmErr != nil && !os.IsNotExist(rmErr) {
		return rmErr
	}
	return nil
}

func (s *localBackupStorage) Describe() string {
	return "local:" + s.base
}

// ---- S3-compatible target (MinIO / AWS S3 / any S3 endpoint) ----

type s3BackupStorage struct {
	client *minio.Client
	bucket string
	prefix string
}

func (s *s3BackupStorage) key(rel string) string {
	return s.prefix + "/" + strings.TrimPrefix(path.Clean("/"+rel), "/")
}

func (s *s3BackupStorage) Put(ctx context.Context, rel string, r io.Reader) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.key(rel), r, -1,
		minio.PutObjectOptions{PartSize: 32 << 20})
	if err != nil {
		return fmt.Errorf("put s3://%s/%s: %w", s.bucket, s.key(rel), err)
	}
	return nil
}

func (s *s3BackupStorage) Get(ctx context.Context, rel string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.key(rel), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get s3://%s/%s: %w", s.bucket, s.key(rel), err)
	}
	return obj, nil
}

func (s *s3BackupStorage) Exists(ctx context.Context, rel string) (bool, error) {
	_, err := s.client.StatObject(ctx, s.bucket, s.key(rel), minio.StatObjectOptions{})
	if err != nil {
		resp := minio.ToErrorResponse(err)
		if resp.Code == "NoSuchKey" {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *s3BackupStorage) List(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	prefixKey := s.key(prefix)
	if !strings.HasSuffix(prefixKey, "/") {
		prefixKey += "/"
	}
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefixKey,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		rel := strings.TrimPrefix(obj.Key, s.prefix+"/")
		out = append(out, rel)
	}
	return out, nil
}

func (s *s3BackupStorage) Delete(ctx context.Context, rel string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.key(rel), minio.RemoveObjectOptions{})
}

func (s *s3BackupStorage) Describe() string {
	return fmt.Sprintf("s3://%s/%s", s.bucket, s.prefix)
}
