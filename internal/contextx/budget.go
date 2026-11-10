package contextx

import (
	"strconv"
	"strings"
)

// Layer identifies one of the five context budget layers (L0-L4).
type Layer int

const (
	// LayerSystem (L0) — system prompt + instructions. Fixed, must not overflow.
	LayerSystem Layer = iota
	// LayerMemory (L1) — user profile + long-term memory. Fixed, truncated by relevance.
	LayerMemory
	// LayerRetrieval (L2) — RAG retrieved chunks. Elastic; low-relevance chunks
	// degrade to summaries, then get dropped.
	LayerRetrieval
	// LayerHistory (L3) — conversation history. Elastic; old rounds get
	// summarized, sticky rounds are exempt.
	LayerHistory
	// LayerQuery (L4) — current user query + tool results. Fixed; tool results
	// are inner-summarized upstream before reaching the budget check.
	LayerQuery
)

func (l Layer) String() string {
	switch l {
	case LayerSystem:
		return "L0_system"
	case LayerMemory:
		return "L1_memory"
	case LayerRetrieval:
		return "L2_retrieval"
	case LayerHistory:
		return "L3_history"
	case LayerQuery:
		return "L4_query"
	default:
		return "unknown"
	}
}

// IntentClass is the coarse query-intent bucket used for dynamic budget
// allocation. It is derived from (but independent of) the pipeline's
// types.QueryIntent so this package stays import-light.
type IntentClass string

const (
	// IntentTech — code/technical questions: retrieval deserves the largest share.
	IntentTech IntentClass = "tech"
	// IntentChat — chitchat/creative: history continuity matters most.
	IntentChat IntentClass = "chat"
	// IntentAnalysis — summarize/analyze: retrieval and history share evenly.
	IntentAnalysis IntentClass = "analysis"
	// IntentGeneral — default balanced profile.
	IntentGeneral IntentClass = "general"
)

// BudgetProfile declares per-layer fractions of the usable context window.
// Fractions are normalized by Allocate, so they need not sum to exactly 1.
//
// 注意（FR-11 Prompt 缓存前缀稳定化）：System / Memory 两项只是固定占比的镜像，
// 供诊断展示；Allocate 一律使用 StableSystemFraction / StableMemoryFraction，
// 不随 IntentClass 变化，也不接受调用方覆盖。
type BudgetProfile struct {
	System    float64
	Memory    float64
	Retrieval float64
	History   float64
	Query     float64
}

// L0/L1 的固定占比。
//
// FR-11（Prompt 缓存前缀稳定化）约束：五层弹性互借只允许发生在 L2/L3/L4，
// L0（系统指令）与 L1（记忆常驻块）的预算必须在多轮之间保持不变。否则意图分类
// 的抖动会改变 L0/L1 的截断点，稳定前缀的字节随之改变，供应商侧的前缀缓存将
// 永远无法命中。因此这两个比例是常量，既不进入 intent profile，也不接受覆盖。
const (
	StableSystemFraction = 0.08
	StableMemoryFraction = 0.10
)

// defaultProfiles maps intent class to budget allocation. The elastic layers
// (Retrieval/History) yield unused budget to each other at assembly time.
// 只有弹性层（L2/L3/L4）随意图变化；L0/L1 恒为上面的稳定占比。
var defaultProfiles = map[IntentClass]BudgetProfile{
	// 技术/代码问题：检索层最大，历史收窄
	IntentTech: {System: StableSystemFraction, Memory: StableMemoryFraction, Retrieval: 0.50, History: 0.25, Query: 0.10},
	// 闲聊/创意：历史层最大，检索收窄
	IntentChat: {System: StableSystemFraction, Memory: StableMemoryFraction, Retrieval: 0.20, History: 0.45, Query: 0.15},
	// 摘要/分析：检索与历史均衡
	IntentAnalysis: {System: StableSystemFraction, Memory: StableMemoryFraction, Retrieval: 0.40, History: 0.35, Query: 0.10},
	// 默认均衡
	IntentGeneral: {System: StableSystemFraction, Memory: StableMemoryFraction, Retrieval: 0.40, History: 0.32, Query: 0.10},
}

// ProfileForIntent returns the budget profile for an intent class.
// L0/L1 占比与意图无关（缓存前缀稳定性约束）。
func ProfileForIntent(class IntentClass) BudgetProfile {
	p, ok := defaultProfiles[class]
	if !ok {
		p = defaultProfiles[IntentGeneral]
	}
	p.System = StableSystemFraction
	p.Memory = StableMemoryFraction
	return p
}

// Budget holds concrete per-layer token budgets after allocation.
type Budget struct {
	// Total is the full context window (input + output).
	Total int
	// Usable is Total minus the reserved completion budget.
	Usable         int
	System         int
	Memory         int
	Retrieval      int
	History        int
	Query          int
	Profile        BudgetProfile
	Intent         IntentClass
	ReservedOutput int
}

// ForLayer returns the budget of a single layer.
func (b Budget) ForLayer(l Layer) int {
	switch l {
	case LayerSystem:
		return b.System
	case LayerMemory:
		return b.Memory
	case LayerRetrieval:
		return b.Retrieval
	case LayerHistory:
		return b.History
	case LayerQuery:
		return b.Query
	default:
		return 0
	}
}

// SetLayer overwrites one layer's budget (used by elastic redistribution).
//
// L0（系统指令）与 L1（记忆常驻块）的写入被拒绝：这是 FR-11 的代码级不变量——
// 弹性互借只允许发生在 L2/L3/L4，稳定前缀的预算一旦被逐轮改写，供应商侧的
// 前缀缓存就会因为截断点漂移而永远命中不了。
func (b *Budget) SetLayer(l Layer, v int) {
	switch l {
	case LayerSystem, LayerMemory:
		// 稳定前缀层：预算固定，拒绝任何弹性改写。
		return
	case LayerRetrieval:
		b.Retrieval = v
	case LayerHistory:
		b.History = v
	case LayerQuery:
		b.Query = v
	}
}

