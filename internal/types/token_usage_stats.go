package types

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// FR-12 每轮 Token 用量归因与分层看板
//
// 这里定义三样东西：
//  1. ContextTurnStats —— 随 assistant 消息持久化的「本轮上下文归因」：五层
//     （L0–L4）实际占用与预算、弹性调整动作、压缩/截断事件；
//  2. 两条采集通道共用的载荷形状（TurnUsageData 与 Extra 键名）；
//  3. 管理员聚合看板的查询与响应结构。
//
// 口径约束（PRD FR-12 ⑤）：所有 token 数值都来自 internal/contextx 的精确
// tokenizer（Counter.Count / CountMessage）或供应商回报的 usage，绝不混入
// len(string) 之类的字符估算值。types 不能反向依赖 contextx（会形成导入环），
// 因此这里只承载已经算好的整数。
// ---------------------------------------------------------------------------

// 五层名称。与 contextx.Layer.String() 逐字一致，看板按这个顺序展示。
const (
	ContextLayerSystem    = "L0_system"
	ContextLayerMemory    = "L1_memory"
	ContextLayerRetrieval = "L2_retrieval"
	ContextLayerHistory   = "L3_history"
	ContextLayerQuery     = "L4_query"
)

// ContextTurnStats.Source 的取值：标记这份归因来自哪条链路。两条链路的分层语义
// 相同，但只有 chat pipeline 会做五层预算切分（Agent 侧 Budget 为零值）。
const (
	ContextTurnStatsSourcePipeline = "pipeline"
	ContextTurnStatsSourceAgent    = "agent"
)

// ContextTurnEvent.Kind 的取值。压缩与截断分开统计：压缩有摘要顶替，截断没有。
const (
	// ContextEventCompaction —— 一次摘要式压缩（Agent 压缩器 / 历史 Map-Reduce）。
	ContextEventCompaction = "compaction"
	// ContextEventShrink —— 分层降级（检索块按相关度收敛为摘要）。
	ContextEventShrink = "shrink"
	// ContextEventTruncation —— 硬截断 / 整条丢弃 / 规则式截断，被移除的内容没有摘要顶替。
	ContextEventTruncation = "truncation"
	// ContextEventImageDrop —— 多模态溢出降级：移除较早的图片。
	ContextEventImageDrop = "image_drop"
	// ContextEventToolTrim —— 工具结果预览化。
	ContextEventToolTrim = "tool_trim"
)

// ContextLayerTokens 是五层上下文的 token 计量。既可表示预算，也可表示实际占用。
type ContextLayerTokens struct {
	System    int `json:"system"`    // L0 系统提示与指令
	Memory    int `json:"memory"`    // L1 用户画像与长期记忆
	Retrieval int `json:"retrieval"` // L2 检索结果
	History   int `json:"history"`   // L3 会话历史
	Query     int `json:"query"`     // L4 当前查询与工具结果
}

// Total 返回五层之和。
func (l ContextLayerTokens) Total() int {
	return l.System + l.Memory + l.Retrieval + l.History + l.Query
}

// IsZero 报告五层是否全为零（未开启分层预算，或这一轮没有采集到归因）。
func (l ContextLayerTokens) IsZero() bool {
	return l.Total() == 0
}

// Add 把另一份分层计量累加进来。
func (l *ContextLayerTokens) Add(o ContextLayerTokens) {
	if l == nil {
		return
	}
	l.System += o.System
	l.Memory += o.Memory
	l.Retrieval += o.Retrieval
	l.History += o.History
	l.Query += o.Query
}

// ContextLayerUsage 是看板里一层的展示单元：名称、预算、实际占用与占比。
type ContextLayerUsage struct {
	Layer  string  `json:"layer"`
	Budget int     `json:"budget"`
	Tokens int     `json:"tokens"`
	Ratio  float64 `json:"ratio"`
}

