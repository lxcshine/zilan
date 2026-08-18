package types

import (
	"database/sql/driver"
	"encoding/json"
)

// RetrievalConfig holds the global retrieval/search configuration for a tenant.
// This replaces the retrieval-related fields previously scattered in ConversationConfig
// and ChatHistoryConfig. Both knowledge search and message search share these parameters.
//
// Stored as a JSONB column on the tenants table, managed via the settings UI
// at /tenants/kv/retrieval-config.
type RetrievalConfig struct {
	// EmbeddingTopK is the maximum number of chunks returned by vector search (default: 50)
	EmbeddingTopK int `json:"embedding_top_k"`
	// VectorThreshold is the minimum vector similarity score (0-1, default: 0.15)
	VectorThreshold float64 `json:"vector_threshold"`
	// KeywordThreshold is the minimum keyword match score (0-1, default: 0.3)
	KeywordThreshold float64 `json:"keyword_threshold"`
	// RerankTopK is the maximum number of results after reranking (default: 10)
	RerankTopK int `json:"rerank_top_k"`
	// RerankThreshold is the minimum rerank score (-10 to 10, default: 0.2)
	RerankThreshold float64 `json:"rerank_threshold"`
	// RerankModelID is the ID of the rerank model to use (required for search)
	RerankModelID string `json:"rerank_model_id"`

	// RRFK is the smoothing constant of Reciprocal Rank Fusion. Larger values
	// flatten the curve, reducing the bias towards top-1 results.
	// Default: 60. Sensible range: 30..100 depending on corpus size.
	RRFK int `json:"rrf_k,omitempty"`
	// RRFVectorWeight is the weight applied to the vector retriever inside RRF.
	// RRFVectorWeight + RRFKeywordWeight should usually sum to 1.0 but the math
	// works for any positive weights. Default: 0.7.
	RRFVectorWeight float64 `json:"rrf_vector_weight,omitempty"`
	// RRFKeywordWeight is the keyword counterpart. Default: 0.3.
	RRFKeywordWeight float64 `json:"rrf_keyword_weight,omitempty"`

	// --- Multi-channel fusion (ima-grade retrieval) ---

	// EnableIntentRouting turns on the ROUTE_RETRIEVAL stage: fine-grained
	// intent classification (factual/summary/comparison/reasoning/exploratory)
	// with per-intent retrieval strategies. Default: true.
	EnableIntentRouting *bool `json:"enable_intent_routing,omitempty"`
	// EnableQueryEnhancement turns on the QUERY_ENHANCE stage (HyDE,
	// multi-query expansion, step-back prompting). Per-intent profiles still
	// decide which of the three techniques actually run. Default: true.
	EnableQueryEnhancement *bool `json:"enable_query_enhancement,omitempty"`
	// EnableLearnedFusion blends the online-learned channel weights
	// (ChannelWeights, updated by retrieval feedback) into the static
	// profile weights. Default: true when ChannelWeights has data.
	EnableLearnedFusion *bool `json:"enable_learned_fusion,omitempty"`
	// EnableLateInteraction turns on the token-level late-interaction
	// re-scorer applied after model reranking. Default: true.
	EnableLateInteraction *bool `json:"enable_late_interaction,omitempty"`

	// ChannelWeights holds learned/admin-tuned RRF weights per
	// (intent, channel). Outer key: RetrievalIntent value ("factual", ...);
	// "*" applies to all intents. Inner key: channel name (vector/keyword/...).
	// Updated online by the retrieval-feedback loop (Hedge algorithm) and
	// editable by tenant admins. Values blend into DefaultChannelWeights.
	ChannelWeights map[string]map[string]float64 `json:"channel_weights,omitempty"`

	// IntentProfiles holds tenant-level per-intent strategy overrides.
	// Key: RetrievalIntent value. Non-zero fields override the built-in
	// defaults from DefaultIntentProfiles().
	IntentProfiles map[string]*IntentRetrievalProfile `json:"intent_profiles,omitempty"`

	// FeedbackLearnRate is the Hedge update step size for learned fusion.
	// Default: 0.15. Sensible range: 0.05..0.4.
	FeedbackLearnRate float64 `json:"feedback_learn_rate,omitempty"`

	// KBClassProfiles maps an auto-detected KB class ("paper", "manual",
	// "faq", "regulation", "general") to retrieval parameter presets applied
	// when every KB in scope shares that class and the intent profile does
	// not already override the same field. Managed by the profiling job.
	KBClassProfiles map[string]*IntentRetrievalProfile `json:"kb_class_profiles,omitempty"`
}

// GetEffectiveEmbeddingTopK returns EmbeddingTopK with a fallback default.
func (c *RetrievalConfig) GetEffectiveEmbeddingTopK() int {
	if c == nil || c.EmbeddingTopK <= 0 {
		return 50
	}
	return c.EmbeddingTopK
}

// GetEffectiveVectorThreshold returns VectorThreshold with a fallback default.
func (c *RetrievalConfig) GetEffectiveVectorThreshold() float64 {
	if c == nil || c.VectorThreshold <= 0 {
		return 0.15
	}
	return c.VectorThreshold
}

