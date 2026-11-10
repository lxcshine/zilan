package contextx

import (
	"fmt"
	"strings"
)

// Turn is one conversation round for history budgeting. It is deliberately
// decoupled from types.History so this package stays import-light.
type Turn struct {
	User      string
	Assistant string
	// Sticky marks rounds that must survive compression verbatim
	// (decisions, numbers, deadlines, user-approved answers).
	Sticky bool
	// Ref optionally carries a caller-side handle (e.g. the index of the
	// original history entry). It is preserved verbatim through compression
	// so callers can map kept turns back to their source records.
	Ref int
}

// Diagnostics records how the budget was spent for observability.
type Diagnostics struct {
	Window         int           `json:"window"`
	Usable         int           `json:"usable"`
	ReservedOutput int           `json:"reserved_output"`
	Intent         IntentClass   `json:"intent"`
	BudgetByLayer  map[Layer]int `json:"budget_by_layer"`
	UsedByLayer    map[Layer]int `json:"used_by_layer"`
	Actions        []string      `json:"actions"`
}

// Assembler performs five-layer budgeted context assembly.
type Assembler struct {
	Counter *Counter
}

// NewAssembler creates an Assembler; nil counter falls back to generic.
func NewAssembler(counter *Counter) *Assembler {
	if counter == nil {
		counter = NewCounter(VendorGeneric)
	}
	return &Assembler{Counter: counter}
}

// Input carries the raw per-layer content plus optional content-specific
// shrink callbacks.
type Input struct {
	System string
	// Memory 是已渲染的 L1 记忆块文本（兜底口径）。当 MemoryPrompt 非空时，
	// 装配器改用结构化裁剪，本字段被忽略。
	Memory string
	// MemoryPrompt 是结构化的 L1 记忆块（文本 + 召回评分 + 是否常驻）。
	// 提供它才能做"按相关性整条移除 + 常驻块整块降级"（FR-20），
	// 而不是把渲染结果从尾部硬截断。
	MemoryPrompt   MemoryPrompt
	Retrieval      string
	History        []Turn
	HistorySummary string
	Query          string

	Intent        IntentClass
	Window        int
	ReserveOutput int
	Override      *BudgetProfile

	// ShrinkRetrieval progressively degrades retrieval content to fit the
	// given token budget (relevance tiering). Nil means "hard truncate".
	ShrinkRetrieval func(budgetTokens int) string
	// CompressHistory fits history into the given budget. Nil uses the
	// default policy: sticky + newest rounds kept, summary prepended.
	CompressHistory func(h []Turn, summary string, budgetTokens int) []Turn
}

// Result holds the budgeted layer contents, ready for prompt rendering.
type Result struct {
	System    string
	Memory    string
	Retrieval string
	History   []Turn
	Query     string
	Diag      Diagnostics
	// MemoryDropped 是因 L1 预算被整条移除的语义记忆条目数（FR-20 可观测）。
	MemoryDropped int
	// MemoryDegraded 报告常驻块是否整块降级为摘要形式。该决策是
	// (常驻块成本, L1 预算) 的纯函数，因此在一个会话内天然稳定：
	// 决策不变 → L1 渲染字节不变 → 稳定前缀不变（FR-11）。
	MemoryDegraded bool
}

