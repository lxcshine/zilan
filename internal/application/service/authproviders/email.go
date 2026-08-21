package authproviders

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net/smtp"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
)

// ---------------------------------------------------------------------------
// SMTP email provider (standard library only)
// ---------------------------------------------------------------------------

// SMTPEmailProvider sends verification-code email through a configured SMTP
// server. Implicit TLS (port 465) is selected when use_ssl is true — the
// common Chinese-provider setup (QQ/163/enterprise mail) — otherwise a plain
// connection with STARTTLS upgrade (587/25).
type SMTPEmailProvider struct {
	host     string
	port     int
	username string
	password string
	from     string
	useSSL   bool
}

// NewSMTPEmailProvider builds the provider from config.
func NewSMTPEmailProvider(cfg *config.AuthSMTPConfig) *SMTPEmailProvider {
	useSSL := true
	if cfg.UseSSL != nil {
		useSSL = *cfg.UseSSL
	}
	if cfg.Port == 0 {
		if useSSL {
			cfg.Port = 465
		} else {
			cfg.Port = 587
		}
	}
	return &SMTPEmailProvider{
		host:     strings.TrimSpace(cfg.Host),
		port:     cfg.Port,
		username: strings.TrimSpace(cfg.Username),
		password: cfg.Password,
		from:     strings.TrimSpace(cfg.From),
		useSSL:   useSSL,
	}
}

// Send composes and delivers one verification-code email.
func (p *SMTPEmailProvider) Send(ctx context.Context, email, code string) error {
	subject := "=?UTF-8?B?" + base64Encode("知澜验证码") + "?="
	body := strings.Join([]string{
		"Content-Type: text/plain; charset=UTF-8",
		"From: " + p.fromAddress(),
		"To: " + email,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"",
		"您好，",
		"",
		"您的验证码为：" + code + "，10 分钟内有效。",
		"如非本人操作，请忽略本邮件。",
		"",
		"—— 知澜 Zilan",
	}, "\r\n")

	addr := fmt.Sprintf("%s:%d", p.host, p.port)
	auth := smtp.PlainAuth("", p.username, p.password, p.host)

	var err error
	if p.useSSL {
		err = p.sendImplicitTLS(addr, auth, email, []byte(body))
	} else {
		err = smtp.SendMail(addr, auth, p.fromAddress(), []string{email}, []byte(body))
	}
	if err != nil {
		return fmt.Errorf("smtp: send to %s via %s: %w", email, addr, err)
	}
	logger.Infof(ctx, "[auth/email:smtp] code delivered to %s via %s", email, addr)
	return nil
}

// sendImplicitTLS dials a TLS connection (port 465 style) and runs the SMTP
// session over it — net/smtp has no one-shot helper for this mode.
func (p *SMTPEmailProvider) sendImplicitTLS(addr string, auth smtp.Auth, to string, msg []byte) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: p.host})
	if err != nil {
		return fmt.Errorf("tls dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	client, err := smtp.NewClient(conn, p.host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer client.Close()

	if err = client.Auth(auth); err != nil {
		return fmt.Errorf("smtp auth: %w", err)
	}
	if err = client.Mail(p.fromAddress()); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	if err = client.Rcpt(to); err != nil {
		return fmt.Errorf("smtp rcpt: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err = w.Write(msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err = w.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}
	return client.Quit()
}

// fromAddress falls back to the username when no explicit From was set
// (common for provider-issued mailboxes).
func (p *SMTPEmailProvider) fromAddress() string {
	if p.from != "" {
		return p.from
	}
	return p.username
}

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
