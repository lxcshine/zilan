package contextx

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Sticky detection — rounds that must never be summarized away
// ---------------------------------------------------------------------------

var stickyPatterns = []*regexp.Regexp{
	// Decisions / confirmations
	regexp.MustCompile(`(?i)(决定|已确定|拍板|定下来|最终方案|就这么定|decided|decision|final answer|confirmed)`),
	// Deadlines
	regexp.MustCompile(`(?i)(截止|死线|之前完成|之前交付|deadline|due\s+(by|on)|不晚于)`),
	// Explicit memory requests
	regexp.MustCompile(`(?i)(记住|不要忘记|别忘了|please remember|keep in mind)`),
	// Hard numbers with units (budgets, sizes, counts)
	regexp.MustCompile(`\d+(?:\.\d+)?\s*(?:万|亿|%|％|元|块|美元|GB|MB|TB|个|人|天|小时|分钟|台|次)`),
	// Explicit approval / rejection of an answer
	regexp.MustCompile(`(?i)(回答(得|的)?(很好|不对|错了)|这个答案对|说对了|exactly right|that'?s correct|wrong answer)`),
}

// IsStickyTurn reports whether a conversation round carries durable
// information (decisions, deadlines, key numbers, explicit user approval)
// that must be preserved verbatim through compression.
func IsStickyTurn(user, assistant string) bool {
	text := user + "\n" + assistant
	for _, p := range stickyPatterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Alias compression — replace repeated long entity names with short codes
// ---------------------------------------------------------------------------

// AliasConfig tunes alias extraction.
type AliasConfig struct {
	// MinRunes is the minimum entity length (in runes) worth aliasing.
	MinRunes int
	// MinOccurrences is the minimum repetition count across the history.
	MinOccurrences int
	// MaxAliases caps the alias table size to bound legend overhead.
	MaxAliases int
}

// DefaultAliasConfig returns sensible defaults.
func DefaultAliasConfig() AliasConfig {
	return AliasConfig{MinRunes: 8, MinOccurrences: 3, MaxAliases: 8}
}

// Han entity candidate: 6+ Han runes ending in a common institutional suffix.
var hanEntityPattern = regexp.MustCompile(`[\p{Han}]{6,24}(?:数据库|知识库|系统|平台|服务|集群|中心|引擎|框架|模型|方案|项目)`)

// Latin entity candidate: multi-word Capitalized phrase or long token.
var latinEntityPattern = regexp.MustCompile(`\b(?:[A-Z][\w-]*(?:\s+|$)){2,4}|\b[A-Za-z][\w-]{11,}\b`)

// AliasResult carries compressed turns plus the legend to prepend once.
type AliasResult struct {
	Turns  []Turn
	Legend string // e.g. "【E1】=腾讯云向量数据库；【E2】=WeKnora Agent Runtime"
	Count  int    // number of aliases applied
}

// CompressAliases finds long entity names repeated across the history and
// replaces them with 【E1】… short codes. The legend is returned separately
// so the caller can attach it to the summary/system layer exactly once.
func CompressAliases(history []Turn, cfg AliasConfig) AliasResult {
	if cfg.MinRunes <= 0 {
		cfg.MinRunes = DefaultAliasConfig().MinRunes
	}
	if cfg.MinOccurrences <= 0 {
		cfg.MinOccurrences = DefaultAliasConfig().MinOccurrences
	}
	if cfg.MaxAliases <= 0 {
		cfg.MaxAliases = DefaultAliasConfig().MaxAliases
	}
	if len(history) == 0 {
		return AliasResult{Turns: history}
	}

	var corpus strings.Builder
	for _, t := range history {
		corpus.WriteString(t.User)
		corpus.WriteString("\n")
		corpus.WriteString(t.Assistant)
		corpus.WriteString("\n")
	}
	text := corpus.String()

	counts := map[string]int{}
	for _, m := range hanEntityPattern.FindAllString(text, -1) {
		if len([]rune(m)) >= cfg.MinRunes {
			counts[m]++
		}
	}
	for _, m := range latinEntityPattern.FindAllString(text, -1) {
		m = strings.TrimSpace(m)
		if len([]rune(m)) >= cfg.MinRunes {
			counts[m]++
		}
	}

	// Rank by savings = (len - aliasLen) * occurrences.
	type candidate struct {
		name    string
		savings int
	}
	var cands []candidate
	for name, n := range counts {
		if n < cfg.MinOccurrences {
			continue
		}
		savings := (len([]rune(name)) - 4) * n // 【E1】 ≈ 4 runes
		if savings > 0 {
			cands = append(cands, candidate{name, savings})
		}
	}
	if len(cands) == 0 {
		return AliasResult{Turns: history}
	}
	for i := 0; i < len(cands); i++ {
		for j := i + 1; j < len(cands); j++ {
			if cands[j].savings > cands[i].savings {
				cands[i], cands[j] = cands[j], cands[i]
			}
		}
	}
	if len(cands) > cfg.MaxAliases {
		cands = cands[:cfg.MaxAliases]
	}

	aliases := map[string]string{}
	var legendParts []string
	for i, c := range cands {
		code := fmt.Sprintf("【E%d】", i+1)
		aliases[c.name] = code
		legendParts = append(legendParts, code+"="+c.name)
	}

	out := make([]Turn, len(history))
	for i, t := range history {
		out[i] = Turn{User: replaceAll(t.User, aliases), Assistant: replaceAll(t.Assistant, aliases), Sticky: t.Sticky, Ref: t.Ref}
	}
	return AliasResult{
		Turns:  out,
		Legend: strings.Join(legendParts, "；"),
		Count:  len(aliases),
	}
}

func replaceAll(s string, repl map[string]string) string {
	// Longest-first to avoid partial shadowing of nested names.
	keys := make([]string, 0, len(repl))
	for k := range repl {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if len(keys[j]) > len(keys[i]) {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, k := range keys {
		s = strings.ReplaceAll(s, k, repl[k])
	}
	return s
}

// ---------------------------------------------------------------------------
// Map-Reduce incremental summarization
// ---------------------------------------------------------------------------

// SummarizeFunc abstracts one LLM call: prompt in, summary out. The caller
// wires it to a lightweight chat model.
type SummarizeFunc func(ctx context.Context, prompt string) (string, error)

// SmartHistoryConfig tunes the smart compression branch.
type SmartHistoryConfig struct {
	// RecentRounds is the number of newest rounds kept verbatim (uncompressed).
	RecentRounds int
	// ChunkRounds is the map-phase chunk size (rounds per LLM call).
	ChunkRounds int
	// MaxSummaryTokens caps the running summary length.
	MaxSummaryTokens int
}

// DefaultSmartHistoryConfig returns the defaults used when the tenant does
// not override ContextConfig.
func DefaultSmartHistoryConfig() SmartHistoryConfig {
	return SmartHistoryConfig{RecentRounds: 4, ChunkRounds: 6, MaxSummaryTokens: 800}
}

const mapSummaryPrompt = `You are compressing old conversation rounds into a dense running summary.

Rules:
- Keep facts, decisions, deadlines, numbers, user preferences, and unresolved questions.
- Drop chit-chat, filler, and superseded drafts.
- Write in the same language as the conversation.
- Output ONLY the summary, at most %d words.

Conversation rounds:
%s`

const reduceSummaryPrompt = `Merge the existing running summary with the new partial summaries into ONE updated running summary.

Rules:
- Preserve every fact, decision, deadline, number, and user preference.
- Resolve duplicates; keep the newest state when facts changed.
- Same language as the input.
- Output ONLY the merged summary, at most %d words.

Existing running summary:
%s

New partial summaries:
%s`

// SmartCompress splits history into [old | recent], keeps sticky rounds from
// the old part verbatim, map-reduces the rest into the running summary, and
// returns the updated summary plus the rounds to keep as-is.
//
// Incrementality: the caller passes the previous running summary in
// existingSummary together with ONLY the rounds not yet covered by it; the
// merged result is stored back by the caller for the next turn.
func SmartCompress(
	ctx context.Context,
	history []Turn,
	existingSummary string,
	cfg SmartHistoryConfig,
	summarize SummarizeFunc,
	counter *Counter,
) (summary string, kept []Turn, err error) {
	if cfg.RecentRounds <= 0 || cfg.ChunkRounds <= 0 || cfg.MaxSummaryTokens <= 0 {
		d := DefaultSmartHistoryConfig()
		if cfg.RecentRounds <= 0 {
			cfg.RecentRounds = d.RecentRounds
		}
		if cfg.ChunkRounds <= 0 {
			cfg.ChunkRounds = d.ChunkRounds
		}
		if cfg.MaxSummaryTokens <= 0 {
			cfg.MaxSummaryTokens = d.MaxSummaryTokens
		}
	}
	if counter == nil {
		counter = NewCounter(VendorGeneric)
	}

	if len(history) <= cfg.RecentRounds {
		return existingSummary, history, nil
	}

	splitAt := len(history) - cfg.RecentRounds
	old, recent := history[:splitAt], history[splitAt:]

	// Sticky rounds in the old region are kept verbatim, not summarized.
	var sticky, compressible []Turn
	for _, t := range old {
		if t.Sticky || IsStickyTurn(t.User, t.Assistant) {
			t.Sticky = true
			sticky = append(sticky, t)
		} else {
			compressible = append(compressible, t)
		}
	}

	summary = existingSummary
	if len(compressible) > 0 && summarize != nil {
		// Map phase: chunk old rounds, summarize each chunk.
		var partials []string
		for i := 0; i < len(compressible); i += cfg.ChunkRounds {
			end := i + cfg.ChunkRounds
			if end > len(compressible) {
				end = len(compressible)
			}
			var b strings.Builder
			for _, t := range compressible[i:end] {
				b.WriteString("User: " + t.User + "\nAssistant: " + t.Assistant + "\n\n")
			}
			part, mapErr := summarize(ctx, fmt.Sprintf(mapSummaryPrompt, cfg.MaxSummaryTokens/2, b.String()))
			if mapErr != nil {
				// Best effort: on LLM failure keep the raw rounds rather than
				// losing information.
				return existingSummary, history, mapErr
			}
			part = strings.TrimSpace(part)
			if part != "" {
				partials = append(partials, part)
			}
		}

		// Reduce phase: merge partials into the running summary.
		if len(partials) > 0 {
			merged, redErr := summarize(ctx, fmt.Sprintf(reduceSummaryPrompt,
				cfg.MaxSummaryTokens, existingSummary, strings.Join(partials, "\n---\n")))
			if redErr != nil {
				return existingSummary, history, redErr
			}
			if merged = strings.TrimSpace(merged); merged != "" {
				summary = merged
			}
		}
	}

	// Cap runaway summaries.
	if counter.Count(summary) > cfg.MaxSummaryTokens {
		summary = TruncateToTokens(summary, cfg.MaxSummaryTokens, counter)
	}

	// Final order: sticky rounds (chronological) then recent rounds.
	kept = append(kept, sticky...)
	kept = append(kept, recent...)
	return summary, kept, nil
}
