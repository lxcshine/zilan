package types

// Knowledge precipitation (外化记忆, 4.4): high-value conversations are
// distilled into durable knowledge artifacts — a curated summary document in
// a hidden per-tenant knowledge base, and (on demand) an interlinked wiki
// page. Both carry a `session:<id>` provenance marker so the
// "对话→知识→对话" loop stays traceable and retractable.

const (
	// SessionPrecipitationKBName is the well-known name of the hidden,
	// auto-managed per-tenant knowledge base that holds precipitated session
	// documents. The `__...__` convention plus IsTemporary keeps it out of
	// user-facing KB listings (same mechanism as __chat_history__).
	SessionPrecipitationKBName = "__session_insights__"

	// KnowledgeSourceSessionPrefix prefixes Knowledge.Source for documents
	// precipitated from a conversation: "session:<session_id>". It lets the
	// precipitation worker find (and refresh) the existing document for a
	// session instead of duplicating it on every turn.
	KnowledgeSourceSessionPrefix = "session:"

	// WikiSlugSessionPrefix namespaces wiki pages generated from a
	// conversation: "session/<session_id>". Deterministic slugs make the
	// one-click "整理为 Wiki" action idempotent — a second click refreshes
	// the existing page instead of failing on the unique index.
	WikiSlugSessionPrefix = "session/"

	// SessionPrecipitationChannel is the ingestion channel recorded on
	// precipitated knowledge documents.
	SessionPrecipitationChannel = "session"
)

// High-value session detection thresholds. A session is precipitated when
// the user explicitly favorites it OR the conversation shows sustained
// follow-up depth (message count reaches the threshold).
const (
	// DefaultPrecipitateMessageThreshold bounds the follow-up signal: at
	// least this many messages (user + assistant) before an unfavorited
	// session counts as high-value. 10 messages ≈ 5 QA rounds.
	DefaultPrecipitateMessageThreshold = 10
)

// SessionPrecipitatePayload is the asynq payload for TypeSessionPrecipitate.
// Enqueued by the memory-extract worker once a session crosses the
// high-value bar; processed off the interactive path.
type SessionPrecipitatePayload struct {
	TracingContext
	TenantID  uint64 `json:"tenant_id"`
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	// SummaryModelID is the chat model used for distillation; empty falls
	// back to the tenant default KnowledgeQA model.
	SummaryModelID string `json:"summary_model_id,omitempty"`
	// Trigger records why the session qualified ("favorite" | "followups"),
	// purely for observability.
	Trigger string `json:"trigger,omitempty"`
}

// SessionWikiRequest is the body of the one-click "将本次对话整理为 Wiki"
// endpoint.
type SessionWikiRequest struct {
	// KnowledgeBaseID is the target wiki KB. Required.
	KnowledgeBaseID string `json:"knowledge_base_id" binding:"required"`
	// Title optionally overrides the LLM-generated page title.
	Title string `json:"title,omitempty"`
}

// SessionWikiArticle is the LLM output envelope for session→wiki
// distillation: a title, a one-line summary for index listings, and the
// markdown body.
type SessionWikiArticle struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Content string `json:"content"`
}

// SessionKnowledgeSource returns the Knowledge.Source provenance marker for
// a precipitated session document.
func SessionKnowledgeSource(sessionID string) string {
	return KnowledgeSourceSessionPrefix + sessionID
}

// SessionWikiSlug returns the deterministic wiki slug for a session-derived
// page.
func SessionWikiSlug(sessionID string) string {
	return WikiSlugSessionPrefix + sessionID
}

// SessionSourceRef returns the wiki source_refs entry linking a generated
// page back to its triggering conversation. Follows the legacy
// "<ref>|<title>" convention so display code can split on `|`.
func SessionSourceRef(sessionID, sessionTitle string) string {
	return KnowledgeSourceSessionPrefix + sessionID + "|" + sessionTitle
}
