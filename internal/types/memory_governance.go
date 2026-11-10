package types

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 记忆治理（PRD §3.2 状态机 + FR-01 清空围栏 / FR-02 确认门控 /
// FR-03 取代链 / FR-04 容量治理）
// ---------------------------------------------------------------------------

// 记忆来源三分类（FR-02 规则 1）。
const (
	// MemoryOriginExplicit：用户在对话中明确要求记住。
	MemoryOriginExplicit = "explicit"
	// MemoryOriginExtracted：后台抽取任务从对话中推断（系统推断）。
	MemoryOriginExtracted = "extracted"
	// MemoryOriginManual：记忆管理页创建或编辑。
	MemoryOriginManual = "manual"
)

// MemoryStatusForOrigin 按 PRD §3.2 状态转移表决定新记忆的初始状态：
// explicit / manual 直接 active；extracted 属系统推断，一律进 pending，
// 由用户确认后才生效（pending 永不注入 prompt）。
func MemoryStatusForOrigin(origin string) string {
	switch origin {
	case MemoryOriginExplicit, MemoryOriginManual:
		return MemoryStatusActive
	default:
		return MemoryStatusPending
	}
}

// 记忆写入模式（FR-02 规则 7）：仅显式模式不发起后台抽取，因此不产生 pending 项。
const (
	MemoryWriteExplicitOnly = "explicit_only"
	MemoryWriteAuto         = "auto"
)

// 容量与判定阈值默认值。
const (
	// DefaultMemoryMaxActiveItems 是每个记忆主体（租户 + 用户）active 条目上限（FR-04 规则 1）。
	DefaultMemoryMaxActiveItems = 200
	// DefaultMemoryMaxPendingItems 是待确认条目独立上限（FR-02 规则 6 / FR-04 规则 3）。
	DefaultMemoryMaxPendingItems = 50
	// MemoryMaxItemsHardCap 是工作区可配置上限的硬顶，与 MaxFactsPerUser 一致，
	// 保证召回侧的暴力余弦扫描成本有界。
	MemoryMaxItemsHardCap = 2000
	// MemoryArchiveFailureThreshold 是归档连续失败阈值：达到后暂停自动抽取并通知管理员
	// （FR-04 异常）。
	MemoryArchiveFailureThreshold = 3
	// MemoryConflictSimilarityHigh 之上判定为确定矛盾，直接走取代链（FR-03 规则 1）。
	MemoryConflictSimilarityHigh = 0.90
	// MemoryConflictSimilarityLow 与 High 之间是阈值边界：不自动取代，改由用户裁决
	// （FR-03 异常）。
	MemoryConflictSimilarityLow = 0.75
)

// 记忆治理哨兵错误。
var (
	// ErrMemoryWriteFenced 表示写入被清空时序围栏拦截。按 FR-01 规则 5，
	// 该错误在服务层被静默吞掉（只计数），不向用户暴露。
	ErrMemoryWriteFenced = errors.New("memory write blocked by clear fence")
	// ErrMemoryConflict 表示记忆状态机拒绝该转移（例如确认一条已不是 pending 的记忆，
	// 或恢复一个已被取代的目标）。
	ErrMemoryConflict = errors.New("memory state conflict")
	// ErrMemoryForbidden 表示调用者无权对目标记忆执行该操作（PRD §5.1：
	// 确认/拒绝仅记忆归属用户本人；管理员侧操作需工作区管理员及以上角色）。
	ErrMemoryForbidden = errors.New("memory operation forbidden")
)

