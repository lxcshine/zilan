package interfaces

import (
	"context"

	"github.com/hibiken/asynq"

	"github.com/Tencent/WeKnora/internal/types"
)

// SessionPrecipitationService externalizes high-value conversations into
// durable knowledge (知识沉淀, memory layer 4.4).
//
// Two surfaces:
//   - Automatic: after each memory-extract run, sessions that crossed the
//     high-value bar (user favorite or sustained follow-ups) are distilled
//     into a curated Markdown document inside the tenant's hidden
//     __session_insights__ knowledge base, source-tagged "session:<id>".
//   - On demand: the one-click "将本次对话整理为 Wiki" endpoint distills a
//     session into an interlinked wiki page that back-links the triggering
//     conversation via source_refs.
type SessionPrecipitationService interface {
	// MaybeEnqueuePrecipitation evaluates the high-value criteria for the
	// session and, when met, enqueues the async precipitation task. Called
	// from the memory-extract worker; best-effort and never blocks on
	// errors.
	MaybeEnqueuePrecipitation(ctx context.Context, payload *types.SessionPrecipitatePayload)

	// ProcessSessionPrecipitate is the task handler bound to
	// types.TypeSessionPrecipitate.
	ProcessSessionPrecipitate(ctx context.Context, task *asynq.Task) error

	// CreateWikiFromSession distills the session into a wiki page inside the
	// caller-chosen wiki knowledge base. Idempotent per session: a repeated
	// call refreshes the existing session/<id> page. The caller must own
	// the session (enforced via the session service's user scope).
	CreateWikiFromSession(
		ctx context.Context, sessionID string, req *types.SessionWikiRequest,
	) (*types.WikiPage, error)
}
