package types

// RetrievalIntent is the fine-grained retrieval-oriented classification of a
// user query. It is a second axis on top of QueryIntent: QueryIntent decides
// WHETHER to retrieve (kb_search vs chitchat...), RetrievalIntent decides HOW
// to retrieve (which channels, thresholds, fusion weights and enhancements).
type RetrievalIntent string

const (
	// RetrievalIntentFactual - who/what/when style fact lookup.
	// Strategy: precision-first vector retrieval, low keyword threshold.
	RetrievalIntentFactual RetrievalIntent = "factual"
	// RetrievalIntentSummary - summarize / overview requests.
	// Strategy: high recall, cross-chunk dedup, document-level evidence boost.
	RetrievalIntentSummary RetrievalIntent = "summary"
	// RetrievalIntentComparison - "A vs B" requests.
	// Strategy: decompose into per-entity sub-queries, then fuse.
	RetrievalIntentComparison RetrievalIntent = "comparison"
	// RetrievalIntentReasoning - why/how/analyze requests.
	// Strategy: multi-document fusion + graph/entity channel boost.
	RetrievalIntentReasoning RetrievalIntent = "reasoning"
	// RetrievalIntentExploratory - vague / open-ended browsing requests.
	// Strategy: max diversity (low MMR lambda), broad channels.
	RetrievalIntentExploratory RetrievalIntent = "exploratory"
)

// DefaultRetrievalIntent is used when classification is unavailable/uncertain.
const DefaultRetrievalIntent = RetrievalIntentFactual

// Valid reports whether the value is a known retrieval intent.
func (r RetrievalIntent) Valid() bool {
	switch r {
	case RetrievalIntentFactual, RetrievalIntentSummary, RetrievalIntentComparison,
		RetrievalIntentReasoning, RetrievalIntentExploratory:
		return true
	}
	return false
}

// Retrieval channel names used by the multi-channel fusion layer. Each channel
// produces an independent ranked list that is fused with weighted RRF.
const (
	// ChannelVector is the dense-embedding channel of the base hybrid search.
	ChannelVector = "vector"
	// ChannelKeyword is the lexical (BM25/tsvector) channel of the base hybrid search.
	ChannelKeyword = "keyword"
	// ChannelSparse is the term-expansion sparse channel (BM25-style re-scored
	// lexical retrieval over LLM/rule-expanded term sets; upgrade path to SPLADE).
	ChannelSparse = "sparse"
	// ChannelHyde is the hypothetical-document-embedding channel.
	ChannelHyde = "hyde"
	// ChannelMultiQuery is the LLM multi-query expansion channel.
	ChannelMultiQuery = "multi_query"
	// ChannelStepBack is the step-back (abstracted query) channel.
	ChannelStepBack = "step_back"
	// ChannelGraph is the entity/graph recall channel.
	ChannelGraph = "graph"
	// ChannelWebSearch is the web search recall channel (renamed from
	// ChannelWeb to avoid collision with the message-ingestion ChannelWeb
	// constant defined in knowledge.go).
	ChannelWebSearch = "web_search"
	// ChannelFAQ is the FAQ direct-match channel.
	ChannelFAQ = "faq"
)

// Knowledge base document classes produced by the automatic KB profiler.
// The profiler samples chunks and measures structural statistics (heading
// density, table ratio, average sentence length, FAQ markers) to classify
// the corpus; retrieval then applies the matching preset from
// DefaultKBClassProfiles unless the tenant overrides KBClassProfiles.
const (
	// KBClassPaper — academic papers: dense headings, references, long sentences.
	KBClassPaper = "paper"
	// KBClassManual — technical manuals/handbooks: hierarchical sections,
	// many lists/tables, medium sentences.
	KBClassManual = "manual"
	// KBClassFAQ — FAQ collections: short Q/A-style chunks, question marks.
	KBClassFAQ = "faq"
	// KBClassRegulation — laws/regulations/contracts: numbered articles,
	// formal long sentences, very low heading density.
	KBClassRegulation = "regulation"
	// KBClassGeneral — fallback when no class signature dominates.
	KBClassGeneral = "general"
)

