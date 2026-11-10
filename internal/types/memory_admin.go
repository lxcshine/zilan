package types

import (
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// FR-09 记忆管理控制台（管理员侧）
//
// 控制台只输出**聚合计数**：工作区记忆总量、按状态 / 按类型的分布、按用户维度的
// 规模。这里的结构体刻意不含任何承载记忆正文的字段——管理员侧的可见面到计数为止，
// 记忆内容仍只能由归属用户通过 /memory/facts 查看（PRD §5.1 权限矩阵）。
// 跨工作区聚合一律不提供；工作区边界由仓储层每条查询写死的 tenant_id 条件保证
// （FR-09 规则 4）。
// ---------------------------------------------------------------------------

// 控制台的分页与批量上限。
const (
	// MemoryAdminDefaultPageSize 是用户维度列表的默认页长。
	MemoryAdminDefaultPageSize = 50
	// MemoryAdminMaxPageSize 是用户维度列表的页长硬顶，防止一次拉全表。
	MemoryAdminMaxPageSize = 200
	// MemoryAdminBatchLimit 是单次批量操作影响的条目上限。不传 ID
	// （= 该成员的全部待确认项）时同样受此约束，避免一条请求删空整个工作区。
	MemoryAdminBatchLimit = 1000
)

var (
	// ErrMemoryAdminTargetNotFound 对"目标成员在本工作区没有记忆"与"目标属于
	// 其他工作区"返回同一个错误，handler 一律映射为 404。状态码不携带差异，
	// 调用方就无法借它探测其他工作区是否存在某个用户（与上游 failMemoryAdmin
	// 的防探测口径一致）。
	ErrMemoryAdminTargetNotFound = errors.New("memory admin target not found")
	// ErrMemoryAdminBadRequest 表示管理员批量请求缺少必填字段或超出单次上限。
	// 用 %w 包装具体原因，handler 用 errors.Is 映射为 400。
	ErrMemoryAdminBadRequest = errors.New("memory admin request invalid")
)

// MemoryAdminPage 钳制控制台列表的分页参数：limit 非正数回落默认页长、超过硬顶
// 收敛到硬顶，offset 负数归零。分页参数来自查询串，必须在进入 SQL 前收敛。
func MemoryAdminPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = MemoryAdminDefaultPageSize
	}
	if limit > MemoryAdminMaxPageSize {
		limit = MemoryAdminMaxPageSize
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// MemoryAdminOverview 是本工作区的记忆聚合总览（FR-09 规则 1）。
type MemoryAdminOverview struct {
	TenantID uint64 `json:"tenant_id"`
	// Total 是全部状态的记忆条目总数（不含软删除行）。
	Total int64 `json:"total"`
	// ByStatus 按 active / pending / superseded / archived（以及存量的 done）分布。
	ByStatus map[string]int64 `json:"by_status"`
	// ByCategory 按类型（profile / fact / preference / todo / feedback / soul / skill）分布。
	ByCategory map[string]int64 `json:"by_category"`
	// UserCount 是本工作区内至少有一条记忆的成员数。
	UserCount int64 `json:"user_count"`
	// WriteMode / MaxActiveItems / MaxPendingItems 是工作区级配置，
	// 供管理员判断"某用户的规模是否已经贴着上限"。
	WriteMode       string    `json:"write_mode"`
	MaxActiveItems  int       `json:"max_active_items"`
	MaxPendingItems int       `json:"max_pending_items"`
	GeneratedAt     time.Time `json:"generated_at"`
}

// MemoryUserSummary 是一个成员的记忆规模（FR-09 规则 2：按用户维度识别异常增长）。
// 列名与仓储层的聚合查询一一对应，因此同时带 gorm 列标签与 json 标签。
type MemoryUserSummary struct {
	UserID          string     `json:"user_id"           gorm:"column:user_id"`
	ItemCount       int64      `json:"item_count"        gorm:"column:item_count"`
	ActiveCount     int64      `json:"active_count"      gorm:"column:active_count"`
	PendingCount    int64      `json:"pending_count"     gorm:"column:pending_count"`
	SupersededCount int64      `json:"superseded_count"  gorm:"column:superseded_count"`
	ArchivedCount   int64      `json:"archived_count"    gorm:"column:archived_count"`
	LastActivityAt  *time.Time `json:"last_activity_at,omitempty" gorm:"column:last_activity_at"`
}

// MemoryAdminUserDetail 是单个成员的明细：规模计数 + 按类型分布 + 治理状态。
// 同样只有聚合值，没有记忆正文。
type MemoryAdminUserDetail struct {
	MemoryUserSummary
	ByCategory      map[string]int64 `json:"by_category"`
	WriteMode       string           `json:"write_mode"`
	MaxActiveItems  int              `json:"max_active_items"`
	MaxPendingItems int              `json:"max_pending_items"`
	// ExtractionPaused / BlockedWrites 来自 FR-04 / FR-01 的治理状态：
	// 自动抽取是否因归档连续失败被暂停、累计被清空围栏拦截的写入数。
	ExtractionPaused bool  `json:"extraction_paused"`
	BlockedWrites    int64 `json:"blocked_writes"`
}

// MemoryAdminBatchRequest 是管理员批量归档 / 批量删除待确认项的请求体。
type MemoryAdminBatchRequest struct {
	// UserID 是被操作成员，必填：管理员批量操作必须指名目标，
	// 不接受"作用于调用者自己"的隐式语义。
	UserID string `json:"user_id" binding:"required"`
	// IDs 为空表示该成员的全部待确认项（仅批量删除待确认项支持这一语义）。
	IDs []string `json:"ids"`
}

// MemoryAdminBatchResponse 回报一次管理员批量操作的影响范围。
// PRD §5.2 要求审计记录"影响条目数"，这里把同一份数字直接回给调用方，
// 使前端展示与审计留痕同源。
type MemoryAdminBatchResponse struct {
	// Action 取 MemoryBatchActionArchive / MemoryBatchActionDelete。
	Action string `json:"action"`
	UserID string `json:"user_id"`
	// Requested 是本次请求意图影响的条目数。
	Requested int `json:"requested"`
	// Affected 是数据库实际变更的行数。
	Affected int64 `json:"affected"`
	// Skipped 仅由"批量删除待确认项"填充：请求里状态不是 pending 的条目 ID。
	// 这些条目被拒绝而非静默忽略，管理员据此知道 active 记忆没有被动过
	// （FR-02 规则 6：待确认裁剪绝不触及 active）。
	Skipped []string `json:"skipped,omitempty"`
}
