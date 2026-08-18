package types

import (
	"time"

	"gorm.io/gorm"
)

// ParseReviewItem represents a document that failed parsing twice and
// entered the human review queue (§5.4).
type ParseReviewItem struct {
	ID              string         `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID        uint64         `json:"tenant_id" gorm:"index"`
	KnowledgeID     string         `json:"knowledge_id" gorm:"type:varchar(36);index"`
	KnowledgeBaseID string         `json:"knowledge_base_id" gorm:"type:varchar(36);index"`
	FileName        string         `json:"file_name"`
	FileType        string         `json:"file_type"`
	FileSize        int64          `json:"file_size"`
	EngineUsed      string         `json:"engine_used"`
	FallbackEngine  string         `json:"fallback_engine,omitempty"`
	QualityScore    float64        `json:"quality_score"`
	GarbleRate      float64        `json:"garble_rate"`
	EmptyPageRate   float64        `json:"empty_page_rate"`
	TableDamageRate float64        `json:"table_damage_rate"`
	ImageLossRate   float64        `json:"image_loss_rate"`
	RetryReason     string         `json:"retry_reason"`
	DocType         string         `json:"doc_type,omitempty"`
	Status          string         `json:"status" gorm:"default:pending"` // pending, reviewing, resolved, ignored
	Resolution      string         `json:"resolution,omitempty"`          // reparse_with_engine, manual_fix, discard
	ReviewerID      string         `json:"reviewer_id,omitempty"`
	ReviewedAt      *time.Time     `json:"reviewed_at,omitempty"`
	CreatedAt       time.Time      `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt       time.Time      `json:"updated_at" gorm:"autoUpdateTime"`
	DeletedAt       gorm.DeletedAt `json:"-" gorm:"index"`
}

// TableName returns the table name for ParseReviewItem.
func (ParseReviewItem) TableName() string {
	return "parse_review_items"
}

// ParseReviewStatus constants
const (
	ParseReviewStatusPending   = "pending"
	ParseReviewStatusReviewing = "reviewing"
	ParseReviewStatusResolved  = "resolved"
	ParseReviewStatusIgnored   = "ignored"
)

// ParseReviewResolution constants
const (
	ParseReviewResolutionReparse = "reparse_with_engine"
	ParseReviewResolutionManual  = "manual_fix"
	ParseReviewResolutionDiscard = "discard"
)
