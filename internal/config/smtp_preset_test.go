package config

import "testing"

// clearAuthEmailEnv neutralises auth email env vars so tests don't inherit
// values from the surrounding environment. Empty strings are treated as
// "unset" by applyAuthVerificationDefaults.
func clearAuthEmailEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"WEKNORA_AUTH_EMAIL_PROVIDER",
		"WEKNORA_AUTH_EMAIL_SMTP_PRESET",
		"WEKNORA_AUTH_EMAIL_SMTP_HOST",
		"WEKNORA_AUTH_EMAIL_SMTP_PORT",
		"WEKNORA_AUTH_EMAIL_SMTP_USERNAME",
		"WEKNORA_AUTH_EMAIL_SMTP_PASSWORD",
		"WEKNORA_AUTH_EMAIL_SMTP_FROM",
	} {
		t.Setenv(key, "")
	}
}

func TestApplySMTPPresetDefaults(t *testing.T) {
	t.Run("qq preset fills host port and implicit ssl", func(t *testing.T) {
		clearAuthEmailEnv(t)
		smtp := &AuthSMTPConfig{Preset: "qq"}

		applySMTPPresetDefaults(smtp)

		if smtp.Host != "smtp.qq.com" {
			t.Fatalf("host = %q, want smtp.qq.com", smtp.Host)
		}
		if smtp.Port != 465 {
			t.Fatalf("port = %d, want 465", smtp.Port)
		}
		if smtp.UseSSL == nil || !*smtp.UseSSL {
			t.Fatal("use_ssl should default to true (implicit TLS on 465)")
		}
	})

	t.Run("preset is case-insensitive", func(t *testing.T) {
		clearAuthEmailEnv(t)
		smtp := &AuthSMTPConfig{Preset: "  QQ "}

		applySMTPPresetDefaults(smtp)

		if smtp.Host != "smtp.qq.com" {
			t.Fatalf("host = %q, want smtp.qq.com", smtp.Host)
		}
	})

	t.Run("outlook preset selects STARTTLS on 587", func(t *testing.T) {
		clearAuthEmailEnv(t)
		smtp := &AuthSMTPConfig{Preset: "outlook"}

		applySMTPPresetDefaults(smtp)

		if smtp.Host != "smtp.office365.com" || smtp.Port != 587 {
			t.Fatalf("host:port = %s:%d, want smtp.office365.com:587", smtp.Host, smtp.Port)
		}
		if smtp.UseSSL == nil || *smtp.UseSSL {
			t.Fatal("outlook preset should use STARTTLS (use_ssl=false)")
		}
	})

	t.Run("explicit fields win over preset", func(t *testing.T) {
		clearAuthEmailEnv(t)
		explicitSSL := false
		smtp := &AuthSMTPConfig{
			Preset: "qq",
			Host:   "smtp.corp.example.com",
			Port:   25,
			UseSSL: &explicitSSL,
		}

		applySMTPPresetDefaults(smtp)

		if smtp.Host != "smtp.corp.example.com" {
			t.Fatalf("explicit host was overwritten: %q", smtp.Host)
		}
		if smtp.Port != 25 {
			t.Fatalf("explicit port was overwritten: %d", smtp.Port)
		}
		if smtp.UseSSL == nil || *smtp.UseSSL {
			t.Fatal("explicit use_ssl was overwritten")
		}
	})

	t.Run("unknown preset leaves fields untouched", func(t *testing.T) {
		clearAuthEmailEnv(t)
		smtp := &AuthSMTPConfig{Preset: "yandex"}

		applySMTPPresetDefaults(smtp)

		if smtp.Host != "" || smtp.Port != 0 || smtp.UseSSL != nil {
			t.Fatalf("unknown preset should not fill anything: %+v", smtp)
		}
	})
}

// TestApplyAuthVerificationDefaults_SMTPPresetViaEnv pins the env contract:
// WEKNORA_AUTH_EMAIL_SMTP_PRESET=qq plus provider=smtp must be enough for
// EmailCodeEnabled() to report the channel usable (host/port resolved by the
// preset), while an explicit WEKNORA_AUTH_EMAIL_SMTP_HOST still wins.
func TestApplyAuthVerificationDefaults_SMTPPresetViaEnv(t *testing.T) {
	t.Run("preset enables smtp channel without explicit host", func(t *testing.T) {
		clearAuthEmailEnv(t)
		t.Setenv("WEKNORA_AUTH_EMAIL_PROVIDER", "smtp")
		t.Setenv("WEKNORA_AUTH_EMAIL_SMTP_PRESET", "163")
		auth := &AuthConfig{}

		applyAuthVerificationDefaults(auth)

		if !auth.EmailCodeEnabled() {
			t.Fatal("provider=smtp + preset=163 should enable the email channel")
		}
		if auth.Email.SMTP.Host != "smtp.163.com" || auth.Email.SMTP.Port != 465 {
			t.Fatalf("preset not applied: %+v", auth.Email.SMTP)
		}
	})

	t.Run("explicit env host beats preset host", func(t *testing.T) {
		clearAuthEmailEnv(t)
		t.Setenv("WEKNORA_AUTH_EMAIL_PROVIDER", "smtp")
		t.Setenv("WEKNORA_AUTH_EMAIL_SMTP_PRESET", "qq")
		t.Setenv("WEKNORA_AUTH_EMAIL_SMTP_HOST", "smtp.qiye.corp.example.com")
		auth := &AuthConfig{}

		applyAuthVerificationDefaults(auth)

		if auth.Email.SMTP.Host != "smtp.qiye.corp.example.com" {
			t.Fatalf("host = %q, want explicit env host", auth.Email.SMTP.Host)
		}
		if auth.Email.SMTP.Port != 465 {
			t.Fatalf("port = %d, preset port should still fill the blank", auth.Email.SMTP.Port)
		}
	})

	t.Run("preset alone does not switch provider to smtp", func(t *testing.T) {
		clearAuthEmailEnv(t)
		t.Setenv("WEKNORA_AUTH_EMAIL_SMTP_PRESET", "qq")
		auth := &AuthConfig{}

		applyAuthVerificationDefaults(auth)

		// provider stays "log" — a preset alone must not silently start
		// real SMTP delivery without the operator opting in.
		if auth.Email.Provider != "log" {
			t.Fatalf("provider = %q, want log (preset must not force smtp)", auth.Email.Provider)
		}
		if auth.Email.SMTP.Host != "smtp.qq.com" {
			t.Fatalf("preset should still fill the host for later use: %+v", auth.Email.SMTP)
		}
	})
}