// MemoryGovernance 是每个记忆主体（租户 + 用户）一行的治理状态，承载 FR-01 的
// 清空时序围栏与 FR-04 的归档失败计数。
//
// 围栏采用"世代令牌 + 时间截止点"双机制（与上游 WeKnora v0.9.0 一致）：
//   - Generation：每次清空轮换的 UUID。后台抽取任务入队时捕获当时的世代，落库前
//     比对；不符说明期间发生过清空，任务携带的推断已失效，直接丢弃。
//   - CutoffNano：清空截止点，单调不回退（逻辑递增而非纯墙钟，可抗时钟回拨）。
//     写入所依据的对话时间早于该点即被拦截。
//
// 时间比较一律用 Unix 纳秒整数在 Go 侧完成：整数比较既不受调用方时区影响，
// 也不受数据库时间列精度（SQLite 的 DATETIME 可能截断亚秒）影响——这两点正是
// "截止点单调不回退"和"跨时区收敛到同一序列"的落地方式。
type MemoryGovernance struct {
	TenantID uint64 `json:"tenant_id" gorm:"primaryKey"`
	UserID   string `json:"user_id"   gorm:"type:varchar(512);primaryKey"`
	// Generation 是当前生效的围栏世代令牌。
	Generation string `json:"generation" gorm:"type:varchar(36)"`
	// CutoffNano 是清空截止点的 Unix 纳秒值；0 表示该主体从未清空过。
	CutoffNano int64 `json:"cutoff_nano"`
	// BlockedWrites 累计被围栏拦截的写入数（FR-01 结果：进入诊断指标）。
	BlockedWrites int64 `json:"blocked_writes"`
	// ArchiveFailures 是容量归档的连续失败次数，成功后清零。
	ArchiveFailures int `json:"archive_failures"`
	// ExtractionPausedAt 非 nil 表示自动抽取已因归档连续失败被暂停（FR-04 异常）。
	ExtractionPausedAt *time.Time `json:"extraction_paused_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName 固定表名。
func (MemoryGovernance) TableName() string { return "memory_governance" }

// CutoffTime 返回截止点的 UTC 时间；从未清空过返回零值。
func (g *MemoryGovernance) CutoffTime() time.Time {
	if g == nil || g.CutoffNano == 0 {
		return time.Time{}
	}
	return time.Unix(0, g.CutoffNano).UTC()
}

// ExtractionPaused 报告自动抽取是否处于暂停状态。
func (g *MemoryGovernance) ExtractionPaused() bool {
	return g != nil && g.ExtractionPausedAt != nil
}

// MemoryClearResult 是一次"清空全部记忆"的结果，用于响应与审计日志
// （PRD §5.2 要求记录围栏拦截数）。
type MemoryClearResult struct {
	DeletedFacts     int64     `json:"deleted_facts"`
	DeletedSummaries int64     `json:"deleted_summaries"`
	Generation       string    `json:"generation"`
	CutoffAt         time.Time `json:"cutoff_at"`
	// BlockedWrites 是清空时点上已累计的围栏拦截数（诊断指标）。
	BlockedWrites int64 `json:"blocked_writes"`
}

// Deleted 返回本次清空移除的记忆总行数。
func (r *MemoryClearResult) Deleted() int64 {
	if r == nil {
		return 0
	}
	return r.DeletedFacts + r.DeletedSummaries
}

// 治理事件类型（FR-04 规则 5 的一次性提示 + 异常通知）。
const (
	// MemoryEventCapacityArchived：active 超限自动归档。
	MemoryEventCapacityArchived = "capacity_archived"
	// MemoryEventPendingTrimmed：待确认容量超限归档最旧 pending。
	MemoryEventPendingTrimmed = "pending_trimmed"
	// MemoryEventArchiveFailed：归档失败，已记录待重试。
	MemoryEventArchiveFailed = "archive_failed"
	// MemoryEventExtractionPaused：归档连续失败达阈值，已暂停自动抽取。
	MemoryEventExtractionPaused = "extraction_paused"
)

// MemoryGovernanceEvent 是一条记忆治理事件。自动归档发生时写入一行，
// 记忆管理页通过 GET /memory/notices 取未确认事件做"一次性提示"，
// 用户确认后写 AcknowledgedAt，不再重复提示。
type MemoryGovernanceEvent struct {
	ID       int64  `json:"id"       gorm:"primaryKey;autoIncrement"`
	TenantID uint64 `json:"tenant_id" gorm:"index"`
	UserID   string `json:"user_id"   gorm:"type:varchar(512)"`
	// Kind 取 MemoryEvent* 常量。
	Kind   string `json:"kind"   gorm:"type:varchar(32)"`
	Reason string `json:"reason"`
	// ItemCount / ItemIDs / ItemContents 说明影响了哪些条目。
	ItemCount    int         `json:"item_count"`
	ItemIDs      StringArray `json:"item_ids,omitempty"      gorm:"type:jsonb"`
	ItemContents StringArray `json:"item_contents,omitempty" gorm:"type:jsonb"`

	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// TableName 固定表名。
func (MemoryGovernanceEvent) TableName() string { return "memory_governance_events" }

// 批量操作动作（FR-02 规则 5 批量确认 / PRD §5.1 批量归档、删除）。
const (
	MemoryBatchActionConfirm = "confirm"
	MemoryBatchActionReject  = "reject"
	MemoryBatchActionArchive = "archive"
	MemoryBatchActionDelete  = "delete"
)

// MemoryBatchResult 是一次批量操作的结果，用于响应与审计日志。
type MemoryBatchResult struct {
	Action    string   `json:"action"`
	Requested int      `json:"requested"`
	Affected  int64    `json:"affected"`
	Failed    []string `json:"failed,omitempty"`
}

// MemoryStatusInfo 是记忆管理页入口的状态摘要：待确认角标（FR-02 结果）、
// 各状态计数、生效的容量上限与写入模式，以及围栏拦截数（FR-01 诊断指标）。
type MemoryStatusInfo struct {
	Enabled          bool   `json:"enabled"`
	WriteMode        string `json:"write_mode"`
	FactCount        int64  `json:"fact_count"` // active 条数，兼容既有前端字段
	ActiveCount      int64  `json:"active_count"`
	PendingCount     int64  `json:"pending_count"`
	SupersededCount  int64  `json:"superseded_count"`
	ArchivedCount    int64  `json:"archived_count"`
	MaxActiveItems   int    `json:"max_active_items"`
	MaxPendingItems  int    `json:"max_pending_items"`
	ExtractionPaused bool   `json:"extraction_paused"`
	BlockedWrites    int64  `json:"blocked_writes"`
}

// MemoryFactRank 是容量治理排名所需的最小投影（不含 embedding，避免为排名
// 拉回大字段）。
type MemoryFactRank struct {
	ID             string     `gorm:"column:id"`
	Category       string     `gorm:"column:category"`
	Content        string     `gorm:"column:content"`
	Importance     float64    `gorm:"column:importance"`
	Confidence     float64    `gorm:"column:confidence"`
	AccessCount    int        `gorm:"column:access_count"`
	LastAccessedAt *time.Time `gorm:"column:last_accessed_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	// Rank 由 MemoryRetentionRank 在 Go 侧算出，不落库。
	Rank float64 `gorm:"-" json:"rank"`
}