// Entries 按 L0→L4 的固定顺序展开五层。ratio 的分母是五层实际占用之和；
// 分母为零时 ratio 一律为 0，避免看板出现 NaN。
func (l ContextLayerTokens) Entries(budget ContextLayerTokens) []ContextLayerUsage {
	total := l.Total()
	ratio := func(tokens int) float64 {
		if total <= 0 {
			return 0
		}
		return float64(tokens) / float64(total)
	}
	return []ContextLayerUsage{
		{Layer: ContextLayerSystem, Budget: budget.System, Tokens: l.System, Ratio: ratio(l.System)},
		{Layer: ContextLayerMemory, Budget: budget.Memory, Tokens: l.Memory, Ratio: ratio(l.Memory)},
		{Layer: ContextLayerRetrieval, Budget: budget.Retrieval, Tokens: l.Retrieval, Ratio: ratio(l.Retrieval)},
		{Layer: ContextLayerHistory, Budget: budget.History, Tokens: l.History, Ratio: ratio(l.History)},
		{Layer: ContextLayerQuery, Budget: budget.Query, Tokens: l.Query, Ratio: ratio(l.Query)},
	}
}

// ContextTurnEvent 是一次压缩/截断事件。
//
// 五层链路（chat pipeline）的事件由装配器的弹性调整动作解析而来，因此带 Layer；
// Agent 链路的事件由压缩器/截断器直接产出，因此带 Reason 与 Round。
type ContextTurnEvent struct {
	// Kind 见 ContextEvent* 常量。
	Kind string `json:"kind"`
	// Reason 是触发原因：threshold / overflow / final_answer（Agent 压缩器），
	// 或五层链路解析出的动作原因（如 hard_truncated）。
	Reason string `json:"reason,omitempty"`
	// Layer 是被影响的层名（L0_system…L4_query），Agent 链路通常为空。
	Layer string `json:"layer,omitempty"`
	// Round 是 Agent 轮次（从 1 开始），五层链路为 0。
	Round int `json:"round,omitempty"`
	// TokensBefore / TokensAfter 是事件前后的精确 token 数（未知则为 0）。
	TokensBefore int `json:"tokens_before,omitempty"`
	TokensAfter  int `json:"tokens_after,omitempty"`
	// FreedTokens 是本次真正回收的空间，负值表示摘要开销大于收益。
	FreedTokens int `json:"freed_tokens,omitempty"`
	// Degraded 标记降级：摘要模型失败后退回规则式归档，或常驻记忆块被整块降级。
	Degraded bool `json:"degraded,omitempty"`
	// CheckpointSaved 标记这次压缩产出了可持久化的检查点（FR-10）。
	CheckpointSaved bool `json:"checkpoint_saved,omitempty"`
	// CheckpointReused 标记这次压缩是在更早的检查点上做增量折叠，而不是从头重算。
	CheckpointReused bool `json:"checkpoint_reused,omitempty"`
	// Detail 保留原始动作文本，便于排障时逐字对照。
	Detail string `json:"detail,omitempty"`
}

// ContextTurnStats 是随 assistant 消息持久化的每轮上下文归因（PRD FR-12 ①②）。
//
// 缓存命中量不在这里重复：它已经在同一行的 Message.Usage
// （CachedTokens / CacheReadTokens / CacheWriteTokens / CacheMissTokens /
// CacheStatus）里，看板直接从 Usage 聚合，避免同一口径出现两份可漂移的副本。
type ContextTurnStats struct {
	// Source 见 ContextTurnStatsSource* 常量。
	Source string `json:"source,omitempty"`
	// Model 是本轮实际使用的对话模型名（不是 ID），用于解释 tokenizer 口径。
	Model string `json:"model,omitempty"`
	// Intent 是本轮的意图分类（决定弹性层的预算权重）。
	Intent string `json:"intent,omitempty"`
	// Window / Usable / ReservedOutput 是上下文窗口口径：总窗口、扣除输出预留后的
	// 可用窗口、输出预留。
	Window         int `json:"window,omitempty"`
	Usable         int `json:"usable,omitempty"`
	ReservedOutput int `json:"reserved_output,omitempty"`
	// CompactThreshold 是触发压缩的阈值（Agent 链路），未启用为 0。
	CompactThreshold int `json:"compact_threshold,omitempty"`
	// Rounds 是本轮消耗的模型往返数（Agent 链路的 ReAct 轮次）。
	Rounds int `json:"rounds,omitempty"`
	// Budget 是五层预算；Agent 链路不做五层切分，因此为零值。
	Budget ContextLayerTokens `json:"budget"`
	// Used 是五层实际占用（精确 token 计数）。
	Used ContextLayerTokens `json:"used"`
	// Actions 是弹性调整动作的原文摘要（只进可观测，绝不进 prompt）。
	Actions []string `json:"actions,omitempty"`
	// Events 是本轮触发的压缩/截断事件。
	Events []ContextTurnEvent `json:"events,omitempty"`
}

