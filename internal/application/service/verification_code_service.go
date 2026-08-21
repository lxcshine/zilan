package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Tencent/WeKnora/internal/application/service/authproviders"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// ---------------------------------------------------------------------------
// VerificationCodeService (P0-4 §6, docs/prd/auth-dual-channel-verification.md)
//
// Ownership-proof codes delivered over SMS/email. Every record is persisted
// (SHA-256 only) so the table doubles as the audit + frequency-control
// source of truth: resend interval and daily cap are row counts, not
// in-memory counters — honest across restarts.
// ---------------------------------------------------------------------------

// Verification error codes surfaced to the frontend verbatim (i18n on the
// client maps code -> message).
const (
	VerificationErrInvalidTarget      = "invalid_target"
	VerificationErrChannelDisabled    = "channel_disabled"
	VerificationErrCaptchaRequired    = "captcha_required"
	VerificationErrResendTooFrequent  = "resend_too_frequent"
	VerificationErrDailyLimitExceeded = "daily_limit_exceeded"
	VerificationErrCodeExpired        = "code_expired"
	VerificationErrCodeMismatch       = "code_mismatch"
	VerificationErrTooManyAttempts    = "too_many_attempts"
)

// VerificationError is the structured error carried back by Send/Verify so
// handlers can return machine-readable codes plus a human hint.
type VerificationError struct {
	Code                string
	Message             string
	RetryAfterSeconds   int
}