// IntentRetrievalProfile holds the per-intent retrieval strategy parameters.
// All scale factors multiply the tenant-level RetrievalConfig baselines, so a
// tenant can re-baseline globally without losing per-intent shaping.
type IntentRetrievalProfile struct {
	// VectorThresholdScale multiplies RetrievalConfig.VectorThreshold.
	VectorThresholdScale float64 `json:"vector_threshold_scale,omitempty"`
	// KeywordThresholdScale multiplies RetrievalConfig.KeywordThreshold.
	KeywordThresholdScale float64 `json:"keyword_threshold_scale,omitempty"`
	// TopKScale multiplies RetrievalConfig.EmbeddingTopK.
	TopKScale float64 `json:"top_k_scale,omitempty"`
	// RerankTopKScale multiplies RetrievalConfig.RerankTopK.
	RerankTopKScale float64 `json:"rerank_top_k_scale,omitempty"`

	// ChannelWeights are the RRF weights per retrieval channel for this intent.
	// Missing channels fall back to DefaultChannelWeights().
	ChannelWeights map[string]float64 `json:"channel_weights,omitempty"`

	// MMRLambda is the relevance/diversity trade-off for MMR re-ranking
	// (higher = more relevance, lower = more diversity). 0 = use default.
	MMRLambda float64 `json:"mmr_lambda,omitempty"`

	// Enhancement toggles for the query-enhancement stage.
	EnableHyDE         bool `json:"enable_hyde,omitempty"`
	EnableMultiQuery   bool `json:"enable_multi_query,omitempty"`
	EnableStepBack     bool `json:"enable_step_back,omitempty"`
	EnableSparse       bool `json:"enable_sparse,omitempty"`
	EnableGraphChannel bool `json:"enable_graph_channel,omitempty"`

	// DocEvidenceBoost enables document-level evidence aggregation
	// (chunks of documents with many hits get boosted). Used for summary intent.
	DocEvidenceBoost bool `json:"doc_evidence_boost,omitempty"`
	// ComparisonDecompose enables per-entity sub-query decomposition.
	ComparisonDecompose bool `json:"comparison_decompose,omitempty"`
}

// DefaultChannelWeights returns the baseline RRF channel weights. Weights are
// relative; fusion normalizes by their sum. The base hybrid channels keep the
// historical 0.7/0.3 vector/keyword split.
func DefaultChannelWeights() map[string]float64 {
	return map[string]float64{
		ChannelVector:     0.50,
		ChannelKeyword:    0.22,
		ChannelSparse:     0.10,
		ChannelHyde:       0.08,
		ChannelMultiQuery: 0.05,
		ChannelStepBack:   0.03,
		ChannelGraph:      0.02,
		ChannelWebSearch:  0.0, // web results keep their own scores (not RRF-fused)
	}
}

// DefaultIntentProfiles returns the built-in per-intent strategy table.
// Tenants may override individual fields via RetrievalConfig.IntentProfiles.
func DefaultIntentProfiles() map[RetrievalIntent]*IntentRetrievalProfile {
	return map[RetrievalIntent]*IntentRetrievalProfile{
		RetrievalIntentFactual: {
			VectorThresholdScale:  1.15, // precision-first: raise vector bar
			KeywordThresholdScale: 0.80, // ...but keep lexical recall broad
			TopKScale:             1.0,
			RerankTopKScale:       1.0,
			MMRLambda:             0.80, // relevance over diversity
			ChannelWeights: map[string]float64{
				ChannelVector: 0.55, ChannelKeyword: 0.25, ChannelSparse: 0.10,
				ChannelHyde: 0.05, ChannelMultiQuery: 0.03, ChannelGraph: 0.02,
			},
			EnableMultiQuery: true,
			EnableSparse:     true,
		},
		RetrievalIntentSummary: {
			VectorThresholdScale:  0.70, // recall-first
			KeywordThresholdScale: 0.60,
			TopKScale:             2.0,
			RerankTopKScale:       1.5,
			MMRLambda:             0.55, // diversify across sections
			ChannelWeights: map[string]float64{
				ChannelVector: 0.40, ChannelKeyword: 0.20, ChannelSparse: 0.10,
				ChannelHyde: 0.10, ChannelMultiQuery: 0.10, ChannelStepBack: 0.05, ChannelGraph: 0.05,
			},
			EnableHyDE:           true,
			EnableMultiQuery:     true,
			EnableSparse:         true,
			DocEvidenceBoost:     true,
			EnableGraphChannel:   true,
			ComparisonDecompose:  false,
			EnableStepBack:       true,
		},
		RetrievalIntentComparison: {
			VectorThresholdScale:  1.0,
			KeywordThresholdScale: 0.85,
			TopKScale:             1.5,
			RerankTopKScale:       1.3,
			MMRLambda:             0.60, // force both sides of the comparison
			ChannelWeights: map[string]float64{
				ChannelVector: 0.45, ChannelKeyword: 0.25, ChannelSparse: 0.10,
				ChannelHyde: 0.05, ChannelMultiQuery: 0.08, ChannelGraph: 0.07,
			},
			EnableMultiQuery:     true,
			EnableSparse:         true,
			EnableGraphChannel:   true,
			ComparisonDecompose:  true,
		},
		RetrievalIntentReasoning: {
			VectorThresholdScale:  0.90,
			KeywordThresholdScale: 0.80,
			TopKScale:             1.6,
			RerankTopKScale:       1.3,
			MMRLambda:             0.65,
			ChannelWeights: map[string]float64{
				ChannelVector: 0.42, ChannelKeyword: 0.18, ChannelSparse: 0.08,
				ChannelHyde: 0.10, ChannelMultiQuery: 0.08, ChannelStepBack: 0.06, ChannelGraph: 0.08,
			},
			EnableHyDE:         true,
			EnableMultiQuery:   true,
			EnableStepBack:     true,
			EnableSparse:       true,
			EnableGraphChannel: true,
		},
		RetrievalIntentExploratory: {
			VectorThresholdScale:  0.75,
			KeywordThresholdScale: 0.70,
			TopKScale:             1.8,
			RerankTopKScale:       1.4,
			MMRLambda:             0.45, // max diversity
			ChannelWeights: map[string]float64{
				ChannelVector: 0.40, ChannelKeyword: 0.20, ChannelSparse: 0.12,
				ChannelHyde: 0.10, ChannelMultiQuery: 0.10, ChannelStepBack: 0.04, ChannelGraph: 0.04,
			},
			EnableHyDE:       true,
			EnableMultiQuery: true,
			EnableSparse:     true,
		},
	}
}