// ReferenceTime 返回排名用的参考时间：最近命中时间、更新时间、创建时间中最新者，
// 与召回侧 reference 取 UpdatedAt / LastAccessedAt 较晚者的口径一致。
func (r *MemoryFactRank) ReferenceTime() time.Time {
	reference := r.UpdatedAt
	if r.CreatedAt.After(reference) {
		reference = r.CreatedAt
	}
	if r.LastAccessedAt != nil && r.LastAccessedAt.After(reference) {
		reference = *r.LastAccessedAt
	}
	return reference
}

// MemoryRetentionRank 计算容量治理排名（FR-04 规则 2），分数越低越先被归档。
//
// 采用的排名规则与理由：
//
//	rank = timeDecay(reference, DefaultFactHalfLife) × (1 + ln(1+accessCount)) × (0.5 + importance) × (0.5 + confidence)
//
// 召回评分是 `语义相似度 × 时间衰减 × (1+ln(1+访问次数))`（另加 0.1×importance 先验）。
// 容量治理发生在没有查询的时刻，语义相似度不可知，因此保留召回公式中与查询无关的
// 两个因子（时间衰减、访问频次），并用 importance / confidence 作为查询无关先验补足
// ——PRD FR-04 规则 2 也正是要求排名综合"命中次数、最近命中时间、重要度、置信度"。
// reference 取 max(last_accessed_at, updated_at, created_at)，与召回侧一致，
// 于是"召回认为重要"的条目在容量侧同样排名靠前，不会出现两套口径互相矛盾。
func MemoryRetentionRank(importance, confidence float64, accessCount int, reference, now time.Time) float64 {
	// 语义项取 1（满相关），只保留衰减与频次两个查询无关因子。
	base := MemoryRecallScore(1, reference, accessCount, DefaultFactHalfLife, now)
	if importance < 0 {
		importance = 0
	}
	if importance > 1 {
		importance = 1
	}
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}
	return base * (0.5 + importance) * (0.5 + confidence)
}

// ComputeNormalizedKey 派生矛盾判定键（FR-03）：sha256(category|subject|predicate)
// 截断为 32 位十六进制。
//
// 为什么不复用 triple_hash：triple_hash 覆盖 (category|subject|predicate|object)，
// 四元组完全相同属于"同一条记忆再次被抽取"，走 upsert 去重而不是矛盾；矛盾是
// 同一主体的同一属性出现了不同的值（"生产库用 MySQL" vs "生产库用 PostgreSQL"），
// 因此键必须去掉 object。subject 与 predicate 同时为空时返回空串——此时没有可比较
// 的属性维度，宁可不判定矛盾，也不能让同类目下所有无主语记忆互相取代。
func ComputeNormalizedKey(category, subject, predicate string) string {
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	subject, predicate = norm(subject), norm(predicate)
	if subject == "" && predicate == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(norm(category) + "|" + subject + "|" + predicate))
	return hex.EncodeToString(sum[:16])
}

