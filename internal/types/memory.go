package types

import (
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Memory categories (L3 long-term memory).
const (
	// MemoryCategoryProfile is a stable user attribute (name, role, org).
	MemoryCategoryProfile = "profile"
	// MemoryCategoryFact is a world/user fact triple, e.g. (user,负责,ProjectX).
	MemoryCategoryFact = "fact"
	// MemoryCategoryPreference captures likes/dislikes and style preferences.
	MemoryCategoryPreference = "preference"
	// MemoryCategoryTodo is an open task with an optional deadline (DueAt).
	MemoryCategoryTodo = "todo"
	// MemoryCategoryFeedback records explicit positive/negative feedback on
	// answers, used to tune future answer style.
	MemoryCategoryFeedback = "feedback"
	// MemoryCategorySoul is a user directive about the assistant's own
	// behavior: how to address the user, reply language, tone, format,
	// verbosity ("以后叫我张工"). Injected as a style layer that overrides
	// the default persona.
	MemoryCategorySoul = "soul"
	// MemoryCategorySkill is a behavioral lesson the assistant distilled
	// from explicit user feedback or instructions ("回答先给结论再展开").
	// The subject of a skill fact is always "assistant".
	MemoryCategorySkill = "skill"
)

// Memory modules (four-module memory architecture, P0-2).
const (
	// MemoryModuleSoul aggregates the assistant persona: the global persona
	// template plus user soul directives.
	MemoryModuleSoul = "soul"
	// MemoryModuleUser aggregates the structured user profile (identity,
	// role, preference, fact).
	MemoryModuleUser = "user"
	// MemoryModuleMemory is the full memory stream (all facts + L2 session
	// summaries), the default module view.
	MemoryModuleMemory = "memory"
	// MemoryModuleAgent aggregates reusable answer strategies: skills
	// distilled from feedback.
	MemoryModuleAgent = "agent"
)

// MemoryModuleOf maps one memory category to its owning module. This is the
// single source of truth shared by the backend aggregation endpoints and the
// frontend navigation (exposed via GET /memory/modules).
func MemoryModuleOf(category string) string {
	switch category {
	case MemoryCategorySoul:
		return MemoryModuleSoul
	case MemoryCategoryProfile, MemoryCategoryFact, MemoryCategoryPreference:
		return MemoryModuleUser
	case MemoryCategorySkill, MemoryCategoryFeedback:
		return MemoryModuleAgent
	default:
		// todo and anything else lives in the memory stream.
		return MemoryModuleMemory
	}
}

// MemoryProfileSectionKey identifies one section of the User profile card.
const (
	MemoryProfileSectionIdentity   = "identity"   // stable attributes (category=profile)
	MemoryProfileSectionRole       = "role"       // duties/responsibilities (role-shaped facts)
	MemoryProfileSectionPreference = "preference" // likes/dislikes (category=preference)
	MemoryProfileSectionFact       = "fact"       // remaining facts
)

// rolePredicateKeywords marks fact triples that describe what the user is
// responsible for, so they surface in the profile "role" section instead of
// being buried in the generic fact list.
var rolePredicateKeywords = []string{
	"负责", "主导", "参与", "管理", "开发", "维护", "运营", "任职", "担任", "工作",
	"responsible", "lead", "manage", "develop", "maintain", "operate", "work",
	"owner", "member",
}

// MemoryProfileSectionOf maps one fact to its User-profile section key.
func MemoryProfileSectionOf(fact *MemoryFact) string {
	if fact == nil {
		return MemoryProfileSectionFact
	}
	switch fact.Category {
	case MemoryCategoryProfile:
		return MemoryProfileSectionIdentity
	case MemoryCategoryPreference:
		return MemoryProfileSectionPreference
	case MemoryCategoryFact:
		p := strings.ToLower(fact.Predicate)
		for _, kw := range rolePredicateKeywords {
			if strings.Contains(p, kw) {
				return MemoryProfileSectionRole
			}
		}
		return MemoryProfileSectionFact
	default:
		return MemoryProfileSectionFact
	}
}

// Memory fact statuses.
const (
	MemoryStatusActive   = "active"
	MemoryStatusDone     = "done"     // todos only: completed
	MemoryStatusArchived = "archived" // hidden from recall, kept for audit
)

// Memory recall tuning defaults. The half-lives control the exponential time
// decay factor of the recall score; MaxFactsPerUser bounds the per-user memory
// set so brute-force cosine over candidates stays cheap and importance-based
// eviction can kick in.
const (
	// DefaultFactHalfLife is the default decay half-life for L3 facts.
	DefaultFactHalfLife = 30 * 24 * time.Hour
	// DefaultSessionSummaryHalfLife decays L2 session summaries faster.
	DefaultSessionSummaryHalfLife = 7 * 24 * time.Hour
	// DefaultMemoryRecallLimit is the default top-k of injected memories.
	DefaultMemoryRecallLimit = 8
	// MaxFactsPerUser bounds active facts per (tenant,user); the service
	// evicts the lowest-importance facts beyond this cap.
	MaxFactsPerUser = 2000
	// MemoryRecallCandidateLimit caps how many candidate rows the repository
	// returns for in-Go rescoring.
	MemoryRecallCandidateLimit = 200
)

// VectorBlob stores an embedding vector as a JSON array so the same column
// works on Postgres (jsonb) and SQLite (TEXT). Per-user memory sets are
// bounded (MaxFactsPerUser), so recall scores candidates in Go instead of
// relying on a vector index.
type VectorBlob []float32

// Value implements driver.Valuer (JSON array).
func (v VectorBlob) Value() (driver.Value, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// Scan implements sql.Scanner (JSON array).
func (v *VectorBlob) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	var b []byte
	switch t := value.(type) {
	case []byte:
		b = t
	case string:
		b = []byte(t)
	default:
		return nil
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// Cosine returns the cosine similarity between two vectors; 0 when either is
// empty, mismatched in dimension, or zero-norm.
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		na += fa * fa
		nb += fb * fb
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// MemoryFact is one L3 long-term memory row: a fact triple, a profile
// attribute, a preference, a todo, or answer feedback.
type MemoryFact struct {
	ID         string `json:"id"         gorm:"type:varchar(36);primaryKey"`
	TenantID   uint64 `json:"tenant_id"  gorm:"index"`
	UserID     string `json:"user_id"    gorm:"type:varchar(512)"`
	SessionID  string `json:"session_id" gorm:"type:varchar(36);index"`
	MessageID  string `json:"message_id" gorm:"type:varchar(36)"`
	Category   string `json:"category"   gorm:"type:varchar(32);index"`
	Subject    string `json:"subject"`
	Predicate  string `json:"predicate"`
	Object     string `json:"object"`
	TripleHash string `json:"-" gorm:"type:varchar(64)"`
	// Content is the canonical human-readable rendering ("用户负责 Project X"),
	// used as the embedding input and for prompt injection / display.
	Content        string     `json:"content"`
	Confidence     float64    `json:"confidence"`
	Importance     float64    `json:"importance"`
	Status         string     `json:"status" gorm:"type:varchar(16)"`
	AccessCount    int        `json:"access_count"`
	LastAccessedAt *time.Time `json:"last_accessed_at,omitempty"`
	// DueAt is the deadline for todo-category facts; nil otherwise.
	DueAt     *time.Time `json:"due_at,omitempty"`
	Embedding VectorBlob `json:"-"    gorm:"type:jsonb"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

// TableName sets the GORM table.
func (MemoryFact) TableName() string { return "memory_facts" }

// BeforeCreate assigns a UUID primary key.
func (m *MemoryFact) BeforeCreate(tx *gorm.DB) (err error) {
	if m.ID == "" {
		m.ID = uuid.New().String()
	}
	return nil
}

// ComputeTripleHash returns the deterministic dedup key for a fact:
// sha256(category|subject|predicate|object) truncated to 32 hex chars.
// Extraction re-seeing the same fact must map to the same hash so the service
// can upsert (refresh confidence/access) instead of duplicating rows.
func ComputeTripleHash(category, subject, predicate, object string) string {
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	sum := sha256.Sum256([]byte(norm(category) + "|" + norm(subject) + "|" + norm(predicate) + "|" + norm(object)))
	return hex.EncodeToString(sum[:16])
}

// MemorySessionSummary is the L2 short-term memory row for one session: a
// rolling LLM summary plus key topics, recalled with semantic similarity and
// time decay.
type MemorySessionSummary struct {
	ID            string      `json:"id"         gorm:"type:varchar(36);primaryKey"`
	TenantID      uint64      `json:"tenant_id"  gorm:"index"`
	UserID        string      `json:"user_id"    gorm:"type:varchar(512)"`
	SessionID     string      `json:"session_id" gorm:"type:varchar(36);index"`
	Title         string      `json:"title"`
	Summary       string      `json:"summary"`
	KeyTopics     StringArray `json:"key_topics,omitempty" gorm:"type:jsonb"`
	MessageCount  int         `json:"message_count"`
	Embedding     VectorBlob  `json:"-"           gorm:"type:jsonb"`
	LastMessageAt *time.Time  `json:"last_message_at,omitempty"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

// TableName sets the GORM table.
func (MemorySessionSummary) TableName() string { return "memory_session_summaries" }

// BeforeCreate assigns a UUID primary key.
func (m *MemorySessionSummary) BeforeCreate(tx *gorm.DB) (err error) {
	if m.ID == "" {
		m.ID = uuid.New().String()
	}
	return nil
}

// MemoryFactQuery filters the user-facing memory listing ("AI 记住了我什么").
type MemoryFactQuery struct {
	Category string // empty = all categories
	Status   string // empty = active only; "all" = every status
	Keyword  string // ILIKE on content
	Page     int
	PageSize int
}

// MemoryRecallParams controls L2+L3 recall for prompt injection.
type MemoryRecallParams struct {
	Query           string
	QueryEmbedding  []float32 // pre-computed by the caller (embedder may be async)
	Limit           int       // 0 -> DefaultMemoryRecallLimit
	Categories      []string  // empty = all
	FactHalfLife    time.Duration
	SummaryHalfLife time.Duration
	Now             time.Time // injectable for tests; zero -> time.Now()
}

// RecalledMemory is one scored memory ready for prompt injection.
type RecalledMemory struct {
	Kind    string                `json:"kind"` // "fact" | "session_summary"
	Fact    *MemoryFact           `json:"fact,omitempty"`
	Summary *MemorySessionSummary `json:"summary,omitempty"`
	Score   float64               `json:"score"`
}

// MemoryExtractPayload is the asynq payload for TypeMemoryExtract. One task
// covers one completed QA turn: L3 fact extraction plus the L2 rolling
// session summary refresh.
type MemoryExtractPayload struct {
	TracingContext
	TenantID  uint64 `json:"tenant_id"`
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	// UserMessageID / AssistantMessageID identify the completed turn.
	UserMessageID      string `json:"user_message_id"`
	AssistantMessageID string `json:"assistant_message_id"`
	// SummaryModelID is the chat model used for extraction (the turn's
	// summary model); empty falls back to the tenant default KnowledgeQA model.
	SummaryModelID string `json:"summary_model_id,omitempty"`
	// EmbeddingModelID for fact/summary embeddings; empty falls back to the
	// tenant default Embedding model. When no embedding model can be resolved
	// the rows are stored without vectors and recall degrades to
	// recency/importance ordering.
	EmbeddingModelID string `json:"embedding_model_id,omitempty"`
}

// ExtractedMemoryItem is one LLM-extracted memory candidate before upsert.
type ExtractedMemoryItem struct {
	Category   string  `json:"category"`
	Subject    string  `json:"subject"`
	Predicate  string  `json:"predicate"`
	Object     string  `json:"object"`
	Content    string  `json:"content"`
	Confidence float64 `json:"confidence"`
	Importance float64 `json:"importance"`
	DueAt      string  `json:"due_at"` // ISO date/datetime; todos only, may be empty
}

// MemoryExtractionResult is the LLM output envelope for a turn.
type MemoryExtractionResult struct {
	Memories []ExtractedMemoryItem `json:"memories"`
	// SessionSummary refreshes the L2 rolling summary of the session.
	SessionSummary string   `json:"session_summary"`
	KeyTopics      []string `json:"key_topics"`
}

// MemoryRecallScore computes score = semantic x timeDecay x (1 + ln(1+accessCount)).
// timeDecay is an exponential decay on the age of the memory with the given
// half-life, floored at a small epsilon so ancient but highly-relevant
// memories never reach exactly zero.
func MemoryRecallScore(semantic float64, reference time.Time, accessCount int, halfLife time.Duration, now time.Time) float64 {
	if semantic < 0 {
		semantic = 0
	}
	if semantic > 1 {
		semantic = 1
	}
	if halfLife <= 0 {
		halfLife = DefaultFactHalfLife
	}
	age := now.Sub(reference)
	if age < 0 {
		age = 0
	}
	decay := math.Exp(-math.Ln2 * age.Hours() / halfLife.Hours())
	const floor = 0.05
	if decay < floor {
		decay = floor
	}
	if accessCount < 0 {
		accessCount = 0
	}
	frequency := 1 + math.Log1p(float64(accessCount))
	return semantic * decay * frequency
}

// ---------------------------------------------------------------------------
// Four-module aggregation payloads (P0-2)
// ---------------------------------------------------------------------------

// MemoryModuleOverview is one row of GET /memory/modules: the module key plus
// its active fact count, used by the frontend navigation badges. The memory
// module additionally reports the L2 session summary count.
type MemoryModuleOverview struct {
	Module       string `json:"module"`
	FactCount    int64  `json:"fact_count"`
	SummaryCount int64  `json:"summary_count,omitempty"` // memory module only
}

// SoulPersona is the read-only global persona resolved from the system prompt
// template; an empty struct when no template is configured.
type SoulPersona struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Content     string `json:"content,omitempty"`
}

// SoulCard is the GET /memory/soul payload: the global persona plus the
// user's style directives (category=soul).
type SoulCard struct {
	GlobalPersona SoulPersona   `json:"global_persona"`
	Adjustments   []*MemoryFact `json:"adjustments"`
}

// MemoryProfileSection is one grouped card of the User profile.
type MemoryProfileSection struct {
	Key   string        `json:"key"`
	Items []*MemoryFact `json:"items"`
}

// ProfileCard is the GET /memory/profile payload. Completeness is the
// weighted share of non-empty sections (identity/role weigh double).
type ProfileCard struct {
	Sections     []*MemoryProfileSection `json:"sections"`
	Completeness float64                 `json:"completeness"`
}

// AgentFeedbackItem is one raw feedback fact annotated with the skill it was
// upgraded into (matched by extraction turn: same session + message ID).
type AgentFeedbackItem struct {
	*MemoryFact
	UpgradedTo string `json:"upgraded_to,omitempty"`
}

// AgentTipsCard is the GET /memory/agent-tips payload: distilled skills plus
// the raw feedback wall.
type AgentTipsCard struct {
	Skills        []*MemoryFact        `json:"skills"`
	Feedback      []*AgentFeedbackItem `json:"feedback"`
	FeedbackTotal int64                `json:"feedback_total"`
}
