// Package authproviders implements the outbound delivery providers behind
// the P0-4 verification-code channel (PRD docs/prd/auth-dual-channel-verification.md §6.2).
//
// Two flavours per channel:
//
//   - "log": the code is written to the server log only. Development
//     default — lets the register/login flow be exercised end-to-end
//     without external credentials.
//   - real delivery: "aliyun" for SMS (Alibaba Cloud dysmsapi, ACS3
//     HMAC-SHA256 signature, zero third-party dependencies) and "smtp"
//     for email (net/smtp with implicit TLS on 465 or STARTTLS on 587/25).
package authproviders

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// ---------------------------------------------------------------------------
// Log providers (development)
// ---------------------------------------------------------------------------

// LogSMSProvider writes the code to the server log instead of sending SMS.
type LogSMSProvider struct{}

// NewLogSMSProvider returns the development SMS provider.
func NewLogSMSProvider() *LogSMSProvider { return &LogSMSProvider{} }

// Send logs the code.
func (p *LogSMSProvider) Send(ctx context.Context, phone, code string) error {
	logger.Infof(ctx, "[auth/sms:log] verification code for %s: %s (dev provider — configure auth.sms.provider=aliyun for real delivery)", phone, code)
	return nil
}

// LogEmailProvider writes the code to the server log instead of sending mail.
type LogEmailProvider struct{}

// NewLogEmailProvider returns the development email provider.
func NewLogEmailProvider() *LogEmailProvider { return &LogEmailProvider{} }

// Send logs the code.
func (p *LogEmailProvider) Send(ctx context.Context, email, code string) error {
	logger.Infof(ctx, "[auth/email:log] verification code for %s: %s (dev provider — configure auth.email.provider=smtp for real delivery)", email, code)
	return nil
}

// NewSMSProviderFromConfig resolves the configured SMS provider: "aliyun"
// when fully credentialed, otherwise the log provider (zero-config safe).
func NewSMSProviderFromConfig(cfg *config.Config) interfaces.SMSProvider {
	if cfg != nil && cfg.Auth != nil && cfg.Auth.SMS != nil &&
		strings.TrimSpace(cfg.Auth.SMS.Provider) == "aliyun" && cfg.Auth.SMSEnabled() {
		return NewAliyunSMSProvider(cfg.Auth.SMS.Aliyun)
	}
	return NewLogSMSProvider()
}

// NewEmailProviderFromConfig resolves the configured email provider: "smtp"
// when a host is set, otherwise the log provider.
func NewEmailProviderFromConfig(cfg *config.Config) interfaces.EmailProvider {
	if cfg != nil && cfg.Auth != nil && cfg.Auth.Email != nil &&
		strings.TrimSpace(cfg.Auth.Email.Provider) == "smtp" && cfg.Auth.EmailCodeEnabled() {
		return NewSMTPEmailProvider(cfg.Auth.Email.SMTP)
	}
	return NewLogEmailProvider()
}

// ---------------------------------------------------------------------------
// Aliyun SMS (dysmsapi, ACS3-HMAC-SHA256)
// ---------------------------------------------------------------------------

const (
	aliyunSMSEndpoint = "https://dysmsapi.aliyuncs.com"
	aliyunSMSAction   = "SendSms"
	aliyunSMSVersion  = "2021-01-11"
)

// AliyunSMSProvider sends codes through Alibaba Cloud dysmsapi using the
// ACS3-HMAC-SHA256 signing scheme. Implemented directly on net/http so the
// project picks up no additional dependency chain for one API call.
type AliyunSMSProvider struct {
	accessKeyID     string
	accessKeySecret string
	signName        string
	templateCode    string
	httpClient      *http.Client
}

// NewAliyunSMSProvider builds the provider from config. Credentials are
// assumed complete — config.AuthConfig.SMSEnabled gates construction.
func NewAliyunSMSProvider(cfg *config.AuthAliyunSMSConfig) *AliyunSMSProvider {
	return &AliyunSMSProvider{
		accessKeyID:     strings.TrimSpace(cfg.AccessKeyID),
		accessKeySecret: cfg.AccessKeySecret,
		signName:        strings.TrimSpace(cfg.SignName),
		templateCode:    strings.TrimSpace(cfg.TemplateCode),
		httpClient:      &http.Client{Timeout: 10 * time.Second},
	}
}