// Assemble applies the five-layer budget architecture:
//
//	L0 system    — fixed; hard-truncated at a boundary if it exceeds its share
//	L1 memory    — fixed; relevance-truncated entry-wise (structured blocks),
//	               resident block kept whole or degraded whole (FR-20)
//	L2 retrieval — elastic; shrunk via ShrinkRetrieval (tiering), then truncated
//	L3 history   — elastic; old rounds compressed to summary, sticky preserved
//	L4 query     — fixed; tail-truncated (tool results are pre-summarized upstream)
//
// Elastic layers yield unused budget to each other before any shrinking.
//
// 缓存前缀稳定性（FR-11）：L0/L1 的预算不随意图变化（见 budget.go），且这两层的
// 裁剪都是 (内容, 预算) 的纯函数——相同输入必得相同字节输出；弹性互借只发生在
// L2/L3/L4。Diagnostics.Actions 只用于可观测，绝不进入 prompt。
func (a *Assembler) Assemble(in Input) Result {
	budget := Allocate(in.Window, in.Intent, in.ReserveOutput, in.Override)
	diag := Diagnostics{
		Window:         budget.Total,
		Usable:         budget.Usable,
		ReservedOutput: budget.ReservedOutput,
		Intent:         budget.Intent,
		BudgetByLayer:  map[Layer]int{},
		UsedByLayer:    map[Layer]int{},
	}

	// --- Fixed layers first: count, then truncate to their guaranteed share ---
	system := a.fitFixed(in.System, budget.System, LayerSystem, &diag)
	memory, memoryFit := a.fitMemory(in, budget.Memory, &diag)
	query := a.fitFixed(in.Query, budget.Query, LayerQuery, &diag)

	// L1 的让渡额 = L1 预算 − 记忆实际用量；由于记忆裁剪只按"整条/整块"发生，
	// 让渡额天然是块粒度的，不会逐轮出现几 token 的抖动（FR-20 ③）。
	fixedUsed := diag.UsedByLayer[LayerSystem] + diag.UsedByLayer[LayerMemory] + diag.UsedByLayer[LayerQuery]

	// --- Elastic layers: redistribute slack between L2 and L3 ---
	// 弹性池 = 可用窗口 − 固定层"实际"用量：L0/L1/L4 没用满的额度整体让渡给
	// L2/L3（FR-20 ③）。因为 L1 的裁剪只按整条/整块发生，让渡额也是块粒度的，
	// 不会出现逐轮几 token 的抖动。
	elasticBudget := budget.Usable - fixedUsed
	if elasticBudget < 0 {
		elasticBudget = 0
	}
	l2Budget := budget.Retrieval
	l3Budget := budget.History
	if total := l2Budget + l3Budget; total != elasticBudget {
		// 按 profile 的相对权重双向缩放：固定层超支时收紧，固定层有富余时放开。
		if total <= 0 {
			total = 1
		}
		l2Budget = elasticBudget * l2Budget / total
		l3Budget = elasticBudget - l2Budget
	}

	rawL2 := a.Counter.Count(in.Retrieval)
	rawL3 := a.countHistory(in.History) + a.Counter.Count(in.HistorySummary)

	// Yield unused budget to the other side before shrinking anything.
	if rawL2 < l2Budget {
		slack := l2Budget - rawL2
		l3Budget += slack
		l2Budget -= slack
		diag.Actions = append(diag.Actions, fmt.Sprintf("L2 slack %d tokens yielded to L3", slack))
	}
	if rawL3 < l3Budget {
		slack := l3Budget - rawL3
		l2Budget += slack
		l3Budget -= slack
		diag.Actions = append(diag.Actions, fmt.Sprintf("L3 slack %d tokens yielded to L2", slack))
	}

	// --- L2 retrieval: tiered shrink then hard truncate as last resort ---
	retrieval := in.Retrieval
	if rawL2 > l2Budget {
		if in.ShrinkRetrieval != nil {
			retrieval = in.ShrinkRetrieval(l2Budget)
			diag.Actions = append(diag.Actions, fmt.Sprintf("L2 shrunk %d -> %d tokens (tiered)", rawL2, a.Counter.Count(retrieval)))
		}
		if a.Counter.Count(retrieval) > l2Budget {
			retrieval = TruncateToTokens(retrieval, l2Budget, a.Counter)
			diag.Actions = append(diag.Actions, "L2 hard-truncated to budget")
		}
	}

	// --- L3 history: compress old rounds, keep sticky + recent ---
	history := in.History
	if rawL3 > l3Budget {
		if in.CompressHistory != nil {
			history = in.CompressHistory(in.History, in.HistorySummary, l3Budget)
		} else {
			history = a.defaultCompressHistory(in.History, in.HistorySummary, l3Budget)
		}
		diag.Actions = append(diag.Actions, fmt.Sprintf("L3 compressed %d -> %d tokens", rawL3, a.countHistory(history)))
	} else if in.HistorySummary != "" {
		// Summary exists and fits: prepend as synthetic first turn for continuity.
		history = append([]Turn{{User: "[早前对话摘要]", Assistant: in.HistorySummary}}, history...)
	}

	// Record final state.
	diag.BudgetByLayer[LayerRetrieval] = l2Budget
	diag.BudgetByLayer[LayerHistory] = l3Budget
	diag.UsedByLayer[LayerRetrieval] = a.Counter.Count(retrieval)
	diag.UsedByLayer[LayerHistory] = a.countHistory(history)

	return Result{
		System:         system,
		Memory:         memory,
		Retrieval:      retrieval,
		History:        history,
		Query:          query,
		Diag:           diag,
		MemoryDropped:  memoryFit.Dropped,
		MemoryDegraded: memoryFit.Degraded,
	}
}

// fitFixed counts a fixed layer and truncates it to its budget when needed.
// 幂等：相同 (content, budget) 必得相同输出，且不引入任何随轮变化的信息
// （时间戳、随机顺序等），这是稳定前缀字节级稳定的前提（FR-11 ①）。
func (a *Assembler) fitFixed(content string, budget int, layer Layer, diag *Diagnostics) string {
	diag.BudgetByLayer[layer] = budget
	used := a.Counter.Count(content)
	if used > budget {
		content = TruncateToTokens(content, budget, a.Counter)
		diag.Actions = append(diag.Actions, fmt.Sprintf("%s truncated %d -> %d tokens", layer, used, budget))
		used = a.Counter.Count(content)
	}
	diag.UsedByLayer[layer] = used
	return content
}