// Clone returns a deep copy of the profile (safe for per-request mutation).
func (p *IntentRetrievalProfile) Clone() *IntentRetrievalProfile {
	if p == nil {
		return nil
	}
	out := *p
	if p.ChannelWeights != nil {
		out.ChannelWeights = make(map[string]float64, len(p.ChannelWeights))
		for k, v := range p.ChannelWeights {
			out.ChannelWeights[k] = v
		}
	}
	return &out
}

// DefaultKBClassProfiles returns built-in retrieval presets per auto-detected
// KB document class. Class presets shape corpus-driven recall breadth and are
// blended under the intent profile (which expresses task shape). Tenants may
// override via RetrievalConfig.KBClassProfiles.
//
// Rationale:
//   - paper: long-range semantic flow — favor dense+HyDE, wider top-k for
//     cross-section synthesis.
//   - manual: hierarchical procedures — favor keyword precision (exact feature
//     names matter) with doc-level evidence boost.
//   - faq: short Q/A pairs — very high vector threshold, minimal top-k; the
//     FAQ direct-answer path handles most of the traffic already.
//   - regulation: exact article wording — strong keyword/sparse weighting,
//     low thresholds to avoid missing a single applicable clause.
//   - general: neutral (all scales 1.0).
func DefaultKBClassProfiles() map[string]*IntentRetrievalProfile {
	return map[string]*IntentRetrievalProfile{
		KBClassPaper: {
			VectorThresholdScale:  0.90,
			KeywordThresholdScale: 0.90,
			TopKScale:             1.30,
			RerankTopKScale:       1.20,
			ChannelWeights: map[string]float64{
				ChannelVector: 0.45, ChannelKeyword: 0.18, ChannelSparse: 0.08,
				ChannelHyde: 0.12, ChannelMultiQuery: 0.08, ChannelStepBack: 0.05, ChannelGraph: 0.04,
			},
			EnableHyDE:       true,
			EnableStepBack:   true,
			DocEvidenceBoost: true,
		},
		KBClassManual: {
			VectorThresholdScale:  1.0,
			KeywordThresholdScale: 0.85,
			TopKScale:             1.10,
			RerankTopKScale:       1.0,
			ChannelWeights: map[string]float64{
				ChannelVector: 0.48, ChannelKeyword: 0.28, ChannelSparse: 0.12,
				ChannelHyde: 0.05, ChannelMultiQuery: 0.04, ChannelGraph: 0.03,
			},
			EnableSparse:     true,
			DocEvidenceBoost: true,
		},
		KBClassFAQ: {
			VectorThresholdScale:  1.30,
			KeywordThresholdScale: 1.10,
			TopKScale:             0.60,
			RerankTopKScale:       0.80,
			ChannelWeights: map[string]float64{
				ChannelVector: 0.60, ChannelKeyword: 0.30, ChannelSparse: 0.10,
			},
		},
		KBClassRegulation: {
			VectorThresholdScale:  0.80,
			KeywordThresholdScale: 0.65,
			TopKScale:             1.40,
			RerankTopKScale:       1.30,
			ChannelWeights: map[string]float64{
				ChannelVector: 0.38, ChannelKeyword: 0.30, ChannelSparse: 0.16,
				ChannelHyde: 0.04, ChannelMultiQuery: 0.06, ChannelGraph: 0.06,
			},
			EnableSparse:     true,
			EnableMultiQuery: true,
		},
		KBClassGeneral: {
			VectorThresholdScale:  1.0,
			KeywordThresholdScale: 1.0,
			TopKScale:             1.0,
			RerankTopKScale:       1.0,
		},
	}
}

