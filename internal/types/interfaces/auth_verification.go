package interfaces

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// ---------------------------------------------------------------------------
// Outbound delivery providers (P0-4 §6.2). Implementations live in
// internal/application/service/authproviders. The "log" providers write the
// code to the server log (development); "aliyun"/"smtp" deliver for real.
// ---------------------------------------------------------------------------

// SMSProvider sends a verification code to a mainland-China mobile number.
type SMSProvider interface {
	Send(ctx context.Context, phone, code string) error
}

// EmailProvider sends a verification code email to an address.
type EmailProvider interface {
	Send(ctx context.Context, email, code string) error
}

// ---------------------------------------------------------------------------
// CaptchaService — human verification (slider / text challenges).
// Process-local storage; see PRD §5.2 for the single-instance semantics.
// ---------------------------------------------------------------------------

type CaptchaService interface {
	// CreateChallenge renders a fresh challenge of the configured type.
	CreateChallenge(ctx context.Context) (*types.CaptchaChallengeResponse, error)
	// VerifyChallenge checks the client's answer and, on success, issues a
	// one-time captcha token (10-minute TTL, single use).
	VerifyChallenge(ctx context.Context, req *types.CaptchaVerifyRequest) (*types.CaptchaVerifyResponse, error)
	// ConsumeToken atomically validates and burns a captcha token. Returns
	// false when the token is unknown, expired or already used.
	ConsumeToken(ctx context.Context, token string) bool
	// ChallengeType reports the configured flavour ("slider" | "text").
	ChallengeType() string
}

// ---------------------------------------------------------------------------
// VerificationCodeService — ownership proof over SMS/email.
// ---------------------------------------------------------------------------

type VerificationCodeService interface {
	// Send issues a code to target over channel, enforcing the captcha
	// ticket, the resend interval and the daily per-target cap. The raw
	// code is handed to the provider and only its SHA-256 is persisted.
	Send(ctx context.Context, req *types.VerificationCodeSendRequest) error
	// Verify checks (and on success consumes) the latest outstanding code
	// for (channel, target, purpose). Enforces TTL and attempt cap.
	Verify(ctx context.Context, channel, target, purpose, code string) error
	// ChannelEnabled reports whether a channel is configured and usable.
	ChannelEnabled(channel string) bool
}

// VerificationCodeRepository persists verification codes and backs the
// frequency-control queries (resend interval, daily cap).
type VerificationCodeRepository interface {
	// Create inserts a new code record.
	Create(ctx context.Context, record *types.VerificationCode) error
	// LatestOutstanding returns the newest unconsumed record for
	// (channel, target, purpose), or nil when there is none.
	LatestOutstanding(ctx context.Context, channel, target, purpose string) (*types.VerificationCode, error)
	// CountSentSince counts records for (channel, target) created at or
	// after since — backs both the resend interval and the daily cap.
	CountSentSince(ctx context.Context, channel, target string, since time.Time) (int64, error)
	// Update persists attempt/consumption transitions.
	Update(ctx context.Context, record *types.VerificationCode) error
}
