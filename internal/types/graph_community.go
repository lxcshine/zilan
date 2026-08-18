package types

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// GraphRAG tuning defaults. The community set per knowledge base is bounded
// (MaxGraphCommunitiesPerKB), so recall pre-filters by tenant/KB in SQL and
// applies cosine similarity in Go — the same pattern as memory_facts.
const (
	// GraphCommunityMinSize is the minimum number of entities a community
	// must have to be summarized; smaller fragments are graph noise.
	GraphCommunityMinSize = 3
	// MaxGraphCommunitiesPerKB caps how many communities (largest first) are
	// summarized per knowledge base, bounding LLM cost per rebuild.
	MaxGraphCommunitiesPerKB = 32
	// GraphCommunityRecallTopK is the default number of community summaries
	// injected into the chat context per query.
	GraphCommunityRecallTopK = 3
	// GraphCommunityRecallThreshold is the minimum cosine similarity between
	// the query embedding and a community summary embedding for injection.
	GraphCommunityRecallThreshold = 0.25
	// GraphExportMaxNodes bounds the node set exported from Neo4j for one KB
	// during community detection; larger graphs are truncated deterministically.
	GraphExportMaxNodes = 20000
	// GraphSubgraphMaxLevel is the ego-graph expansion depth used by local
	// subgraph retrieval (GraphRAG local search).
	GraphSubgraphMaxLevel = 2
	// GraphSubgraphMaxNodes caps nodes returned by one subgraph expansion.
	GraphSubgraphMaxNodes = 200
)

// GraphCommunity is one detected entity community of a knowledge base graph,
// summarized by an LLM (GraphRAG "community summary"). Communities are
// rebuilt wholesale per KB; CommunityKey is the deterministic hash of the
// sorted member names so a rebuild upserts stable rows instead of churning.
type GraphCommunity struct {
	ID              string `json:"id"               gorm:"type:varchar(36);primaryKey"`
	TenantID        uint64 `json:"tenant_id"        gorm:"index"`
	KnowledgeBaseID string `json:"knowledge_base_id" gorm:"type:varchar(36);index"`
	// CommunityKey is sha256(kbID + sorted member names), truncated to 32 hex
	// chars. Used for upsert-on-rebuild.
	CommunityKey string `json:"-" gorm:"type:varchar(64)"`
	Title        string `json:"title"`
	// Summary is the LLM-generated thematic description of the community;
	// it is the text injected into chat context on recall.
	Summary   string      `json:"summary"`
	NodeNames StringArray `json:"node_names" gorm:"type:jsonb"`
	NodeCount int         `json:"node_count"`
	RelCount  int         `json:"rel_count"`
	// SummaryModelID / EmbeddingModelID record which models produced the row
	// so operators can judge staleness after model switches.
	SummaryModelID   string     `json:"summary_model_id,omitempty"`
	EmbeddingModelID string     `json:"embedding_model_id,omitempty"`
	Embedding        VectorBlob `json:"-" gorm:"type:jsonb"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

// TableName sets the GORM table.
func (GraphCommunity) TableName() string { return "graph_communities" }

// BeforeCreate assigns a UUID primary key.
func (c *GraphCommunity) BeforeCreate(tx *gorm.DB) (err error) {
	if c.ID == "" {
		c.ID = uuid.New().String()
	}
	return nil
}

// ComputeGraphCommunityKey returns the deterministic identity of one
// community within a KB: sha256 over the KB id and the sorted, lower-cased
// member entity names. Rebuilds re-detecting the same member set map to the
// same key so the service can update in place.
func ComputeGraphCommunityKey(kbID string, members []string) string {
	norm := make([]string, 0, len(members))
	for _, m := range members {
		norm = append(norm, strings.ToLower(strings.TrimSpace(m)))
	}
	sort.Strings(norm)
	sum := sha256.Sum256([]byte(kbID + "|" + strings.Join(norm, "|")))
	return hex.EncodeToString(sum[:16])
}

// GraphCommunityBuildPayload is the async task payload for rebuilding one
// knowledge base's community summaries.
type GraphCommunityBuildPayload struct {
	TracingContext
	TenantID         uint64 `json:"tenant_id"`
	KnowledgeBaseID  string `json:"knowledge_base_id"`
	SummaryModelID   string `json:"summary_model_id,omitempty"`
	EmbeddingModelID string `json:"embedding_model_id,omitempty"`
	// Trigger records what caused the rebuild ("ingest" | "manual").
	Trigger string `json:"trigger,omitempty"`
}