// DefaultCompletionReserve is the output-token reserve used when the caller
// does not specify MaxCompletionTokens.
const DefaultCompletionReserve = 4096

// minUsableWindow floors pathological configurations.
const minUsableWindow = 2048

// Allocate converts a profile into concrete token budgets.
//   - totalWindow: full model context window (input + output)
//   - reserveOutput: tokens reserved for the completion; <= 0 uses the default
//   - profile overrides: zero-valued fields fall back to the intent profile;
//     System / Memory 不接受覆盖（稳定前缀层，见 StableSystemFraction 注释）
//
// 分配顺序（FR-11）：先按固定占比切出 L0/L1，剩余部分再按 profile 的相对权重
// 分给 L2/L3/L4。这样意图分类只影响弹性层，L0/L1 的 token 额度与截断点在多轮
// 之间恒定，稳定前缀才能被供应商缓存命中。
func Allocate(totalWindow int, class IntentClass, reserveOutput int, override *BudgetProfile) Budget {
	if totalWindow <= 0 {
		totalWindow = DefaultContextWindow(VendorGeneric, "")
	}
	if reserveOutput <= 0 {
		reserveOutput = DefaultCompletionReserve
	}
	usable := totalWindow - reserveOutput
	if usable < minUsableWindow {
		usable = minUsableWindow
	}

	profile := ProfileForIntent(class)
	if override != nil {
		// 仅弹性层可被调用方调整；L0/L1 是缓存前缀稳定性约束下的常量。
		if override.Retrieval > 0 {
			profile.Retrieval = override.Retrieval
		}
		if override.History > 0 {
			profile.History = override.History
		}
		if override.Query > 0 {
			profile.Query = override.Query
		}
	}
	profile.System = StableSystemFraction
	profile.Memory = StableMemoryFraction

	// L0/L1：固定占比，与意图无关。
	system := int(StableSystemFraction * float64(usable))
	memory := int(StableMemoryFraction * float64(usable))

	// L2/L3/L4：瓜分剩余窗口，按 profile 的相对权重归一化。
	elastic := usable - system - memory
	if elastic < 0 {
		elastic = 0
	}
	weight := profile.Retrieval + profile.History + profile.Query
	if weight <= 0 {
		weight = 1
	}
	retrieval := int(float64(elastic) * profile.Retrieval / weight)
	history := int(float64(elastic) * profile.History / weight)
	query := elastic - retrieval - history

	b := Budget{
		Total:          totalWindow,
		Usable:         usable,
		System:         system,
		Memory:         memory,
		Retrieval:      retrieval,
		History:        history,
		Query:          query,
		Profile:        profile,
		Intent:         class,
		ReservedOutput: reserveOutput,
	}
	return b
}

// StableLayerBudgets 返回与意图无关的 L0/L1 token 预算。
// 记忆注入（FR-20）发生在意图分类之前，需要用它提前算出 L1 额度；由于 L0/L1
// 占比是常量，这里的 Memory 与装配阶段 Allocate 得到的值完全一致。
func StableLayerBudgets(totalWindow, reserveOutput int) (system, memory int) {
	b := Allocate(totalWindow, IntentGeneral, reserveOutput, nil)
	return b.System, b.Memory
}

// vendorWindows maps vendor → default context window when the model metadata
// does not declare one. Conservative values; model-specific overrides below.
var vendorWindows = map[Vendor]int{
	VendorOpenAI:   128000,
	VendorAzure:    128000,
	VendorQwen:     131072,
	VendorGLM:      131072,
	VendorDeepSeek: 65536,
	VendorClaude:   200000,
	VendorLlama:    8192,
	VendorGeneric:  32768,
}

// modelWindowHints gives finer-grained windows for well-known model names.
var modelWindowHints = []struct {
	match  string
	window int
}{
	{"gpt-3.5", 16385},
	{"gpt-4o-mini", 128000},
	{"gpt-4o", 128000},
	{"gpt-4-turbo", 128000},
	{"gpt-4", 8192},
	{"qwen-long", 10000000},
	{"qwen3", 131072},
	{"qwen2", 131072},
	{"qwen", 32768},
	{"glm-4", 131072},
	{"chatglm", 32768},
	{"deepseek-v3", 65536},
	{"deepseek-r1", 65536},
	{"deepseek", 32768},
	{"claude", 200000},
	{"llama3", 8192},
	{"llama", 4096},
}

// DefaultContextWindow resolves the context window for a vendor/model pair
// when no explicit configuration exists.
func DefaultContextWindow(vendor Vendor, modelName string) int {
	name := strings.ToLower(modelName)
	for _, h := range modelWindowHints {
		if strings.Contains(name, h.match) {
			return h.window
		}
	}
	if w, ok := vendorWindows[vendor]; ok {
		return w
	}
	return vendorWindows[VendorGeneric]
}

// ResolveContextWindow picks the effective context window, in priority order:
//  1. explicitMaxTokens (tenant/agent ContextConfig.MaxTokens)
//  2. model ExtraConfig["context_window"] (per-model declaration)
//  3. vendor/model defaults
func ResolveContextWindow(explicitMaxTokens int, extraConfig map[string]string, vendor Vendor, modelName string) int {
	if explicitMaxTokens > 0 {
		return explicitMaxTokens
	}
	if extraConfig != nil {
		if v, ok := extraConfig["context_window"]; ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				return n
			}
		}
	}
	return DefaultContextWindow(vendor, modelName)
}
