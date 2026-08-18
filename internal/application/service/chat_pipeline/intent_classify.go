package chatpipeline

import (
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// Rule-based fast-path classifier for RetrievalIntent.
//
// Design goals (ima-grade):
//   - Zero latency / zero cost for obvious cases (summary verbs, "A vs B",
//     reasoning markers). These patterns cover the majority of production
//     traffic in Chinese + English knowledge-base Q&A.
//   - Conservative: returns ("", false) whenever the signal is ambiguous, in
//     which case the LLM classifier inside query-understand decides, and the
//     factual default applies if the LLM is unavailable.
//   - Pure function over the (rewritten) query text — no I/O, safe to run on
//     every request.

// summarySignals mark requests to summarize / overview / digest content.
var summarySignals = regexp.MustCompile(
	`总结|概括|摘要|综述|概述|归纳|整理一下|梳理|讲了什么|主要内容|大意|要点|划重点|` +
		`(?i)\b(summari[sz]e|summary|overview|outline|recap|digest|tl;?dr|key\s+points?|main\s+ideas?)\b`,
)

// reasoningSignals mark why/how/analyze/explain causal or analytical requests.
var reasoningSignals = regexp.MustCompile(
	`为什么|为何|原因|原理|机制|怎么(做到|实现|工作)|如何(实现|工作|做到)|分析一下|分析|解读|评价|优缺点|影响|意义|` +
		`(?i)\b(why|reason|analyze|analysis|analyse|explain|mechanism|principle|pros\s+and\s+cons|impact|implications?)\b`,
)

// comparisonSignals mark explicit A-vs-B comparison requests. The strongest
// signal is the presence of comparative connectives between two entities.
var comparisonSignals = regexp.MustCompile(
	`对比|比较|相比|哪个好|哪家好|有何区别|有什么区别|差异|优劣|孰优孰劣|还是.{1,30}好|` +
		`(?i)\b(vs\.?|versus|compare|comparison|difference(s)?\s+between|better\s+than|which\s+is\s+better|pros\s+and\s+cons\s+of)\b`,
)

// comparisonSplitPattern splits "A和B / A与B / A vs B" into comparison sides.
var comparisonSplitPattern = regexp.MustCompile(
	`(?i)\s*(?:和|与|跟|及|vs\.?|versus|和。。|,\s*and\s+|\s+and\s+)\s*`,
)

// factualSignals mark canonical fact-lookup questions. These short-circuit to
// the factual profile even when other weak signals are present.
var factualSignals = regexp.MustCompile(
	`^(什么是|什么是|谁|谁写的|什么时候|何时|哪里|哪个|多少|几个)|` +
		`(?i)\b(what\s+is|who\s+is|when\s+did|where\s+is|how\s+much|how\s+many|define|definition\s+of)\b`,
)

// exploratorySignals mark vague open-ended browsing ("给我讲讲", "介绍一下").
var exploratorySignals = regexp.MustCompile(
	`介绍一下|讲讲|说说|聊一聊|给我讲讲|随便聊聊|了解一下|` +
		`(?i)\b(tell\s+me\s+about|walk\s+me\s+through|introduce|give\s+me\s+an\s+overview\s+of)\b`,
)

// classifyRetrievalIntentByRules runs the deterministic fast path.
// Returns (intent, true) on a confident classification, ("", false) otherwise.
func classifyRetrievalIntentByRules(query string) (types.RetrievalIntent, bool) {
	q := strings.TrimSpace(query)
	if q == "" {
		return "", false
	}

	comparison := comparisonSignals.MatchString(q)
	summary := summarySignals.MatchString(q)
	reasoning := reasoningSignals.MatchString(q)
	factual := factualSignals.MatchString(q)
	exploratory := exploratorySignals.MatchString(q)

	// Priority order resolves multi-signal queries:
	//   comparison > summary > reasoning > exploratory > factual.
	// Comparison wins because "对比A和B的优缺点" also matches reasoning markers;
	// summary beats reasoning because "总结...的原理" is still a summary task.
	switch {
	case comparison:
		return types.RetrievalIntentComparison, true
	case summary:
		return types.RetrievalIntentSummary, true
	case reasoning && !factual:
		return types.RetrievalIntentReasoning, true
	case exploratory:
		return types.RetrievalIntentExploratory, true
	case factual:
		return types.RetrievalIntentFactual, true
	}
	return "", false
}

// extractComparisonEntities splits a comparison query into its A/B sides.
// Returns nil when fewer than two plausible entities are found — the
// decomposition step then falls back to the entity extractor's output.
func extractComparisonEntities(query string) []string {
	q := strings.TrimSpace(query)
	// Strip leading question scaffolding so the split operates on entities.
	q = questionWords.ReplaceAllString(q, "")
	q = strings.Trim(q, " 。？?！!")

	parts := comparisonSplitPattern.Split(q, -1)
	entities := make([]string, 0, 2)
	seen := make(map[string]struct{})
	for _, p := range parts {
		p = strings.TrimSpace(p)
		// Drop trailing comparison scaffolding ("哪个好", "的区别" etc.)
		p = comparisonSignals.ReplaceAllString(p, "")
		p = strings.Trim(p, " 。？?！!，,；;：:")
		if p == "" || len([]rune(p)) > 40 {
			continue
		}
		key := strings.ToLower(p)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		entities = append(entities, p)
	}
	if len(entities) < 2 {
		return nil
	}
	// Comparison decomposition targets exactly two sides; extra fragments are
	// almost always scaffolding noise, so cap at the first two.
	if len(entities) > 2 {
		entities = entities[:2]
	}
	return entities
}
