package middleware

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSanitizeBody(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "camelCase apiKey",
			in:   `{"modelName":"gpt-5.2","apiKey":"sk-secret-123","provider":"azure_openai"}`,
			want: `{"modelName":"gpt-5.2","apiKey":"***","provider":"azure_openai"}`,
		},
		{
			name: "snake_case api_key",
			in:   `{"api_key":"sk-secret-123"}`,
			want: `{"api_key":"***"}`,
		},
		{
			name: "PascalCase APIKey",
			in:   `{"APIKey":"sk-secret-123"}`,
			want: `{"APIKey":"***"}`,
		},
		{
			name: "secretKey camelCase",
			in:   `{"secretKey":"abc","accessKeyId":"id"}`,
			want: `{"secretKey":"***","accessKeyId":"id"}`,
		},
		{
			name: "refreshToken / accessToken camelCase",
			in:   `{"refreshToken":"rt","accessToken":"at"}`,
			want: `{"refreshToken":"***","accessToken":"***"}`,
		},
		{
			name: "password and token preserved as masked",
			in:   `{"password":"p","token":"t"}`,
			want: `{"password":"***","token":"***"}`,
		},
		{
			name: "snake_case new_password and old_password",
			in:   `{"email":"alice@example.com","new_password":"FreshPass9","old_password":"OldPass9"}`,
			want: `{"email":"alice@example.com","new_password":"***","old_password":"***"}`,
		},
		{
			name: "extra whitespace around colon",
			in:   `{"apiKey"  :   "leak"}`,
			want: `{"apiKey":"***"}`,
		},
		{
			name: "non sensitive fields untouched",
			in:   `{"baseUrl":"https://example.com","modelName":"gpt"}`,
			want: `{"baseUrl":"https://example.com","modelName":"gpt"}`,
		},
		{
			name: "OAuth authorization response fields",
			in:   `{"authorization_url":"https://idp.example/authorize?state=secret","authorization_attempt":"secret-state"}`,
			want: `{"authorization_url":"***","authorization_attempt":"***"}`,
		},
		{
			name: "ticket / assertion / device & pending token",
			in:   `{"ticket":"ws-handshake-cred","assertion":"jwt-blob","device_token":"dt","pending_token":"pt","next_token":"nt"}`,
			want: `{"ticket":"***","assertion":"***","device_token":"***","pending_token":"***","next_token":"***"}`,
		},
		{
			name: "pairing link",
			in:   `{"pairing_link":"https://im.example/pair?code=secret"}`,
			want: `{"pairing_link":"***"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeBody(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeBody(%q)\n got: %s\nwant: %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeQuery(t *testing.T) {
	got := sanitizeQuery("code=secret-code&state=secret-state&next=%2Fsettings&state=second")
	want := "code=%2A%2A%2A&next=%2Fsettings&state=%2A%2A%2A"
	if got != want {
		t.Fatalf("sanitizeQuery() = %q, want %q", got, want)
	}
}

// ticket 走 query string（浏览器无法在 upgrade 请求上设置 Authorization 头），
// 必须和 token 一样被脱敏。
func TestSanitizeQuery_Ticket(t *testing.T) {
	got := sanitizeQuery("ticket=handshake-cred&room=42")
	if strings.Contains(got, "handshake-cred") {
		t.Fatalf("ticket leaked into sanitized query: %q", got)
	}
	if !strings.Contains(got, "room=42") {
		t.Errorf("non-sensitive param dropped: %q", got)
	}
}

// 多模态请求里的 data URI 不能把 base64 载荷整块写进日志，
// 但媒体类型要保留（诊断需要）。
func TestSanitizeBody_RedactsDataURI(t *testing.T) {
	payload := strings.Repeat("QUJD", 2000)
	in := `{"image":"data:image/png;base64,` + payload + `","prompt":"hi"}`
	got := sanitizeBody(in)

	if strings.Contains(got, payload) {
		t.Fatal("base64 payload leaked into sanitized body")
	}
	if !strings.Contains(got, "data:image/png;base64,[redacted") {
		t.Errorf("media type not preserved: %q", got)
	}
	if !strings.Contains(got, fmt.Sprintf("[redacted %d chars]", len(payload))) {
		t.Errorf("payload length marker missing: %q", got)
	}
	if !strings.Contains(got, `"prompt":"hi"`) {
		t.Errorf("non-sensitive field dropped: %q", got)
	}
}

// data URI 脱敏要先于敏感字段脱敏，且两者可以同时生效。
func TestSanitizeBody_DataURIWithSensitiveField(t *testing.T) {
	in := `{"token":"t0ken","file":"data:application/pdf;base64,AAAA"}`
	got := sanitizeBody(in)
	want := `{"token":"***","file":"data:application/pdf;base64,[redacted 4 chars]"}`
	if got != want {
		t.Errorf("sanitizeBody() = %q, want %q", got, want)
	}
}

// 入站请求体不得被无界缓冲：中间件只驻留日志需要的字节，
// 剩余部分原样交还 handler（FR-07）。
func TestReadRequestBody_CapsBufferAndReplaysBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := strings.Repeat("a", maxBodySize*3)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/chat-messages", strings.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	logged := readRequestBody(c)

	if !strings.Contains(logged, bodyTruncatedMarker) {
		t.Errorf("truncated body not marked: %q", logged[len(logged)-64:])
	}
	if n := strings.Count(logged, "a"); n > maxBodySize {
		t.Errorf("log kept %d body bytes, want at most %d", n, maxBodySize)
	}

	// handler 仍能读到完整 body（回放必须无损）。
	got, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("handler read: %v", err)
	}
	if string(got) != payload {
		t.Errorf("handler got %d bytes, want %d", len(got), len(payload))
	}
	if err := c.Request.Body.Close(); err != nil {
		t.Errorf("body close: %v", err)
	}
}

// 小于上限的请求体不标注截断，且原样交给 handler。
func TestReadRequestBody_SmallBodyUntouched(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := `{"query":"hello"}`

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/search", strings.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	if logged := readRequestBody(c); logged != payload {
		t.Errorf("logged = %q, want %q", logged, payload)
	}
	got, err := io.ReadAll(c.Request.Body)
	if err != nil {
		t.Fatalf("handler read: %v", err)
	}
	if string(got) != payload {
		t.Errorf("handler got %q, want %q", got, payload)
	}
}

// handler 自己装的 http.MaxBytesReader 仍然要看到完整请求并自行判定，
// 中间件的回放不能替它把 body 截短。
func TestReadRequestBody_KeepsHandlerBodyLimitEffective(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := strings.Repeat("b", 4096)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/x", strings.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	_ = readRequestBody(c)
	c.Request.Body = http.MaxBytesReader(w, c.Request.Body, 1024)
	if _, err := io.ReadAll(c.Request.Body); err == nil {
		t.Fatal("MaxBytesReader did not trip, body must have been truncated by the middleware")
	}
}
