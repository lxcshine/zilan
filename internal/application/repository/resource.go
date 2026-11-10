package repository

import (
	"context"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type resourceRepository struct{ db *gorm.DB }

// NewResourceRepository creates the persistence adapter for resource metadata.
func NewResourceRepository(db *gorm.DB) interfaces.ResourceRepository {
	return &resourceRepository{db: db}
}

func (r *resourceRepository) Create(ctx context.Context, resource *types.StoredResource) error {
	return r.db.WithContext(ctx).Create(resource).Error
}

func (r *resourceRepository) GetByID(ctx context.Context, id string) (*types.StoredResource, error) {
	var resource types.StoredResource
	err := r.db.WithContext(ctx).Where("id = ? AND state = ?", id, types.ResourceStateActive).First(&resource).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &resource, err
}

func (r *resourceRepository) GetByHandle(ctx context.Context, handle string) (*types.StoredResource, error) {
	var resource types.StoredResource
	err := r.db.WithContext(ctx).
		Where("handle = ? AND state = ?", handle, types.ResourceStateActive).
		First(&resource).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &resource, err
}

func (r *resourceRepository) GetByTenantLocation(
	ctx context.Context,
	tenantID uint64,
	locationHash string,
) (*types.StoredResource, error) {
	var resource types.StoredResource
	err := r.db.WithContext(ctx).
		Where(
			"tenant_id = ? AND location_hash = ? AND state = ?",
			tenantID,
			locationHash,
			types.ResourceStateActive,
		).
		First(&resource).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &resource, err
}

// GetActiveByContentHash resolves identical bytes already registered under any
// physical location（FR-18 规则 5：相同图片字节的重复出现复用同一资源句柄）。
// Only active resources are eligible — one queued for deletion may be removed
// at any moment — and, when the history contains duplicates, the oldest one
// wins so two identical lookups always resolve to the same handle.
func (r *resourceRepository) GetActiveByContentHash(
	ctx context.Context,
	tenantID uint64,
	contentHash string,
) (*types.StoredResource, error) {
	if contentHash == "" {
		return nil, nil
	}
	var resource types.StoredResource
	err := r.db.WithContext(ctx).
		Where(
			"tenant_id = ? AND content_hash = ? AND state = ?",
			tenantID,
			contentHash,
			types.ResourceStateActive,
		).
		Order("created_at ASC").
		First(&resource).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// A temporary resource is scratch storage that its owner may delete at
	// any time; aliasing a long-lived reference onto it would let that
	// cleanup remove bytes the reusing caller still serves.
	if resource.Lifecycle == types.ResourceLifecycleTemporary {
		return nil, nil
	}
	return &resource, nil
}

func (r *resourceRepository) MarkDeleted(ctx context.Context, id string) error {
	return r.db.WithContext(ctx).Model(&types.StoredResource{}).Where("id = ?", id).
		Updates(map[string]interface{}{"state": types.ResourceStateDeleted, "deleted_at": time.Now()}).Error
}

func (r *resourceRepository) CreateBinding(ctx context.Context, binding *types.ResourceBinding) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(binding).Error
}

func (r *resourceRepository) CreateGrant(ctx context.Context, grant *types.ResourceAccessGrant) error {
	return r.db.WithContext(ctx).Create(grant).Error
}

func (r *resourceRepository) GetValidGrant(
	ctx context.Context,
	tokenHash string,
	now time.Time,
) (*types.ResourceAccessGrant, error) {
	var grant types.ResourceAccessGrant
	err := r.db.WithContext(ctx).
		Where("token_hash = ? AND revoked_at IS NULL AND expires_at > ?", tokenHash, now).
		First(&grant).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &grant, err
}

func (r *resourceRepository) DeleteExpiredGrants(ctx context.Context, before time.Time) error {
	return r.db.WithContext(ctx).
		Where("expires_at <= ? OR revoked_at IS NOT NULL", before).
		Delete(&types.ResourceAccessGrant{}).Error
}
