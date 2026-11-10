package config

import (
	"testing"
	"time"
)

// FR-14 ③ 超时协同：外层 DocReader 调用超时必须严格大于 MinerU 的内层
// 解析预算，否则会出现"内层还在跑、外层已放弃"的资源浪费。

func TestMinerUInnerBudget(t *testing.T) {
	for _, tt := range []struct {
		name        string
		self, cloud string
		want        time.Duration
	}{
		{"defaults", "", "", 30 * time.Minute},
		{"self hosted larger", "1h", "10m", time.Hour},
		{"cloud larger", "10m", "45m", 45 * time.Minute},
		{"invalid falls back", "nonsense", "-5m", 30 * time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(minerUInnerTimeoutEnv, tt.self)
			t.Setenv(minerUCloudInnerTimeoutEnv, tt.cloud)
			if got := MinerUInnerBudget(); got != tt.want {
				t.Fatalf("MinerUInnerBudget() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestEnsureDocReaderTimeoutCoversMinerU(t *testing.T) {
	for _, tt := range []struct {
		name      string
		outer, in time.Duration
		want      time.Duration
	}{
		{"outer strictly greater", 40 * time.Minute, 30 * time.Minute, 40 * time.Minute},
		// 相等就是倒挂：内层跑满的那一刻外层也放弃了。
		{"equal is raised", 30 * time.Minute, 30 * time.Minute, 35 * time.Minute},
		{"inverted is raised", 10 * time.Minute, 30 * time.Minute, 35 * time.Minute},
		{"unknown inner budget", 30 * time.Minute, 0, 30 * time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ensureDocReaderTimeoutCoversMinerU(tt.outer, tt.in); got != tt.want {
				t.Fatalf("ensureDocReaderTimeoutCoversMinerU(%s, %s) = %s, want %s", tt.outer, tt.in, got, tt.want)
			}
		})
	}
}

func TestApplyKnowledgeBaseEnvOverridesRaisesDocReaderTimeout(t *testing.T) {
	for _, tt := range []struct {
		name string
		envs map[string]string
		want time.Duration
	}{
		{
			// 默认值本身就是相等的（外层 30min、云端内层 30min），必须被抬升。
			name: "defaults are raised",
			envs: map[string]string{
				"WEKNORA_DOCREADER_CALL_TIMEOUT": "",
				minerUInnerTimeoutEnv:            "",
				minerUCloudInnerTimeoutEnv:       "",
			},
			want: 30*time.Minute + docReaderTimeoutHeadroom,
		},
		{
			name: "explicit outer wins when larger",
			envs: map[string]string{
				"WEKNORA_DOCREADER_CALL_TIMEOUT": "2h",
				minerUInnerTimeoutEnv:            "",
				minerUCloudInnerTimeoutEnv:       "",
			},
			want: 2 * time.Hour,
		},
		{
			name: "large inner budget raises outer",
			envs: map[string]string{
				"WEKNORA_DOCREADER_CALL_TIMEOUT": "30m",
				minerUInnerTimeoutEnv:            "10m",
				minerUCloudInnerTimeoutEnv:       "90m",
			},
			want: 90*time.Minute + docReaderTimeoutHeadroom,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range tt.envs {
				t.Setenv(key, value)
			}
			cfg := &Config{}
			applyKnowledgeBaseEnvOverrides(cfg)
			if got := cfg.KnowledgeBase.DocReaderCallTimeout; got != tt.want {
				t.Fatalf("DocReaderCallTimeout = %s, want %s", got, tt.want)
			}
			if inner := MinerUInnerBudget(); cfg.KnowledgeBase.DocReaderCallTimeout <= inner {
				t.Fatalf("outer %s must stay strictly greater than inner %s",
					cfg.KnowledgeBase.DocReaderCallTimeout, inner)
			}
		})
	}
}
