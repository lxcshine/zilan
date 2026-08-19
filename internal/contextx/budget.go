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
type BudgetProfile struct {
	System    float64
	Memory    float64
	Retrieval float64
	History   float64
	Query     float64
}

// defaultProfiles maps intent class to budget allocation. The elastic layers
// (Retrieval/History) yield unused budget to each other at assembly time.
var defaultProfiles = map[IntentClass]BudgetProfile{
	// 技术/代码问题：检索层 50%，历史收窄
	IntentTech: {System: 0.07, Memory: 0.08, Retrieval: 0.50, History: 0.25, Query: 0.10},
	// 闲聊/创意：历史层 40%，检索收窄
	IntentChat: {System: 0.08, Memory: 0.12, Retrieval: 0.20, History: 0.45, Query: 0.15},
	// 摘要/分析：检索与历史均衡
	IntentAnalysis: {System: 0.07, Memory: 0.08, Retrieval: 0.40, History: 0.35, Query: 0.10},
	// 默认均衡
	IntentGeneral: {System: 0.08, Memory: 0.10, Retrieval: 0.40, History: 0.32, Query: 0.10},
}

// ProfileForIntent returns the budget profile for an intent class.
func ProfileForIntent(class IntentClass) BudgetProfile {
	if p, ok := defaultProfiles[class]; ok {
		return p
	}
	return defaultProfiles[IntentGeneral]
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
func (b *Budget) SetLayer(l Layer, v int) {
	switch l {
	case LayerSystem:
		b.System = v
	case LayerMemory:
		b.Memory = v
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
//   - profile overrides: zero-valued fields fall back to the intent profile
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
		if override.System > 0 {
			profile.System = override.System
		}
		if override.Memory > 0 {
			profile.Memory = override.Memory
		}
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

	sum := profile.System + profile.Memory + profile.Retrieval + profile.History + profile.Query
	if sum <= 0 {
		sum = 1
	}
	scale := float64(usable) / sum

	b := Budget{
		Total:          totalWindow,
		Usable:         usable,
		System:         int(profile.System * scale),
		Memory:         int(profile.Memory * scale),
		Retrieval:      int(profile.Retrieval * scale),
		History:        int(profile.History * scale),
		Query:          int(profile.Query * scale),
		Profile:        profile,
		Intent:         class,
		ReservedOutput: reserveOutput,
	}
	return b
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
