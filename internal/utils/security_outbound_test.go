package utils

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 端口黑名单必须在拨号侧复检：URL 校验只覆盖经过 handler 的入参，
// 重定向或 SDK 运行时发现的地址会直接走到 dialer。
func TestSSRFSafeDialContext_RechecksRestrictedPort(t *testing.T) {
	for _, port := range []string{"22", "3306", "6379", "9200"} {
		// 端口检查排在主机名检查之前，所以这里必然命中端口规则，
		// 不需要真的解析或建连。
		_, err := SSRFSafeDialContext(context.Background(), "tcp", "localhost:"+port)
		if err == nil {
			t.Fatalf("dial to port %s succeeded, want blocked", port)
		}
		if !strings.Contains(err.Error(), "port "+port+" is restricted") {
			t.Errorf("dial to port %s: err = %v, want a restricted-port error", port, err)
		}
	}
}

// 非黑名单端口不应被端口规则拦下（这里继续被主机名规则拦住，
// 说明拦截原因是主机名而不是端口）。
func TestSSRFSafeDialContext_AllowsNormalPortThroughPortRule(t *testing.T) {
	_, err := SSRFSafeDialContext(context.Background(), "tcp", "localhost:8080")
	if err == nil {
		t.Fatal("expected localhost to be blocked, got nil error")
	}
	if strings.Contains(err.Error(), "port 8080 is restricted") {
		t.Errorf("port 8080 wrongly treated as restricted: %v", err)
	}
	if !strings.Contains(err.Error(), "hostname localhost is restricted") {
		t.Errorf("err = %v, want a hostname-restricted error", err)
	}
}

// 跨源重定向不得重放非 GET 请求或带 body 的请求：Go 默认会把原始 body
// 重新发往新主机，等于把用户提交的数据交给未经指定的第三方。
func TestNewSSRFCheckRedirect_RejectsCrossOriginReplay(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1,::1,localhost")
	ResetSSRFWhitelistForTest()
	t.Cleanup(ResetSSRFWhitelistForTest)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	mux := http.NewServeMux()
	// 307 保留方法与 body，正是重放发生的场景。
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/landed", http.StatusTemporaryRedirect)
	})
	origin := httptest.NewServer(mux)
	defer origin.Close()

	client := NewSSRFSafeHTTPClient(SSRFSafeHTTPClientConfig{
		Timeout:      5 * time.Second,
		MaxRedirects: 5,
	})
	resp, err := client.Post(origin.URL+"/start", "application/json", strings.NewReader(`{"secret":"value"}`))
	if err == nil {
		resp.Body.Close()
		t.Fatal("cross-origin POST redirect was followed, want rejected")
	}
	if !errors.Is(err, ErrSSRFRedirectBlocked) {
		t.Fatalf("err = %v, want ErrSSRFRedirectBlocked", err)
	}
	if !strings.Contains(err.Error(), "cross-origin request replay is forbidden") {
		t.Errorf("err = %v, want a replay-forbidden message", err)
	}
}

// 跨源的 GET（无 body）仍然允许跟随，只是要把凭据头剥掉。
func TestNewSSRFCheckRedirect_AllowsCrossOriginGet(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1,::1,localhost")
	ResetSSRFWhitelistForTest()
	t.Cleanup(ResetSSRFWhitelistForTest)

	var landed bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		landed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/landed", http.StatusFound)
	})
	origin := httptest.NewServer(mux)
	defer origin.Close()

	client := NewSSRFSafeHTTPClient(SSRFSafeHTTPClientConfig{Timeout: 5 * time.Second, MaxRedirects: 5})
	resp, err := client.Get(origin.URL + "/start")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	if !landed {
		t.Fatal("cross-origin GET redirect was not followed")
	}
}

// SameOriginRedirectsOnly 打开时，任何跨源跳转都直接拒绝。
func TestNewSSRFSafeHTTPClient_SameOriginRedirectsOnly(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1,::1,localhost")
	ResetSSRFWhitelistForTest()
	t.Cleanup(ResetSSRFWhitelistForTest)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/same", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/landed", http.StatusFound)
	})
	mux.HandleFunc("/landed", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/cross", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/landed", http.StatusFound)
	})
	origin := httptest.NewServer(mux)
	defer origin.Close()

	client := NewSSRFSafeHTTPClient(SSRFSafeHTTPClientConfig{
		SameOriginRedirectsOnly: true,
		Timeout:                 5 * time.Second,
		MaxRedirects:            5,
	})

	resp, err := client.Get(origin.URL + "/same")
	if err != nil {
		t.Fatalf("same-origin redirect rejected: %v", err)
	}
	resp.Body.Close()

	_, err = client.Get(origin.URL + "/cross")
	if err == nil {
		t.Fatal("cross-origin redirect was followed, want rejected")
	}
	if !errors.Is(err, ErrSSRFRedirectBlocked) || !strings.Contains(err.Error(), "cross-origin redirect is forbidden") {
		t.Fatalf("err = %v, want cross-origin redirect forbidden", err)
	}
}

// RoundTripper 兜底：从未经过 handler 校验的 URL 也要被拦下。
func TestSSRFValidatingRoundTripper_BlocksRestrictedTarget(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "")
	ResetSSRFWhitelistForTest()
	t.Cleanup(ResetSSRFWhitelistForTest)

	rt := &SSRFValidatingRoundTripper{Base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("base transport must not be reached for a blocked URL")
		return nil, nil
	})}
	req, err := http.NewRequest(http.MethodGet, "http://metadata.tencentyun.com/latest/meta-data/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if _, err := rt.RoundTrip(req); err == nil {
		t.Fatal("RoundTrip to a restricted host succeeded, want blocked")
	} else if !strings.Contains(err.Error(), "blocked by SSRF policy") {
		t.Errorf("err = %v, want an SSRF policy error", err)
	}
}

func TestSSRFValidatingRoundTripper_RequiresBase(t *testing.T) {
	rt := &SSRFValidatingRoundTripper{}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	if _, err := rt.RoundTrip(req); err == nil {
		t.Fatal("RoundTrip without a base transport succeeded, want error")
	}
}

// 拨号超时必须有上界且可配置；非正值不得把上界清掉。
func TestOutboundDialTimeout(t *testing.T) {
	original := OutboundDialTimeout()
	t.Cleanup(func() { SetOutboundDialTimeout(original) })

	if original != defaultOutboundDialTimeout {
		t.Errorf("default dial timeout = %v, want %v", original, defaultOutboundDialTimeout)
	}
	SetOutboundDialTimeout(3 * time.Second)
	if got := OutboundDialTimeout(); got != 3*time.Second {
		t.Errorf("OutboundDialTimeout() = %v, want 3s", got)
	}
	SetOutboundDialTimeout(0)
	SetOutboundDialTimeout(-time.Second)
	if got := OutboundDialTimeout(); got != 3*time.Second {
		t.Errorf("non-positive value changed the timeout: got %v, want 3s", got)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