// fitMemory 是 L1（记忆层）的专用裁剪，替代通用的尾部硬截断（FR-20）：
//   - 提供结构化记忆块时：按召回评分从低到高整条移除，常驻块整块保留或整块降级；
//   - 只有渲染文本时：退化为尾部硬截断（兜底，保证任何输入都不会突破 L1 预算）。
//
// 与 fitFixed 一样是纯函数：相同输入多轮渲染字节一致。
func (a *Assembler) fitMemory(in Input, budget int, diag *Diagnostics) (string, MemoryFitResult) {
	diag.BudgetByLayer[LayerMemory] = budget

	var fit MemoryFitResult
	if !in.MemoryPrompt.IsEmpty() {
		fit = a.FitMemory(in.MemoryPrompt, budget)
	} else {
		fit.Text = in.Memory
		fit.Used = a.Counter.Count(in.Memory)
		if fit.Used > budget {
			fit.Text = TruncateToTokens(in.Memory, budget, a.Counter)
			fit.Actions = append(fit.Actions,
				fmt.Sprintf("%s truncated %d -> %d tokens", LayerMemory, fit.Used, budget))
			fit.Used = a.Counter.Count(fit.Text)
		}
	}

	diag.UsedByLayer[LayerMemory] = fit.Used
	diag.Actions = append(diag.Actions, fit.Actions...)
	return fit.Text, fit
}

// countHistory sums tokens over history turns (user + assistant + framing).
func (a *Assembler) countHistory(history []Turn) int {
	total := 0
	for _, t := range history {
		total += a.Counter.Count(t.User) + a.Counter.Count(t.Assistant) + 2*perMessageOverhead
	}
	return total
}

// defaultCompressHistory keeps sticky turns and the newest rounds within
// budget; older non-sticky rounds are folded into the summary turn.
func (a *Assembler) defaultCompressHistory(history []Turn, summary string, budget int) []Turn {
	if budget <= 0 {
		return nil
	}
	var kept []Turn
	used := 0

	summaryTurn := Turn{User: "[早前对话摘要]", Assistant: summary}
	summaryCost := 0
	if summary != "" {
		summaryCost = a.Counter.Count(summary) + a.Counter.Count(summaryTurn.User) + 2*perMessageOverhead
	}

	// Walk newest-first: sticky turns and recent turns get priority.
	keptReversed := make([]Turn, 0, len(history))
	for i := len(history) - 1; i >= 0; i-- {
		t := history[i]
		cost := a.Counter.Count(t.User) + a.Counter.Count(t.Assistant) + 2*perMessageOverhead
		if t.Sticky || used+cost+summaryCost <= budget {
			keptReversed = append(keptReversed, t)
			used += cost
			continue
		}
		break
	}
	for i := len(keptReversed) - 1; i >= 0; i-- {
		kept = append(kept, keptReversed[i])
	}
	if summary != "" {
		kept = append([]Turn{summaryTurn}, kept...)
	}
	return kept
}

// TruncateToTokens cuts s to approximately budget tokens at the nearest
// paragraph/sentence boundary before the limit, never mid-word when avoidable.
func TruncateToTokens(s string, budget int, c *Counter) string {
	if budget <= 0 || s == "" {
		return ""
	}
	if c.Count(s) <= budget {
		return s
	}
	// Estimate the cut position from the token budget. CJK-heavy text has a
	// lower runes/token ratio; err on the safe side and verify afterwards.
	ratio := cjkRatio(s)
	runesPerToken := 4.0*(1-ratio) + 1.5*ratio
	cut := int(float64(budget) * runesPerToken)
	runes := []rune(s)
	if cut >= len(runes) {
		cut = len(runes)
	}
	// Back off to a paragraph, then newline, then sentence boundary.
	window := string(runes[:cut])
	if idx := strings.LastIndex(window, "\n\n"); idx > cut/2 {
		window = window[:idx]
	} else if idx := strings.LastIndex(window, "\n"); idx > cut/2 {
		window = window[:idx]
	} else if idx := strings.LastIndexAny(window, "。.!?"); idx > cut/2 {
		window = window[:idx+1]
	}
	// Hard-verify and shave if the boundary pick still overflows.
	for c.Count(window) > budget && len(window) > 0 {
		trim := len(window) / 20
		if trim < 8 {
			trim = 8
		}
		window = window[:len(window)-trim]
		if idx := strings.LastIndex(window, "\n"); idx > 0 {
			window = window[:idx]
		}
	}
	return window
}
