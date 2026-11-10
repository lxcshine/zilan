package docparser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
)

// FR-14 ③ 超时协同：内层解析预算必须可配、必须被真正执行，并且永远不会
// 超过外层 ctx 的剩余时间（外层与内层的严格大小关系由 internal/config 在
// 启动时校验抬升，见 config 包的 mineru_inner_budget_test.go）。

func TestMinerUTimeoutConfig(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		want        time.Duration
	}{
		{"empty", "", defaultMinerUTimeout},
		{"whitespace", "  ", defaultMinerUTimeout},
		{"minutes", "90m", 90 * time.Minute},
		{"trimmed", " 5400s ", 90 * time.Minute},
		{"invalid", "invalid", defaultMinerUTimeout},
		{"unitless", "5400", defaultMinerUTimeout},
		{"zero", "0s", defaultMinerUTimeout},
		{"negative", "-1s", defaultMinerUTimeout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(mineruTimeoutEnv, tt.value)
			assert.Equal(t, tt.want, NewMinerUReader(nil).timeout)
		})
	}
}

func TestMinerUCloudTimeoutConfig(t *testing.T) {
	t.Setenv(mineruCloudTimeoutEnv, "")
	assert.Equal(t, defaultCloudTimeout, NewMinerUCloudReader(nil).timeout)
	assert.Equal(t, 30*time.Minute, defaultCloudTimeout, "cloud default aligns with upstream")
	t.Setenv(mineruCloudTimeoutEnv, "45m")
	assert.Equal(t, 45*time.Minute, NewMinerUCloudReader(nil).timeout)
	t.Setenv(mineruCloudTimeoutEnv, "0s")
	assert.Equal(t, defaultCloudTimeout, NewMinerUCloudReader(nil).timeout)
}

// 内层预算取 min(自身配置, 外层剩余)：外层已经只剩一点时间时，
// 内层不能再按自己的配置跑，否则就是"内层还在跑、外层已放弃"。
func TestMinerUEffectiveBudget(t *testing.T) {
	assert.Equal(t, 10*time.Minute, minerUEffectiveBudget(context.Background(), 10*time.Minute))
	assert.Equal(t, defaultMinerUTimeout, minerUEffectiveBudget(context.Background(), 0))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	budget := minerUEffectiveBudget(ctx, time.Hour)
	assert.LessOrEqual(t, budget, 3*time.Second)
	assert.Greater(t, budget, 2*time.Second)
}

// 配置出来的超时（而不是旧的硬编码值）必须在两条协议上都真正终止解析，
// 并且要早于调用方自己的截止时间。
func TestMinerUReaderHonorsConfiguredTimeout(t *testing.T) {
	allowLoopbackSSRF(t)
	req := &types.ReadRequest{FileContent: []byte("%PDF"), FileName: "a.pdf", FileType: "pdf"}

	t.Run("v1", func(t *testing.T) {
		t.Setenv(mineruTimeoutEnv, "200ms")
		statuses := make([]string, 1000)
		for i := range statuses {
			statuses[i] = "running"
		}
		fake := &fakeMinerUV1{t: t, pollStatuses: statuses}
		server := httptest.NewServer(fake.handler())
		defer server.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := NewMinerUReader(map[string]string{"mineru_endpoint": server.URL}).Read(ctx, req)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.NoError(t, ctx.Err(), "the configured timeout, not the caller's deadline, should stop the parse")
		assert.True(t, fake.canceled, "abandoned job should be canceled on the server")
	})

	t.Run("legacy", func(t *testing.T) {
		t.Setenv(mineruTimeoutEnv, "100ms")
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/health" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer server.Close()
		defer close(release)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := NewMinerUReader(map[string]string{"mineru_endpoint": server.URL}).Read(ctx, req)
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Client.Timeout"),
			"expected a timeout error, got %v", err)
		assert.NoError(t, ctx.Err(), "the configured timeout, not the caller's deadline, should stop the parse")
	})
}

func TestMinerUCloudPollStopsAtConfiguredTimeout(t *testing.T) {
	allowLoopbackSSRF(t)
	t.Setenv(mineruCloudTimeoutEnv, "200ms")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"extract_result":[{"state":"running"}]}}`)
	}))
	defer server.Close()

	reader := NewMinerUCloudReader(nil)
	reader.baseURL = server.URL
	reader.pollInterval = 10 * time.Millisecond

	start := time.Now()
	_, _, _, err := reader.pollBatchResult(context.Background(), "batch-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("timed out after %s", 200*time.Millisecond))
	assert.Less(t, time.Since(start), 5*time.Second)
}

// 批次预算同样要收敛到外层 ctx 之内：外层只剩 200ms 时，1 小时的内层配置
// 不能让轮询继续跑下去。
func TestMinerUCloudPollStopsAtCallerDeadline(t *testing.T) {
	allowLoopbackSSRF(t)
	t.Setenv(mineruCloudTimeoutEnv, "1h")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"extract_result":[{"state":"running"}]}}`)
	}))
	defer server.Close()

	reader := NewMinerUCloudReader(nil)
	reader.baseURL = server.URL
	reader.pollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, _, err := reader.pollBatchResult(ctx, "batch-1")
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
}

// config 与 docparser 各自持有同一组默认值与 env 名，这里守住漂移：
// 任何一侧改了常量或环境变量名，这个测试就会失败。
func TestMinerUInnerBudgetMatchesConfig(t *testing.T) {
	t.Setenv(mineruTimeoutEnv, "")
	t.Setenv(mineruCloudTimeoutEnv, "")
	want := defaultMinerUTimeout
	if defaultCloudTimeout > want {
		want = defaultCloudTimeout
	}
	assert.Equal(t, want, config.MinerUInnerBudget(),
		"internal/config must compute the same inner budget as docparser")

	t.Setenv(mineruTimeoutEnv, "7m")
	t.Setenv(mineruCloudTimeoutEnv, "3m")
	assert.Equal(t, 7*time.Minute, config.MinerUInnerBudget())
}