// Empty 报告这份归因是否没有任何可展示的内容。空值存 NULL（见 Value）。
func (s *ContextTurnStats) Empty() bool {
	if s == nil {
		return true
	}
	return s.Window == 0 && s.Usable == 0 && s.Rounds == 0 &&
		s.Budget.IsZero() && s.Used.IsZero() &&
		len(s.Actions) == 0 && len(s.Events) == 0
}

// RecordEvent 追加一个事件，并在调用方只给了前后规模时补齐释放量。
func (s *ContextTurnStats) RecordEvent(ev ContextTurnEvent) {
	if s == nil {
		return
	}
	if ev.FreedTokens == 0 && ev.TokensBefore > 0 {
		ev.FreedTokens = ev.TokensBefore - ev.TokensAfter
	}
	s.Events = append(s.Events, ev)
}

// LayerEntries 展开五层的实际占用与占比，供单轮明细直接渲染。
func (s *ContextTurnStats) LayerEntries() []ContextLayerUsage {
	if s == nil {
		return nil
	}
	return s.Used.Entries(s.Budget)
}

// Value 实现 driver.Valuer。空归因（以及 nil）存 NULL：它与「这一轮没有采集到
// 上下文归因」在语义上不可区分，读取侧只认 NULL / 非 NULL。指针接收者与 Message
// 上其它 jsonb 列（ContextCheckpoint 等）保持一致。
func (s *ContextTurnStats) Value() (driver.Value, error) {
	if s.Empty() {
		return nil, nil
	}
	return json.Marshal(s)
}

// Scan 实现 sql.Scanner。
func (s *ContextTurnStats) Scan(value any) error {
	if value == nil {
		*s = ContextTurnStats{}
		return nil
	}
	var b []byte
	switch v := value.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("types: cannot scan context turn stats from unsupported type")
	}
	if len(b) == 0 {
		*s = ContextTurnStats{}
		return nil
	}
	return json.Unmarshal(b, s)
}

// ---------------------------------------------------------------------------
// 采集通道：chat pipeline 与 Agent 引擎共用同一份载荷形状
// ---------------------------------------------------------------------------

// TurnUsageData 是 chat pipeline 推给 handler 落库的每轮用量载荷。
// 它通过 EventBus 的 chat.complete 事件传递（该事件没有其它订阅者，因此不会
// 污染 SSE 流），handler 收到后写到 assistant 消息的 usage / context_stats 列上。
type TurnUsageData struct {
	MessageID    string            `json:"message_id,omitempty"`
	Usage        *TokenUsage       `json:"usage,omitempty"`
	ContextStats *ContextTurnStats `json:"context_stats,omitempty"`
}

// Agent 引擎把同一份数据挂在 EventAgentComplete 的 Extra 上。进程内传递，
// 不做 JSON 序列化，因此值是 *TokenUsage / *ContextTurnStats 本身。
const (
	TurnUsageExtraKey        = "turn_usage"
	TurnContextStatsExtraKey = "turn_context_stats"
)