type aliyunSMSResponse struct {
	// HTTP 2xx with a Code field present means the API received the
	// request but rejected it (e.g. invalid template) — treat as failure.
	RequestID string `json:"RequestId"`
	Code      string `json:"Code"`
	Message   string `json:"Message"`
}

// Send signs and posts one SendSms request.
func (p *AliyunSMSProvider) Send(ctx context.Context, phone, code string) error {
	payload, err := json.Marshal(map[string]string{
		"SignName":      p.signName,
		"TemplateCode":  p.templateCode,
		"TemplateParam": fmt.Sprintf(`{"code":%q}`, code),
		"PhoneNumbers":  phone,
	})
	if err != nil {
		return fmt.Errorf("aliyun sms: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, aliyunSMSEndpoint, strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("aliyun sms: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-acs-action", aliyunSMSAction)
	req.Header.Set("x-acs-version", aliyunSMSVersion)
	req.Header.Set("x-acs-date", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	req.Header.Set("x-acs-signature-nonce", uuid.New().String())
	req.Header.Set("Authorization", p.signRequest(req, payload))

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("aliyun sms: send: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return fmt.Errorf("aliyun sms: read response: %w", err)
	}
	var parsed aliyunSMSResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("aliyun sms: non-JSON response (http %d): %s", resp.StatusCode, truncateBody(body))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || parsed.Code != "" {
		return fmt.Errorf("aliyun sms: SendSms failed (http %d): code=%s message=%s",
			resp.StatusCode, parsed.Code, parsed.Message)
	}
	logger.Infof(ctx, "[auth/sms:aliyun] code delivered to %s (request %s)", phone, parsed.RequestID)
	return nil
}

// signRequest computes the ACS3-HMAC-SHA256 Authorization header value for
// the outgoing request. Canonical form follows the Alibaba Cloud "V3
// signature" spec: method, URI, query, sorted lowercase headers, signed
// header list, and the hex SHA-256 of the body.
func (p *AliyunSMSProvider) signRequest(req *http.Request, payload []byte) string {
	payloadHash := sha256.Sum256(payload)
	payloadHashHex := hex.EncodeToString(payloadHash[:])

	// Headers that participate in signing: the x-acs-* set plus Host.
	// Content-Type is part of the request but not required to be signed;
	// keeping the signed set minimal (host + x-acs-*) matches the official
	// SDK behaviour for JSON POST calls.
	headers := map[string]string{
		"host":                  req.URL.Host,
		"x-acs-action":          req.Header.Get("x-acs-action"),
		"x-acs-date":            req.Header.Get("x-acs-date"),
		"x-acs-signature-nonce": req.Header.Get("x-acs-signature-nonce"),
		"x-acs-version":         req.Header.Get("x-acs-version"),
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)

	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(strings.TrimSpace(headers[name]))
		canonicalHeaders.WriteString("\n")
	}
	signedHeaders := strings.Join(names, ";")

	// Canonical query string: empty for this API (all params ride in the
	// JSON body), but kept explicit for future GET-style calls.
	canonicalQuery := ""
	if req.URL.RawQuery != "" {
		values, _ := url.ParseQuery(req.URL.RawQuery)
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			vs := values[k]
			sort.Strings(vs)
			for _, v := range vs {
				parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
			}
		}
		canonicalQuery = strings.Join(parts, "&")
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		canonicalQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHashHex,
	}, "\n")

	crHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "ACS3-HMAC-SHA256\n" + hex.EncodeToString(crHash[:])

	mac := hmac.New(sha256.New, []byte(p.accessKeySecret))
	mac.Write([]byte(stringToSign))
	signature := hex.EncodeToString(mac.Sum(nil))

	return fmt.Sprintf("ACS3-HMAC-SHA256 Credential=%s, SignedHeaders=%s, Signature=%s",
		p.accessKeyID, signedHeaders, signature)
}

func truncateBody(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