// GetEffectiveKeywordThreshold returns KeywordThreshold with a fallback default.
func (c *RetrievalConfig) GetEffectiveKeywordThreshold() float64 {
	if c == nil || c.KeywordThreshold <= 0 {
		return 0.3
	}
	return c.KeywordThreshold
}

// GetEffectiveRerankTopK returns RerankTopK with a fallback default.
func (c *RetrievalConfig) GetEffectiveRerankTopK() int {
	if c == nil || c.RerankTopK <= 0 {
		return 10
	}
	return c.RerankTopK
}

// GetEffectiveRerankThreshold returns RerankThreshold with a fallback default.
func (c *RetrievalConfig) GetEffectiveRerankThreshold() float64 {
	if c == nil {
		return 0.2
	}
	return c.RerankThreshold
}

// GetEffectiveRRFK returns the RRF smoothing constant with a fallback default.
func (c *RetrievalConfig) GetEffectiveRRFK() int {
	if c == nil || c.RRFK <= 0 {
		return 60
	}
	return c.RRFK
}

// GetEffectiveRRFWeights returns vector / keyword weights with sensible defaults.
// When neither weight is set explicitly, returns 0.7 / 0.3.
func (c *RetrievalConfig) GetEffectiveRRFWeights() (vector, keyword float64) {
	if c == nil || (c.RRFVectorWeight == 0 && c.RRFKeywordWeight == 0) {
		return 0.7, 0.3
	}
	v := c.RRFVectorWeight
	k := c.RRFKeywordWeight
	if v <= 0 {
		v = 0.7
	}
	if k <= 0 {
		k = 0.3
	}
	return v, k
}

// boolDefault resolves an optional bool flag with a default.
func boolDefault(ptr *bool, def bool) bool {
	if ptr == nil {
		return def
	}
	return *ptr
}

// IsIntentRoutingEnabled reports whether fine-grained intent routing runs.
func (c *RetrievalConfig) IsIntentRoutingEnabled() bool {
	return boolDefault(c.GetSelf().EnableIntentRouting, true)
}

// IsQueryEnhancementEnabled reports whether the query-enhancement stage runs.
func (c *RetrievalConfig) IsQueryEnhancementEnabled() bool {
	return boolDefault(c.GetSelf().EnableQueryEnhancement, true)
}

// IsLearnedFusionEnabled reports whether learned channel weights blend in.
func (c *RetrievalConfig) IsLearnedFusionEnabled() bool {
	return boolDefault(c.GetSelf().EnableLearnedFusion, true)
}

// IsLateInteractionEnabled reports whether the late-interaction re-scorer runs.
func (c *RetrievalConfig) IsLateInteractionEnabled() bool {
	return boolDefault(c.GetSelf().EnableLateInteraction, true)
}

// GetSelf handles nil receivers so the Is* helpers can be chained safely.
func (c *RetrievalConfig) GetSelf() *RetrievalConfig {
	if c == nil {
		return &RetrievalConfig{}
	}
	return c
}

// GetEffectiveChannelWeights returns the channel weight overrides for an
// intent: wildcard "*" weights merged with intent-specific weights (the latter
// winning). Returns nil when no overrides exist.
func (c *RetrievalConfig) GetEffectiveChannelWeights(intent string) map[string]float64 {
	if c == nil || len(c.ChannelWeights) == 0 {
		return nil
	}
	if !c.IsLearnedFusionEnabled() {
		return nil
	}
	out := make(map[string]float64)
	for ch, w := range c.ChannelWeights["*"] {
		if w > 0 {
			out[ch] = w
		}
	}
	if intent != "" {
		for ch, w := range c.ChannelWeights[intent] {
			if w > 0 {
				out[ch] = w
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// GetEffectiveFeedbackLearnRate returns the Hedge learning rate (default 0.15).
func (c *RetrievalConfig) GetEffectiveFeedbackLearnRate() float64 {
	if c == nil || c.FeedbackLearnRate <= 0 {
		return 0.15
	}
	return c.FeedbackLearnRate
}

// Clone returns a deep copy of the RetrievalConfig.
func (c *RetrievalConfig) Clone() *RetrievalConfig {
	if c == nil {
		return nil
	}
	out := *c
	if c.ChannelWeights != nil {
		out.ChannelWeights = make(map[string]map[string]float64, len(c.ChannelWeights))
		for k, v := range c.ChannelWeights {
			inner := make(map[string]float64, len(v))
			for ch, w := range v {
				inner[ch] = w
			}
			out.ChannelWeights[k] = inner
		}
	}
	if c.IntentProfiles != nil {
		out.IntentProfiles = make(map[string]*IntentRetrievalProfile, len(c.IntentProfiles))
		for k, v := range c.IntentProfiles {
			out.IntentProfiles[k] = v.Clone()
		}
	}
	if c.KBClassProfiles != nil {
		out.KBClassProfiles = make(map[string]*IntentRetrievalProfile, len(c.KBClassProfiles))
		for k, v := range c.KBClassProfiles {
			out.KBClassProfiles[k] = v.Clone()
		}
	}
	return &out
}

// Value implements the driver.Valuer interface for database serialization
func (c RetrievalConfig) Value() (driver.Value, error) {
	return json.Marshal(c)
}

// Scan implements the sql.Scanner interface for database deserialization
func (c *RetrievalConfig) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	b, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(b, c)
}