// NewTurnUsageExtra 把一轮的用量与归因装进事件 Extra。两条采集通道（Agent 引擎的
// agent.complete、chat pipeline 经 handler 转挂的 agent.complete）共用这个形状，
// 于是消费侧只需要认一种键名。两者都缺时返回 nil，不凭空造一个空 map。
func NewTurnUsageExtra(usage *TokenUsage, stats *ContextTurnStats) map[string]interface{} {
	if usage == nil && stats == nil {
		return nil
	}
	extra := make(map[string]interface{}, 2)
	if usage != nil {
		extra[TurnUsageExtraKey] = usage
	}
	if stats != nil {
		extra[TurnContextStatsExtraKey] = stats
	}
	return extra
}

// TurnUsageFromExtra 是 NewTurnUsageExtra 的反向操作。类型不符时安静地返回 nil：
// 归因缺失只让消息行上的 usage / context_stats 存 NULL，绝不该影响答案本身的落库。
func TurnUsageFromExtra(extra map[string]interface{}) (*TokenUsage, *ContextTurnStats) {
	usage, _ := extra[TurnUsageExtraKey].(*TokenUsage)
	stats, _ := extra[TurnContextStatsExtraKey].(*ContextTurnStats)
	return usage, stats
}

// ContextTurnStatsFromDiagnostics 把 chat pipeline 的五层诊断转成可持久化的归因。
// diag 为 nil（未开启分层预算）时返回 nil，于是这一行的 context_stats 存 NULL。
func ContextTurnStatsFromDiagnostics(source string, diag *ContextDiagnostics) *ContextTurnStats {
	if diag == nil {
		return nil
	}
	stats := &ContextTurnStats{
		Source:         source,
		Intent:         diag.Intent,
		Window:         diag.Window,
		Usable:         diag.Usable,
		ReservedOutput: diag.ReservedOutput,
		Budget:         layerTokensOf(diag.Budget),
		Used:           layerTokensOf(diag.Used),
	}
	if len(diag.Actions) > 0 {
		stats.Actions = append([]string(nil), diag.Actions...)
		for _, action := range diag.Actions {
			if ev, ok := parseContextAction(action); ok {
				stats.Events = append(stats.Events, ev)
			}
		}
	}
	if stats.Empty() {
		return nil
	}
	return stats
}

func layerTokensOf(b ContextLayerBudget) ContextLayerTokens {
	return ContextLayerTokens{
		System:    b.System,
		Memory:    b.Memory,
		Retrieval: b.Retrieval,
		History:   b.History,
		Query:     b.Query,
	}
}

// parseContextAction 把装配器的一条弹性调整动作解析成结构化事件。
//
// 只识别 contextx 已知的动作格式（见 internal/contextx/assembler.go 与 memory.go）：
// 预算互借（"L2 slack N tokens yielded to L3"）不是压缩/截断，返回 false，
// 原文仍然保留在 ContextTurnStats.Actions 里。无法解析的格式同样返回 false，
// 宁可少一条结构化事件，也不要造出一条口径错误的。
func parseContextAction(action string) (ContextTurnEvent, bool) {
	text := strings.TrimSpace(action)
	if text == "" {
		return ContextTurnEvent{}, false
	}
	layer := contextActionLayer(text)

	before, after, hasDelta := scanTokenDelta(text)
	ev := ContextTurnEvent{Layer: layer, Detail: text, TokensBefore: before, TokensAfter: after}
	if hasDelta {
		ev.FreedTokens = before - after
	}

	switch {
	case strings.Contains(text, "compressed"):
		ev.Kind = ContextEventCompaction
		ev.Reason = "history_compressed"
	case strings.Contains(text, "shrunk"):
		ev.Kind = ContextEventShrink
		ev.Reason = "tiered_shrink"
	case strings.Contains(text, "hard-truncated"), strings.Contains(text, "truncated"):
		ev.Kind = ContextEventTruncation
		ev.Reason = "hard_truncated"
		if !hasDelta && strings.Contains(text, "to ") {
			// "L2 hard-truncated to budget" / "L1 hard-truncated to N tokens"：
			// 只有截断目标，没有截断前规模。
			ev.TokensAfter = trailingTokenTarget(text)
		}
	case strings.Contains(text, "degraded"):
		ev.Kind = ContextEventTruncation
		ev.Reason = "resident_degraded"
		ev.Degraded = true
	case strings.Contains(text, "dropped"):
		ev.Kind = ContextEventTruncation
		ev.Reason = "entries_dropped"
	default:
		return ContextTurnEvent{}, false
	}
	return ev, true
}

