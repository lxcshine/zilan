package contextx

import (
	"fmt"
	"sort"
	"strings"
)

// memory.go 实现 L1（记忆层）的确定性渲染与预算裁剪，同时服务两个需求：
//
//	FR-11 Prompt 缓存前缀稳定化 —— 记忆常驻块属于稳定前缀的一部分，渲染顺序必须
//	        确定（分节固定顺序 + 节内稳定键排序），且相同输入必须得到相同字节。
//	FR-20 上下文预算 × 记忆预算统一调度 —— 记忆注入用精确 tokenizer 计量，超预算时
//	        按召回评分从低到高"整条"移除，常驻块只在"整块保留 / 整块降级为摘要"
//	        两态之间切换，绝不逐轮微调长度。

// MemoryBlock 是 L1 记忆层的最小注入单元（渲染后的一行）。
type MemoryBlock struct {
	// Text 是该条记忆渲染后的文本（不含结尾换行）。
	Text string
	// Summary 是常驻块的"摘要降级形式"。为空表示该块不可降级，预算不足时整条丢弃。
	Summary string
	// Score 是召回评分：语义相似度 × 时间衰减 × 访问频次。预算裁剪按它升序移除。
	Score float64
	// Resident 标记常驻块（soul / profile / preference / skill）：它们不参与
	// 逐条裁剪，只走"整块保留 / 整块降级"两态。
	Resident bool
}

// MemorySection 是记忆块的一个分节。分节顺序与节内顺序都由构造方固定，渲染结果
// 是这些顺序的纯函数。
type MemorySection struct {
	// Title 是分节标题（含自身的 "### " 等前缀）；渲染时独占一行。
	Title  string
	Blocks []MemoryBlock
}

// MemoryPrompt 是 L1 记忆层的结构化渲染输入。
type MemoryPrompt struct {
	// Header 是块首固定说明文字（仅在有任何条目时渲染）。
	Header   string
	Sections []MemorySection
}

// BlockCount 返回全部条目数。
func (p MemoryPrompt) BlockCount() int {
	n := 0
	for _, s := range p.Sections {
		n += len(s.Blocks)
	}
	return n
}

// IsEmpty 报告是否没有任何可注入条目。
func (p MemoryPrompt) IsEmpty() bool { return p.BlockCount() == 0 }