// ---------------------------------------------------------------------------
// 清空时序围栏的写入上下文（FR-01）
// ---------------------------------------------------------------------------

// memoryWriteFenceKey 是围栏在 context 中的私有键。
type memoryWriteFenceKey struct{}

// MemoryWriteFence 描述一次记忆写入所依据的对话时间与（后台任务）世代令牌。
// 由服务层在写入前放进 context，仓储层的每一个写路径统一校验（唯一收口），
// 因此新增写入路径不会漏掉围栏。
type MemoryWriteFence struct {
	// ConversationAt 是本次写入所依据的对话发生时间；零值表示"就是当下"。
	ConversationAt time.Time
	// Generation 是任务入队时捕获的围栏世代；空串表示不做世代校验（同步写入路径）。
	Generation string
}

// WithMemoryWriteFence 把围栏信息挂到 context 上。
func WithMemoryWriteFence(ctx context.Context, fence MemoryWriteFence) context.Context {
	return context.WithValue(ctx, memoryWriteFenceKey{}, fence)
}

// MemoryWriteFenceFromContext 取回围栏信息；未设置时返回零值与 false。
func MemoryWriteFenceFromContext(ctx context.Context) (MemoryWriteFence, bool) {
	fence, ok := ctx.Value(memoryWriteFenceKey{}).(MemoryWriteFence)
	return fence, ok
}

// ---------------------------------------------------------------------------
// 工作区级记忆配置（FR-02 规则 7 / FR-04 规则 1：默认 200，可按工作区配置）
// ---------------------------------------------------------------------------

// TenantMemoryConfig 是工作区级记忆治理配置，存于 tenants.memory_config(jsonb)。
// 零值字段一律回落到全局默认，因此存量工作区不需要数据迁移（PRD §5.4）。
type TenantMemoryConfig struct {
	// WriteMode 取 MemoryWriteExplicitOnly（仅显式）或 MemoryWriteAuto（自动）。
	WriteMode string `json:"write_mode"`
	// MaxActiveItems 是 active 条目上限，0 表示用默认 200。
	MaxActiveItems int `json:"max_active_items"`
	// MaxPendingItems 是待确认条目上限，0 表示用默认 50。
	MaxPendingItems int `json:"max_pending_items"`
}

// Value 实现 driver.Valuer。
func (c TenantMemoryConfig) Value() (driver.Value, error) {
	return json.Marshal(c)
}

// Scan 实现 sql.Scanner。
func (c *TenantMemoryConfig) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	var b []byte
	switch v := value.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return nil
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, c)
}

// EffectiveWriteMode 返回生效的写入模式，兼容 nil 与非法值。
func (c *TenantMemoryConfig) EffectiveWriteMode() string {
	if c == nil || c.WriteMode != MemoryWriteExplicitOnly {
		return MemoryWriteAuto
	}
	return MemoryWriteExplicitOnly
}

// AutoExtractEnabled 报告是否允许发起后台抽取（仅显式模式不抽取）。
func (c *TenantMemoryConfig) AutoExtractEnabled() bool {
	return c.EffectiveWriteMode() == MemoryWriteAuto
}

// EffectiveMaxActiveItems 返回生效的 active 上限，兼容 nil 并夹在硬顶内。
func (c *TenantMemoryConfig) EffectiveMaxActiveItems() int {
	return clampMemoryCap(c.maxActiveItemsOrNil(), DefaultMemoryMaxActiveItems)
}

// EffectiveMaxPendingItems 返回生效的待确认上限，兼容 nil 并夹在硬顶内。
func (c *TenantMemoryConfig) EffectiveMaxPendingItems() int {
	return clampMemoryCap(c.maxPendingItemsOrNil(), DefaultMemoryMaxPendingItems)
}

func (c *TenantMemoryConfig) maxActiveItemsOrNil() int {
	if c == nil {
		return 0
	}
	return c.MaxActiveItems
}

func (c *TenantMemoryConfig) maxPendingItemsOrNil() int {
	if c == nil {
		return 0
	}
	return c.MaxPendingItems
}

func clampMemoryCap(value, fallback int) int {
	if value <= 0 {
		value = fallback
	}
	if value > MemoryMaxItemsHardCap {
		value = MemoryMaxItemsHardCap
	}
	return value
}
