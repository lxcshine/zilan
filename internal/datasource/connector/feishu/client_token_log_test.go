package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/logger"
)

// FR-08 条 1：tenant_access_token 的任何片段（含首 8 位 / 末 4 位这类截断形式）
// 都不得写入日志。此前这里打印的是 "前 8 位...末 4 位"。
func TestGetTenantAccessToken_DoesNotLogTokenFragments(t *testing.T) {
	logger.ConfigureFromEnv()
	t.Cleanup(func() { logger.ConfigureFromEnv() })

	var buf bytes.Buffer
	logger.SetOutput(&buf)
	defer logger.SetOutput(os.Stdout)

	const token = "t-g1040abcdefghijSECRET1234567890WXYZ"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":                0,
			"msg":                 "success",
			"tenant_access_token": token,
			"expire":              7200,
		})
	}))
	defer srv.Close()

	c := &Client{
		baseURL:    srv.URL,
		appID:      "cli_app_id",
		appSecret:  "app-secret-value",
		httpClient: srv.Client(),
	}

	got, err := c.getTenantAccessToken(context.Background())
	if err != nil {
		t.Fatalf("getTenantAccessToken: %v", err)
	}
	if got != token {
		t.Fatalf("token = %q, want %q", got, token)
	}

	out := buf.String()
	if !strings.Contains(out, "tenant_access_token") {
		t.Fatalf("expected a token-acquisition log line, got:\n%s", out)
	}
	for _, fragment := range []string{
		token,
		token[:8],
		token[len(token)-4:],
		"app-secret-value",
	} {
		if strings.Contains(out, fragment) {
			t.Errorf("credential fragment %q leaked into logs:\n%s", fragment, out)
		}
	}
	// 诊断信息仍然保留：长度与有效期。
	if want := fmt.Sprintf("len=%d", len(token)); !strings.Contains(out, want) {
		t.Errorf("log lost %q:\n%s", want, out)
	}
	if !strings.Contains(out, "expire=7200s") {
		t.Errorf("log lost the expiry:\n%s", out)
	}
}