// contextActionLayer 取动作开头的层标签（"L0_system truncated …" / "L1 dropped …"）。
func contextActionLayer(text string) string {
	for _, name := range []string{
		ContextLayerSystem, ContextLayerMemory, ContextLayerRetrieval,
		ContextLayerHistory, ContextLayerQuery,
	} {
		if strings.HasPrefix(text, name) {
			return name
		}
		// 部分动作只写短标签（"L2 slack …"）。
		if short := name[:2]; strings.HasPrefix(text, short+" ") {
			return name
		}
	}
	return ""
}

// scanTokenDelta 解析 "A -> B tokens" 形式的前后规模。
func scanTokenDelta(text string) (before, after int, ok bool) {
	idx := strings.Index(text, " -> ")
	if idx < 0 {
		return 0, 0, false
	}
	before = lastIntBefore(text[:idx])
	after = firstIntAfter(text[idx+len(" -> "):])
	if before == 0 && after == 0 {
		return 0, 0, false
	}
	return before, after, true
}

// lastIntBefore 取字符串末尾的那段数字（"L2 shrunk 1234" → 1234）。
func lastIntBefore(text string) int {
	end := len(text)
	start := end
	for start > 0 && isDigit(text[start-1]) {
		start--
	}
	if start == end {
		return 0
	}
	n, err := strconv.Atoi(text[start:end])
	if err != nil {
		return 0
	}
	return n
}

// firstIntAfter 取字符串开头的那段数字（"800 tokens (tiered)" → 800）。
func firstIntAfter(text string) int {
	start := 0
	for start < len(text) && text[start] == ' ' {
		start++
	}
	end := start
	for end < len(text) && isDigit(text[end]) {
		end++
	}
	if start == end {
		return 0
	}
	n, err := strconv.Atoi(text[start:end])
	if err != nil {
		return 0
	}
	return n
}