// RetrievalPlan is the per-request retrieval strategy resolved by the
// ROUTE_RETRIEVAL stage from (intent profile, tenant config, learned weights).
// Plugins downstream read it to parameterize search, fusion and rerank.
type RetrievalPlan struct {
	// Intent is the classified fine-grained retrieval intent.
	Intent RetrievalIntent `json:"intent"`
	// IntentSource records how the intent was decided: "rule", "llm", "default".
	IntentSource string `json:"intent_source,omitempty"`

	// Effective retrieval parameters (tenant baseline x intent scales).
	VectorThreshold  float64 `json:"vector_threshold"`
	KeywordThreshold float64 `json:"keyword_threshold"`
	EmbeddingTopK    int     `json:"embedding_top_k"`
	RerankTopK       int     `json:"rerank_top_k"`

	// ChannelWeights are the final RRF channel weights, after tenant overrides
	// and (optional) learned-weight blending.
	ChannelWeights map[string]float64 `json:"channel_weights,omitempty"`

	// MMRLambda for the post-rerank diversification step.
	MMRLambda float64 `json:"mmr_lambda"`

	// Enhancement decisions for this request.
	UseHyDE         bool `json:"use_hyde"`
	UseMultiQuery   bool `json:"use_multi_query"`
	UseStepBack     bool `json:"use_step_back"`
	UseSparse       bool `json:"use_sparse"`
	UseGraphChannel bool `json:"use_graph_channel"`
	UseDocBoost     bool `json:"use_doc_boost"`
	UseDecompose    bool `json:"use_decompose"`

	// CompareEntities holds the A/B entities extracted for comparison queries.
	CompareEntities []string `json:"compare_entities,omitempty"`
}

// ChannelWeight returns the effective weight for a channel (0 when unknown).
func (p *RetrievalPlan) ChannelWeight(channel string) float64 {
	if p == nil || p.ChannelWeights == nil {
		return 0
	}
	return p.ChannelWeights[channel]
}

// NewDefaultRetrievalPlan builds a plan from the tenant retrieval config with
// the default (factual) profile. Used when routing is skipped or fails.
func NewDefaultRetrievalPlan(cfg *RetrievalConfig) *RetrievalPlan {
	base := &RetrievalPlan{
		Intent:           DefaultRetrievalIntent,
		IntentSource:     "default",
		VectorThreshold:  cfg.GetEffectiveVectorThreshold(),
		KeywordThreshold: cfg.GetEffectiveKeywordThreshold(),
		EmbeddingTopK:    cfg.GetEffectiveEmbeddingTopK(),
		RerankTopK:       cfg.GetEffectiveRerankTopK(),
		ChannelWeights:   DefaultChannelWeights(),
		MMRLambda:        DefaultMMRLambda,
	}
	if p := DefaultIntentProfiles()[DefaultRetrievalIntent]; p != nil {
		base.applyProfile(p, cfg)
	}
	return base
}

// DefaultMMRLambda is the historical MMR trade-off used before per-intent
// profiles existed (see rerank.go applyMMR call site).
const DefaultMMRLambda = 0.7

