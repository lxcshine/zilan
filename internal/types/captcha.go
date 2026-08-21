package types

import (
	"regexp"
	"time"
)

// ---------------------------------------------------------------------------
// Shared validation helpers (single source of truth for both the handler
// layer and the frontend mirror in frontend/src/utils/identifier.ts).
// ---------------------------------------------------------------------------

// MainlandChinaMobileRegex matches an 11-digit mainland China mobile number.
var MainlandChinaMobileRegex = regexp.MustCompile(`^1[3-9]\d{9}$`)

// EmailRegex is a pragmatic RFC-lite email pattern: local@domain.tld with
// common character classes. Full RFC 5322 compliance is deliberately not
// attempted — gin's own email validator stays authoritative for classic
// registration; this regex powers the auto-detection path.
var EmailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

// IsMainlandChinaMobile reports whether s looks like a mainland-China
// mobile number (11 digits starting 13-19).
func IsMainlandChinaMobile(s string) bool {
	return MainlandChinaMobileRegex.MatchString(s)
}

// IsEmailFormat reports whether s looks like an email address.
func IsEmailFormat(s string) bool {
	return EmailRegex.MatchString(s)
}

// PasswordStrengthRegexes capture the P0-4 policy: 8-32 printable ASCII,
// must contain at least one lowercase letter, one uppercase letter and one
// digit. Applied to new registrations and password changes only — existing
// users keep logging in with legacy passwords.
var (
	passwordLowercase = regexp.MustCompile(`[a-z]`)
	passwordUppercase = regexp.MustCompile(`[A-Z]`)
	passwordDigit     = regexp.MustCompile(`\d`)
	passwordAllowed   = regexp.MustCompile(`^[A-Za-z0-9!@#$%^&*()_+\-=\[\]{};':"\\|,.<>/?~\x60]{8,32}$`)
)

// ValidatePasswordStrength returns nil when password satisfies the P0-4
// policy (8-32 chars, upper + lower + digit). The error message is stable
// so handlers can surface it verbatim and i18n it on the frontend.
func ValidatePasswordStrength(password string) bool {
	if password == "" || !passwordAllowed.MatchString(password) {
		return false
	}
	return passwordLowercase.MatchString(password) &&
		passwordUppercase.MatchString(password) &&
		passwordDigit.MatchString(password)
}

// ---------------------------------------------------------------------------
// Captcha (human verification) wire types
// ---------------------------------------------------------------------------

// Captcha challenge kinds. Configured via auth.captcha.type.
const (
	CaptchaTypeSlider = "slider"
	CaptchaTypeText   = "text"
)

// CaptchaChallengeResponse is returned by GET /auth/captcha. Slider
// challenges carry the rendered background + puzzle piece (base64 PNG data
// URIs) and the piece's vertical offset; text challenges carry a single
// distorted-digits image. The answer never leaves the server.
type CaptchaChallengeResponse struct {
	Success   bool   `json:"success"`
	CaptchaID string `json:"captcha_id"`
	Type      string `json:"type"` // slider | text
	// Slider fields
	BackgroundImage string `json:"background_image,omitempty"` // data:image/png;base64,...
	PieceImage      string `json:"piece_image,omitempty"`      // data:image/png;base64,... (transparent)
	PieceY          int    `json:"piece_y,omitempty"`          // vertical offset of the piece
	PieceSize       int    `json:"piece_size,omitempty"`       // square piece edge in px
	// Text fields
	TextImage string `json:"text_image,omitempty"` // data:image/png;base64,...
}

// CaptchaVerifyRequest carries the client's answer. Slider: X is the
// final horizontal offset of the dragged piece. Text: Answer is the
// digits typed by the user.
type CaptchaVerifyRequest struct {
	CaptchaID string `json:"captcha_id" binding:"required"`
	X         *int   `json:"x"      binding:"omitempty"`
	Answer    string `json:"answer" binding:"omitempty"`
}

// CaptchaVerifyResponse hands back the one-time ticket consumed by
// login / verification-code-send.
type CaptchaVerifyResponse struct {
	Success      bool   `json:"success"`
	CaptchaToken string `json:"captcha_token,omitempty"`
	Message      string `json:"message,omitempty"`
}

// ---------------------------------------------------------------------------
// Verification code (ownership proof) wire + persistence types
// ---------------------------------------------------------------------------

// Verification channels and purposes.
const (
	VerificationChannelSMS   = "sms"
	VerificationChannelEmail = "email"
	VerificationPurposeRegister = "register"
)

// VerificationCodeSendRequest is the body of POST
// /auth/verification-code/send. A valid captcha_token must accompany every
// send to prevent SMS/email bombing.
type VerificationCodeSendRequest struct {
	Channel      string `json:"channel"       binding:"required,oneof=sms email"`
	Target       string `json:"target"        binding:"required"`
	Purpose      string `json:"purpose"       binding:"required,oneof=register"`
	CaptchaToken string `json:"captcha_token" binding:"required"`
}

// VerificationCode is the persistent record of a sent code. The raw code is
// never stored — only its SHA-256 hash — so a database leak does not turn
// into usable codes (they are also TTL-bounded and attempt-capped).
type VerificationCode struct {
	ID string `json:"id"          gorm:"type:varchar(36);primaryKey"`
	// Channel: sms | email
	Channel string `json:"channel"     gorm:"type:varchar(8);index:idx_verification_codes_lookup"`
	// Target is the phone number or email address the code was sent to.
	Target string `json:"target"      gorm:"type:varchar(255);index:idx_verification_codes_lookup"`
	// Purpose: register (future: reset/bind)
	Purpose string `json:"purpose"     gorm:"type:varchar(16);index:idx_verification_codes_lookup"`
	// CodeHash is hex(SHA-256(code)); the plaintext code exists only in
	// the outbound provider call and the client's inbox.
	CodeHash string `json:"-"           gorm:"type:varchar(64);not null"`
	// Attempts counts failed verifications; at MaxAttempts the record is
	// dead and the user must request a new code.
	Attempts int `json:"attempts"    gorm:"not null;default:0"`
	// ConsumedAt is set once the code verifies successfully (one-time use).
	ConsumedAt *time.Time `json:"consumed_at"  gorm:""`
	// ExpiresAt bounds the code's lifetime (default 10 minutes).
	ExpiresAt time.Time `json:"expires_at"   gorm:"not null"`
	CreatedAt time.Time `json:"created_at"   gorm:"index:idx_verification_codes_lookup"`
}