// trailingTokenTarget 解析 "… to 800 tokens" 里的目标规模。
func trailingTokenTarget(text string) int {
	idx := strings.LastIndex(text, " to ")
	if idx < 0 {
		return 0
	}
	return firstIntAfter(text[idx+len(" to "):])
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// ---------------------------------------------------------------------------
// 管理员聚合看板（PRD FR-12 ③）
// ---------------------------------------------------------------------------

// 看板时间粒度。
const (
	TokenUsageGranularityDay  = "day"
	TokenUsageGranularityHour = "hour"
)

var (
	// ErrTokenUsageForbidden 表示调用者不是本工作区的管理员。
	ErrTokenUsageForbidden = errors.New("token usage dashboard requires a workspace administrator")
	// ErrTokenUsageBadRequest 表示时间范围/粒度参数非法。
	ErrTokenUsageBadRequest = errors.New("token usage dashboard request invalid")
)

// TokenUsageQuery 是一次看板查询的作用域与时间窗。TenantID 由服务端从调用者
// 身份解析后写死，绝不接受调用方传参（PRD §5.1）。
type TokenUsageQuery struct {
	TenantID    uint64    `json:"tenant_id"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Granularity string    `json:"granularity"`
}

// MessageTurnUsage 是聚合扫描时从 messages 行上取出的最小列集。
type MessageTurnUsage struct {
	ID           string            `json:"id"            gorm:"column:id"`
	CreatedAt    time.Time         `json:"created_at"    gorm:"column:created_at"`
	Usage        *TokenUsage       `json:"usage,omitempty" gorm:"column:usage"`
	ContextStats *ContextTurnStats `json:"context_stats,omitempty" gorm:"column:context_stats"`
}

// TokenUsageTrendPoint 是一个时间桶里的用量。
type TokenUsageTrendPoint struct {
	Bucket           string `json:"bucket"`
	Turns            int    `json:"turns"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
}

// TokenUsageLayerStat 是一层在时间窗内的平均占用与平均占比。
type TokenUsageLayerStat struct {
	Layer     string  `json:"layer"`
	Turns     int     `json:"turns"`
	Total     int     `json:"total_tokens"`
	AvgTokens float64 `json:"avg_tokens"`
	AvgRatio  float64 `json:"avg_ratio"`
}

// TokenUsageCompactionStat 是一类压缩/截断事件的触发频率与收益。
type TokenUsageCompactionStat struct {
	Kind        string  `json:"kind"`
	Reason      string  `json:"reason,omitempty"`
	Count       int     `json:"count"`
	FreedTokens int     `json:"freed_tokens"`
	Degraded    int     `json:"degraded"`
	AvgFreed    float64 `json:"avg_freed_tokens"`
}

// TokenUsageCacheStat 是缓存命中率。命中率有两个口径，看板都要给：
//   - HitRate：按轮次，供应商回报了缓存计量的轮次里命中（cache_read > 0）的比例；
//   - TokenHitRate：按 token，cache_read_tokens / prompt_tokens。
type TokenUsageCacheStat struct {
	Turns           int     `json:"turns"`
	ReportedTurns   int     `json:"reported_turns"`
	HitTurns        int     `json:"hit_turns"`
	HitRate         float64 `json:"hit_rate"`
	TokenHitRate    float64 `json:"token_hit_rate"`
	PromptTokens    int     `json:"prompt_tokens"`
	CacheReadTokens int     `json:"cache_read_tokens"`
	// ByStatus 按 PromptCacheStatus 分桶（unsupported / unreported / miss / hit）。
	// 命中率只在"供应商真的回报了缓存计量"的轮次上才有意义，因此看板必须能把
	// 不支持缓存计量的模型与真正的未命中区分开。
	ByStatus map[string]int `json:"by_status,omitempty"`
}

// TokenUsageTotals 是时间窗内的总量。
type TokenUsageTotals struct {
	Turns            int `json:"turns"`
	TurnsWithUsage   int `json:"turns_with_usage"`
	TurnsWithStats   int `json:"turns_with_context_stats"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
}

// TokenUsageOverview 是管理员看板的完整响应体。
type TokenUsageOverview struct {
	TenantID    uint64    `json:"tenant_id"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Granularity string    `json:"granularity"`

	Totals TokenUsageTotals `json:"totals"`
	// Trend 按时间升序，空桶也保留（前端画折线不需要自己补洞）。
	Trend []TokenUsageTrendPoint `json:"trend"`
	// Layers 固定 L0→L4 五行。
	Layers []TokenUsageLayerStat `json:"layers"`
	// Compaction 是事件明细，CompactionTurns / CompactionEvents 给出频率。
	Compaction       []TokenUsageCompactionStat `json:"compaction"`
	CompactionTurns  int                        `json:"compaction_turns"`
	CompactionEvents int                        `json:"compaction_events"`
	CompactionRate   float64                    `json:"compaction_rate"`
	Cache            TokenUsageCacheStat        `json:"cache"`

	// ScannedTurns 是参与聚合的 assistant 行数；Truncated 为真表示时间窗内的行
	// 超过了单次聚合上限，看板数据只覆盖其中最早的一部分。
	ScannedTurns int  `json:"scanned_turns"`
	Truncated    bool `json:"truncated"`

	GeneratedAt time.Time `json:"generated_at"`
}
