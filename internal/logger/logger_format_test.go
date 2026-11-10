package logger

import (
	"io"
	"os"
	"strings"
	"testing"
)

// captureStderr 临时把 os.Stderr 换成管道，用于断言 deprecation / 校验告警。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	original := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stderr = original
	_ = w.Close()
	return <-done
}

// ZILAN_LOG_FORMAT 优先，且必须包含 %msg（FR-08 条 4）。
func TestResolveLogFormatFromEnv(t *testing.T) {
	tests := []struct {
		name      string
		zilan     string
		zilanSet  bool
		legacy    string
		legacySet bool
		want      string
		wantErr   bool
	}{
		{name: "unset uses default", want: ""},
		{name: "zilan template", zilan: "%d %level %msg", zilanSet: true, want: "%d %level %msg"},
		{
			name: "zilan wins over legacy", zilan: "%msg A", zilanSet: true,
			legacy: "%msg B", legacySet: true, want: "%msg A",
		},
		{
			name: "explicitly empty zilan disables legacy", zilan: "", zilanSet: true,
			legacy: "%d %msg", legacySet: true, want: "",
		},
		{name: "legacy fallback still works", legacy: "%level %msg", legacySet: true, want: "%level %msg"},
		{name: "zilan without msg", zilan: "%d %level", zilanSet: true, wantErr: true},
		{name: "legacy without msg", legacy: "%d %level", legacySet: true, wantErr: true},
		{name: "whitespace only", zilan: "   ", zilanSet: true, want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.zilanSet {
				t.Setenv(logFormatEnv, tc.zilan)
			} else {
				os.Unsetenv(logFormatEnv)
				t.Cleanup(func() { os.Unsetenv(logFormatEnv) })
			}
			if tc.legacySet {
				t.Setenv(deprecatedLogFormatEnv, tc.legacy)
			} else {
				os.Unsetenv(deprecatedLogFormatEnv)
				t.Cleanup(func() { os.Unsetenv(deprecatedLogFormatEnv) })
			}

			var got string
			var err error
			captureStderr(t, func() { got, err = resolveLogFormatFromEnv() })

			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveLogFormatFromEnv() = %q, nil; want an error", got)
				}
				if !strings.Contains(err.Error(), "%msg") {
					t.Errorf("error %q does not mention the missing %%msg requirement", err)
				}
				wantName := logFormatEnv
				if !tc.zilanSet {
					wantName = deprecatedLogFormatEnv
				}
				if !strings.Contains(err.Error(), wantName) {
					t.Errorf("error %q does not name the offending variable %s", err, wantName)
				}
				if got != "" {
					t.Errorf("template = %q, want empty on validation failure", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("template = %q, want %q", got, tc.want)
			}
		})
	}
}

// 回落旧变量名时必须给出 deprecation 提示。
func TestResolveLogFormatFromEnv_DeprecationNotice(t *testing.T) {
	os.Unsetenv(logFormatEnv)
	t.Cleanup(func() { os.Unsetenv(logFormatEnv) })
	t.Setenv(deprecatedLogFormatEnv, "%d %msg")

	out := captureStderr(t, func() {
		if _, err := resolveLogFormatFromEnv(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	if !strings.Contains(out, deprecatedLogFormatEnv) || !strings.Contains(out, logFormatEnv) {
		t.Errorf("stderr = %q, want a deprecation notice naming both variables", out)
	}
}

// 新变量名下不应出现 deprecation 提示。
func TestResolveLogFormatFromEnv_NoNoticeForNewVar(t *testing.T) {
	t.Setenv(logFormatEnv, "%d %msg")
	os.Unsetenv(deprecatedLogFormatEnv)
	t.Cleanup(func() { os.Unsetenv(deprecatedLogFormatEnv) })

	out := captureStderr(t, func() {
		if _, err := resolveLogFormatFromEnv(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	if strings.Contains(out, "已废弃") {
		t.Errorf("stderr = %q, want no deprecation notice", out)
	}
}

// ConfigureFromEnv 必须把校验错误暴露给入口程序，由入口拒绝启动；
// 同时自身回落默认格式，保证这条错误本身还能被看见。
func TestConfigureFromEnv_RecordsFormatError(t *testing.T) {
	t.Setenv("LOG_PATH", "")
	t.Setenv(logFormatEnv, "%d %level") // 缺 %msg
	t.Cleanup(func() {
		os.Unsetenv(logFormatEnv)
		ConfigureFromEnv()
	})

	captureStderr(t, ConfigureFromEnv)

	err := LogFormatConfigError()
	if err == nil {
		t.Fatal("LogFormatConfigError() = nil, want a validation error")
	}
	if !strings.Contains(err.Error(), "%msg") {
		t.Errorf("error %q does not explain the missing %%msg requirement", err)
	}

	// 修正后错误必须清空。
	t.Setenv(logFormatEnv, "%d %level %msg")
	captureStderr(t, ConfigureFromEnv)
	if err := LogFormatConfigError(); err != nil {
		t.Errorf("LogFormatConfigError() = %v, want nil after fixing the template", err)
	}
}