func (e *VerificationError) Error() string {
	if e.RetryAfterSeconds > 0 {
		return fmt.Sprintf("%s: %s (retry after %ds)", e.Code, e.Message, e.RetryAfterSeconds)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func verificationErr(code, message string) *VerificationError {
	return &VerificationError{Code: code, Message: message}
}

type verificationCodeService struct {
	repo        interfaces.VerificationCodeRepository
	captcha     interfaces.CaptchaService
	smsProvider interfaces.SMSProvider
	emailProvider interfaces.EmailProvider
	cfg         *config.AuthVerificationCodeConfig
	smsEnabled  bool
	emailEnabled bool
}

// NewVerificationCodeService wires the providers selected by config via the
// authproviders factories: "log" always works; "aliyun"/"smtp" only when
// credentials are complete (availability pre-validated via
// AuthConfig.SMSEnabled/EmailCodeEnabled). Returns the interface so the DI
// container can satisfy consumers that depend on
// interfaces.VerificationCodeService.
func NewVerificationCodeService(
	cfg *config.Config,
	repo interfaces.VerificationCodeRepository,
	captcha interfaces.CaptchaService,
) interfaces.VerificationCodeService {
	svc := &verificationCodeService{
		repo:    repo,
		captcha: captcha,
		cfg: &config.AuthVerificationCodeConfig{
			Length:                6,
			TTLMinutes:            10,
			ResendIntervalSeconds: 60,
			DailyLimitPerTarget:   10,
			MaxAttempts:           5,
		},
		smsProvider:   authproviders.NewSMSProviderFromConfig(cfg),
		emailProvider: authproviders.NewEmailProviderFromConfig(cfg),
	}
	if cfg != nil && cfg.Auth != nil {
		if cfg.Auth.VerificationCode != nil {
			svc.cfg = cfg.Auth.VerificationCode
		}
		svc.smsEnabled = cfg.Auth.SMSEnabled()
		svc.emailEnabled = cfg.Auth.EmailCodeEnabled()
	}
	return svc
}

// ChannelEnabled reports whether a channel is configured and usable.
func (s *verificationCodeService) ChannelEnabled(channel string) bool {
	switch channel {
	case types.VerificationChannelSMS:
		return s.smsEnabled && s.smsProvider != nil
	case types.VerificationChannelEmail:
		return s.emailEnabled && s.emailProvider != nil
	default:
		return false
	}
}

// Send issues a code to target over channel. Order matters for abuse
// resistance: format check -> channel check -> burn the captcha token
// (so a frequency-limit rejection still consumed the human proof) ->
// frequency caps -> deliver -> persist.
func (s *verificationCodeService) Send(ctx context.Context, req *types.VerificationCodeSendRequest) error {
	channel := strings.TrimSpace(req.Channel)
	target := strings.TrimSpace(req.Target)

	// Format validation per channel.
	switch channel {
	case types.VerificationChannelSMS:
		if !types.IsMainlandChinaMobile(target) {
			return verificationErr(VerificationErrInvalidTarget, "invalid mainland China mobile number")
		}
	case types.VerificationChannelEmail:
		if !types.IsEmailFormat(target) {
			return verificationErr(VerificationErrInvalidTarget, "invalid email address")
		}
	default:
		return verificationErr(VerificationErrInvalidTarget, "unsupported channel")
	}

	if !s.ChannelEnabled(channel) {
		return verificationErr(VerificationErrChannelDisabled, "channel not configured")
	}

	// Burn the captcha ticket first — even a rate-limited request must not
	// leave a reusable token behind.
	if s.captcha == nil || !s.captcha.ConsumeToken(ctx, req.CaptchaToken) {
		return verificationErr(VerificationErrCaptchaRequired, "captcha verification required")
	}

	// Resend interval.
	interval := time.Duration(s.cfg.ResendIntervalSeconds) * time.Second
	if sent, err := s.repo.CountSentSince(ctx, channel, target, time.Now().Add(-interval)); err != nil {
		return fmt.Errorf("verification code: resend check: %w", err)
	} else if sent > 0 {
		return &VerificationError{
			Code:              VerificationErrResendTooFrequent,
			Message:           "please wait before requesting another code",
			RetryAfterSeconds: s.cfg.ResendIntervalSeconds,
		}
	}

	// Daily per-target cap.
	dayStart := time.Now().Truncate(24 * time.Hour)
	if sent, err := s.repo.CountSentSince(ctx, channel, target, dayStart); err != nil {
		return fmt.Errorf("verification code: daily cap check: %w", err)
	} else if sent >= int64(s.cfg.DailyLimitPerTarget) {
		return verificationErr(VerificationErrDailyLimitExceeded, "daily send limit reached for this target")
	}

	// Generate the code (crypto-random digits).
	code, err := randomDigits(s.cfg.Length)
	if err != nil {
		return fmt.Errorf("verification code: generate: %w", err)
	}

	// Deliver first, persist second — a send failure leaves no orphan row
	// the user could later "verify" against.
	switch channel {
	case types.VerificationChannelSMS:
		err = s.smsProvider.Send(ctx, target, code)
	case types.VerificationChannelEmail:
		err = s.emailProvider.Send(ctx, target, code)
	}
	if err != nil {
		logger.Errorf(ctx, "verification code: deliver via %s to %s failed: %v", channel, target, err)
		return verificationErr(VerificationErrChannelDisabled, "failed to deliver code, please try again later")
	}

	hash := sha256.Sum256([]byte(code))
	record := &types.VerificationCode{
		ID:        uuid.New().String(),
		Channel:   channel,
		Target:    target,
		Purpose:   req.Purpose,
		CodeHash:  hex.EncodeToString(hash[:]),
		ExpiresAt: time.Now().Add(time.Duration(s.cfg.TTLMinutes) * time.Minute),
		CreatedAt: time.Now(),
	}
	if err := s.repo.Create(ctx, record); err != nil {
		// The code was already delivered but cannot be verified later —
		// surface a generic failure so the user simply retries.
		logger.Errorf(ctx, "verification code: persist failed: %v", err)
		return verificationErr(VerificationErrChannelDisabled, "failed to record code, please try again")
	}
	logger.Infof(ctx, "verification code sent via %s to %s (purpose=%s)", channel, target, req.Purpose)
	return nil
}

// Verify checks (and on success consumes) the latest outstanding code for
// (channel, target, purpose).
func (s *verificationCodeService) Verify(ctx context.Context, channel, target, purpose, code string) error {
	channel = strings.TrimSpace(channel)
	target = strings.TrimSpace(target)
	record, err := s.repo.LatestOutstanding(ctx, channel, target, purpose)
	if err != nil {
		return fmt.Errorf("verification code: lookup: %w", err)
	}
	if record == nil {
		return verificationErr(VerificationErrCodeMismatch, "no outstanding code, request a new one")
	}
	if time.Now().After(record.ExpiresAt) {
		return verificationErr(VerificationErrCodeExpired, "code expired, request a new one")
	}
	if record.Attempts >= s.cfg.MaxAttempts {
		return verificationErr(VerificationErrTooManyAttempts, "too many failed attempts, request a new code")
	}

	hash := sha256.Sum256([]byte(strings.TrimSpace(code)))
	if hex.EncodeToString(hash[:]) != record.CodeHash {
		record.Attempts++
		if updateErr := s.repo.Update(ctx, record); updateErr != nil {
			logger.Errorf(ctx, "verification code: persist attempt count failed: %v", updateErr)
		}
		if record.Attempts >= s.cfg.MaxAttempts {
			return verificationErr(VerificationErrTooManyAttempts, "too many failed attempts, request a new code")
		}
		return verificationErr(VerificationErrCodeMismatch, "incorrect code")
	}

	// Success: single-use consumption.
	now := time.Now()
	record.ConsumedAt = &now
	if err := s.repo.Update(ctx, record); err != nil {
		return fmt.Errorf("verification code: consume: %w", err)
	}
	return nil
}

// randomDigits returns a crypto-random string of n digits ("0"-"9").
func randomDigits(n int) (string, error) {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		sb.WriteString(v.String())
	}
	return sb.String(), nil
}

// AsVerificationError extracts the structured error, if any.
func AsVerificationError(err error) (*VerificationError, bool) {
	var ve *VerificationError
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}