// RenderMemoryPrompt 按固定分节顺序渲染 L1 记忆块。
//
// 幂等：相同输入必得相同字节输出（FR-11 ①④）。没有任何条目时返回空串，
// 避免只渲染出一个空的块头。
func RenderMemoryPrompt(p MemoryPrompt) string {
	if p.IsEmpty() {
		return ""
	}
	var b strings.Builder
	b.WriteString(p.Header)
	for _, s := range p.Sections {
		if len(s.Blocks) == 0 {
			continue
		}
		b.WriteString("\n")
		b.WriteString(s.Title)
		b.WriteString("\n")
		for _, blk := range s.Blocks {
			b.WriteString(blk.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ResidentKeepRatio 是"常驻块整块保留"的判据比例：常驻块整块 token 成本
// ≤ L1 预算 × 该比例时整块保留，否则整块降级为摘要形式（FR-20 ③）。
// 只有两态，不按轮微调长度——这样 L1 的渲染结果在多轮之间要么完全不变，
// 要么一次性切换，稳定前缀不会因为记忆块长度的细微抖动而失效。
const ResidentKeepRatio = 0.6

// MemoryFitResult 是 L1 预算裁剪的结果。
type MemoryFitResult struct {
	// Text 是裁剪后渲染出的记忆块文本（可能为空串）。
	Text string
	// Used 是 Text 的精确 token 用量（与裁剪口径一致，绝不使用字符近似）。
	Used int
	// Dropped 是因预算被整条移除的非常驻条目数。
	Dropped int
	// Degraded 报告常驻块是否整块降级为摘要形式。
	Degraded bool
	// Actions 是诊断动作，只进入 Diagnostics，绝不进入 prompt。
	Actions []string
}

// FitMemory 把结构化记忆块裁剪进 budget（token）内：
//
//  1. 常驻块整块计量：整块成本 ≤ budget × ResidentKeepRatio → 整块保留；
//     否则整块降级为摘要形式（无摘要形式的块整条丢弃）。
//  2. 其余（语义召回）条目按 Score 升序整条移除，直到放入预算；评分相同时按
//     原始顺序决定去留，保证多轮结果字节一致。
//  3. 兜底：若渲染结果仍然超预算（块头/标题等固定开销），做一次尾部硬截断。
//
// 裁剪是 (prompt, budget) 的纯函数：相同输入多轮渲染结果完全相同。
func (a *Assembler) FitMemory(prompt MemoryPrompt, budget int) MemoryFitResult {
	res := MemoryFitResult{}
	if prompt.IsEmpty() {
		return res
	}
	if budget <= 0 {
		res.Actions = append(res.Actions, "L1 memory dropped: zero budget")
		return res
	}

	// keep[section][index] 标记该条是否保留；常驻块成本按精确 tokenizer 计量。
	keep := make([][]bool, len(prompt.Sections))
	residentCost := 0
	for i, s := range prompt.Sections {
		keep[i] = make([]bool, len(s.Blocks))
		for j := range s.Blocks {
			keep[i][j] = true
			if s.Blocks[j].Resident {
				residentCost += a.Counter.Count(s.Blocks[j].Text)
			}
		}
	}

	// --- 常驻块：整块保留 / 整块降级两态 ---
	// 判据是"常驻块整块 token 成本 ≤ L1 预算 × ResidentKeepRatio"，与本轮语义条目
	// 的数量无关，因此决策在一个会话内是粘性的：决策不变 → 渲染字节不变（FR-11）。
	residentKeepLimit := int(float64(budget) * ResidentKeepRatio)
	if residentCost > residentKeepLimit {
		for i, s := range prompt.Sections {
			for j := range s.Blocks {
				if !s.Blocks[j].Resident {
					continue
				}
				if strings.TrimSpace(s.Blocks[j].Summary) == "" {
					// 不可降级：整条丢弃（仍是整块语义，不做逐字裁剪）。
					keep[i][j] = false
				}
			}
		}
		res.Degraded = true
		res.Actions = append(res.Actions, fmt.Sprintf(
			"L1 resident block degraded to summary (%d > %d tokens, %.0f%% of budget)",
			residentCost, residentKeepLimit, ResidentKeepRatio*100))
	}

	// --- 语义条目：按 Score 升序整条移除 ---
	type semanticRef struct {
		section int
		index   int
		score   float64
	}
	var semantics []semanticRef
	for i, s := range prompt.Sections {
		for j := range s.Blocks {
			if s.Blocks[j].Resident || !keep[i][j] {
				continue
			}
			semantics = append(semantics, semanticRef{section: i, index: j, score: s.Blocks[j].Score})
		}
	}

	// 移除顺序：Score 升序；同分按 (分节, 节内) 原始顺序，保证确定性。
	sort.SliceStable(semantics, func(x, y int) bool {
		if semantics[x].score != semantics[y].score {
			return semantics[x].score < semantics[y].score
		}
		if semantics[x].section != semantics[y].section {
			return semantics[x].section < semantics[y].section
		}
		return semantics[x].index < semantics[y].index
	})

	// --- 渲染 + 精确复核 ---
	// 每移除一条都重新渲染并用精确 tokenizer 计量：逐条成本相加与整体渲染之间存在
	// BPE 边界差异，最终判定必须以整体渲染的精确计数为准（FR-20 ①）。
	render := func() string {
		fitted := MemoryPrompt{Header: prompt.Header}
		for i, s := range prompt.Sections {
			section := MemorySection{Title: s.Title}
			for j := range s.Blocks {
				if !keep[i][j] {
					continue
				}
				blk := s.Blocks[j]
				if blk.Resident && res.Degraded && strings.TrimSpace(blk.Summary) != "" {
					// 整块降级：常驻条目统一替换为摘要形式。
					blk.Text = strings.TrimSpace(blk.Summary)
				}
				section.Blocks = append(section.Blocks, blk)
			}
			fitted.Sections = append(fitted.Sections, section)
		}
		return RenderMemoryPrompt(fitted)
	}

	res.Text = render()
	res.Used = a.Counter.Count(res.Text)
	// semantics 已按移除优先级（Score 升序）排序，逐条整条移除直到放入预算。
	for next := 0; res.Used > budget && next < len(semantics); next++ {
		ref := semantics[next]
		if !keep[ref.section][ref.index] {
			continue
		}
		keep[ref.section][ref.index] = false
		res.Dropped++
		res.Text = render()
		res.Used = a.Counter.Count(res.Text)
	}
	if res.Dropped > 0 {
		res.Actions = append(res.Actions, fmt.Sprintf(
			"L1 dropped %d lowest-score memory entries to fit %d tokens", res.Dropped, budget))
	}

	// 兜底：语义条目已全部移除仍然超预算（常驻块整块成本过高）时，
	// 才做一次尾部硬截断，保证 L1 绝不突破预算。
	if res.Used > budget {
		res.Text = TruncateToTokens(res.Text, budget, a.Counter)
		res.Used = a.Counter.Count(res.Text)
		res.Actions = append(res.Actions, fmt.Sprintf("L1 hard-truncated to %d tokens", budget))
	}
	return res
}
