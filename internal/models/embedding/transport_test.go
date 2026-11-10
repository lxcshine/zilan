package embedding

import (
	"net/http"
	"strings"
	"testing"
	"time"

	secutils "github.com/Tencent/WeKnora/internal/utils"
)

func TestNewEmbeddingHTTPClient_ReusesTransport(t *testing.T) {
	firstTimeout := 15 * time.Second
	secondTimeout := 45 * time.Second
	first := newEmbeddingHTTPClient(firstTimeout)
	second := newEmbeddingHTTPClient(secondTimeout)

	if first == second {
		t.Fatal("expected distinct HTTP clients")
	}
	// 客户端的 Transport 现在统一是 SSRFValidatingRoundTripper 兜底包装，
	// 连接池是否共享要看它包着的 Base。
	firstBase := ssrfBaseTransport(t, first)
	secondBase := ssrfBaseTransport(t, second)
	if firstBase != secondBase {
		t.Fatal("expected embedding HTTP clients to share a transport")
	}
	if firstBase != http.RoundTripper(sharedEmbeddingHTTPTransport) {
		t.Fatal("expected embedding HTTP client to use the shared transport")
	}
	if first.Timeout != firstTimeout {
		t.Fatalf("unexpected first client timeout: got %v, want %v", first.Timeout, firstTimeout)
	}
	if second.Timeout != secondTimeout {
		t.Fatalf("unexpected second client timeout: got %v, want %v", second.Timeout, secondTimeout)
	}
}

// ssrfBaseTransport 取出 SSRFValidatingRoundTripper 包装的真实 transport。
func ssrfBaseTransport(t *testing.T, client *http.Client) http.RoundTripper {
	t.Helper()
	wrapper, ok := client.Transport.(*secutils.SSRFValidatingRoundTripper)
	if !ok {
		t.Fatalf("expected *secutils.SSRFValidatingRoundTripper, got %T", client.Transport)
	}
	if wrapper.Base == nil {
		t.Fatal("SSRFValidatingRoundTripper has no base transport")
	}
	return wrapper.Base
}

func TestValidateEmbeddingBaseURL_RejectsLoopback(t *testing.T) {
	err := validateEmbeddingBaseURL("http://169.254.169.254/latest/meta-data")
	if err == nil {
		t.Fatal("expected SSRF error for link-local metadata URL")
	}
	if !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateEmbeddingBaseURL_AllowsEmpty(t *testing.T) {
	if err := validateEmbeddingBaseURL(""); err != nil {
		t.Fatalf("empty base URL should be allowed: %v", err)
	}
}

func TestNewOpenAIEmbedder_RejectsPrivateBaseURL(t *testing.T) {
	_, err := NewOpenAIEmbedder(
		"test-key",
		"http://169.254.169.254/latest/meta-data",
		"text-embedding-3-small",
		511,
		256,
		"model-id",
		nil,
	)
	if err == nil {
		t.Fatal("expected SSRF rejection for link-local metadata URL")
	}
}