// applyProfile applies an intent profile on top of tenant baselines.
func (p *RetrievalPlan) applyProfile(profile *IntentRetrievalProfile, cfg *RetrievalConfig) {
	if profile == nil {
		return
	}
	scaleF := func(base, scale float64) float64 {
		if scale <= 0 {
			return base
		}
		return base * scale
	}
	scaleI := func(base int, scale float64) int {
		if scale <= 0 {
			return base
		}
		v := int(float64(base)*scale + 0.5)
		if v < 1 {
			return 1
		}
		return v
	}
	p.VectorThreshold = clamp01(scaleF(p.VectorThreshold, profile.VectorThresholdScale))
	p.KeywordThreshold = clamp01(scaleF(p.KeywordThreshold, profile.KeywordThresholdScale))
	p.EmbeddingTopK = scaleI(p.EmbeddingTopK, profile.TopKScale)
	p.RerankTopK = scaleI(p.RerankTopK, profile.RerankTopKScale)
	if profile.MMRLambda > 0 {
		p.MMRLambda = clamp01(profile.MMRLambda)
	}
	// Channel weights: start from defaults, overlay profile weights.
	merged := DefaultChannelWeights()
	for ch, w := range profile.ChannelWeights {
		merged[ch] = w
	}
	// Tenant-level channel weight overrides (incl. learned weights).
	if cfg != nil {
		for ch, w := range cfg.GetEffectiveChannelWeights(string(p.Intent)) {
			merged[ch] = w
		}
	}
	p.ChannelWeights = merged
	p.UseHyDE = profile.EnableHyDE
	p.UseMultiQuery = profile.EnableMultiQuery
	p.UseStepBack = profile.EnableStepBack
	p.UseSparse = profile.EnableSparse
	p.UseGraphChannel = profile.EnableGraphChannel
	p.UseDocBoost = profile.DocEvidenceBoost
	p.UseDecompose = profile.ComparisonDecompose
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ResolveRetrievalPlan builds the effective plan for a request:
// tenant baseline -> default profile for intent -> tenant per-intent overrides.
func ResolveRetrievalPlan(cfg *RetrievalConfig, intent RetrievalIntent, source string) *RetrievalPlan {
	if !intent.Valid() {
		intent = DefaultRetrievalIntent
	}
	plan := &RetrievalPlan{
		Intent:           intent,
		IntentSource:     source,
		VectorThreshold:  cfg.GetEffectiveVectorThreshold(),
		KeywordThreshold: cfg.GetEffectiveKeywordThreshold(),
		EmbeddingTopK:    cfg.GetEffectiveEmbeddingTopK(),
		RerankTopK:       cfg.GetEffectiveRerankTopK(),
		ChannelWeights:   DefaultChannelWeights(),
		MMRLambda:        DefaultMMRLambda,
	}
	profile := DefaultIntentProfiles()[intent]
	// Tenant per-intent overrides win over built-in defaults.
	if cfg != nil && cfg.IntentProfiles != nil {
		if override, ok := cfg.IntentProfiles[string(intent)]; ok && override != nil {
			merged := profile.Clone()
			mergeProfile(merged, override)
			profile = merged
		}
	}
	plan.applyProfile(profile, cfg)
	return plan
}

// mergeProfile overlays non-zero fields of override onto base.
func mergeProfile(base, override *IntentRetrievalProfile) {
	if base == nil || override == nil {
		return
	}
	if override.VectorThresholdScale > 0 {
		base.VectorThresholdScale = override.VectorThresholdScale
	}
	if override.KeywordThresholdScale > 0 {
		base.KeywordThresholdScale = override.KeywordThresholdScale
	}
	if override.TopKScale > 0 {
		base.TopKScale = override.TopKScale
	}
	if override.RerankTopKScale > 0 {
		base.RerankTopKScale = override.RerankTopKScale
	}
	if override.MMRLambda > 0 {
		base.MMRLambda = override.MMRLambda
	}
	for ch, w := range override.ChannelWeights {
		if base.ChannelWeights == nil {
			base.ChannelWeights = map[string]float64{}
		}
		base.ChannelWeights[ch] = w
	}
	// Booleans are tri-state in effect: override only when it turns a feature on
	// OR explicitly turns it off while base has it on. Since Go bools cannot
	// distinguish unset from false in JSON, tenant overrides treat "true" as
	// enable and leave false as "no opinion" unless base is true and the
	// tenant explicitly disables via channel weights. Keep it simple: OR-merge.
	base.EnableHyDE = base.EnableHyDE || override.EnableHyDE
	base.EnableMultiQuery = base.EnableMultiQuery || override.EnableMultiQuery
	base.EnableStepBack = base.EnableStepBack || override.EnableStepBack
	base.EnableSparse = base.EnableSparse || override.EnableSparse
	base.EnableGraphChannel = base.EnableGraphChannel || override.EnableGraphChannel
	base.DocEvidenceBoost = base.DocEvidenceBoost || override.DocEvidenceBoost
	base.ComparisonDecompose = base.ComparisonDecompose || override.ComparisonDecompose
}
