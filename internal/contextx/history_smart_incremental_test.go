package contextx

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件覆盖 PRD FR-10 的增量摘要缓存契约：调用方把上一轮的运行摘要与「尚未被它覆盖
// 的轮次」一起交回来时，SmartCompress 不得重算已经覆盖过的内容。

// countingSummarizer 记录每一次摘要调用的提示词。
type countingSummarizer struct {
	calls   int
	prompts []string
	reply   string
}

func (s *countingSummarizer) fn() SummarizeFunc {
	return func(_ context.Context, prompt string) (string, error) {
		s.calls++
		s.prompts = append(s.prompts, prompt)
		if strings.Contains(prompt, "Merge the existing running summary") {
			return s.reply, nil
		}
		return "部分摘要", nil
	}
}

func smartTurns(count int, prefix string) []Turn {
	out := make([]Turn, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, Turn{
			User:      fmt.Sprintf("%s 第%d轮提问", prefix, i),
			Assistant: fmt.Sprintf("%s 第%d轮回答", prefix, i),
			Ref:       i,
		})
	}
	return out
}

func TestSmartCompressReuseSkipsTheSummarizerEntirely(t *testing.T) {
	cfg := SmartHistoryConfig{RecentRounds: 4, ChunkRounds: 6, MaxSummaryTokens: 800}
	ctx := context.Background()

	// 第一轮：10 轮历史，旧区间 6 轮被折进摘要。
	first := &countingSummarizer{reply: "运行摘要 S1"}
	summary, kept, report, err := SmartCompressWithReport(
		ctx, smartTurns(10, "旧"), "", cfg, first.fn(), NewCounter(VendorGeneric))
	require.NoError(t, err)
	require.Equal(t, "运行摘要 S1", summary)
	require.Len(t, kept, 4, "最新的四轮原样保留")
	require.Equal(t, 6, report.CompressedRounds)
	require.Greater(t, first.calls, 0)

	// 第二轮：调用方交回摘要 + 只交「尚未被覆盖」的轮次。没有新轮次越过保留窗口时，
	// 一次摘要调用都不该发生——这正是检查点缓存要消除的每轮全量重算。
	second := &countingSummarizer{reply: "运行摘要 S2"}
	summary2, kept2, report2, err := SmartCompressWithReport(
		ctx, kept, summary, cfg, second.fn(), NewCounter(VendorGeneric))
	require.NoError(t, err)
	require.Equal(t, "运行摘要 S1", summary2, "没有新内容时摘要原样返回")
	require.Len(t, kept2, 4)
	require.Zero(t, second.calls, "复用已有摘要时不得再调用摘要模型")
	require.Zero(t, report2.CompressedRounds)
	require.Zero(t, report2.SummarizeCalls)
}

func TestSmartCompressOnlySummarizesTheNewRounds(t *testing.T) {
	cfg := SmartHistoryConfig{RecentRounds: 4, ChunkRounds: 6, MaxSummaryTokens: 800}
	ctx := context.Background()

	first := &countingSummarizer{reply: "运行摘要 S1"}
	summary, kept, _, err := SmartCompressWithReport(
		ctx, smartTurns(10, "旧"), "", cfg, first.fn(), NewCounter(VendorGeneric))
	require.NoError(t, err)
	require.Equal(t, "运行摘要 S1", summary)

	// 下一轮：保留的四轮 + 一轮新对话。只有越过保留窗口的那一轮进入 map，
	// 上一份摘要通过 reduce 合并，而不是把十轮重读一遍。
	next := append(append([]Turn{}, kept...), Turn{User: "新提问", Assistant: "新回答", Ref: len(kept)})
	second := &countingSummarizer{reply: "运行摘要 S2"}
	summary2, kept2, report2, err := SmartCompressWithReport(
		ctx, next, summary, cfg, second.fn(), NewCounter(VendorGeneric))
	require.NoError(t, err)
	require.Equal(t, "运行摘要 S2", summary2)
	require.Len(t, kept2, 4)
	require.Equal(t, 1, report2.CompressedRounds, "只有新越过窗口的那一轮被折进摘要")
	require.Equal(t, 2, report2.SummarizeCalls, "一次 map + 一次 reduce")

	joined := strings.Join(second.prompts, "\n")
	require.Contains(t, joined, "运行摘要 S1", "上一份摘要作为已有摘要参与合并")
	require.Contains(t, joined, "旧 第6轮提问", "只有刚越过保留窗口的那一轮进入 map")
	require.NotContains(t, joined, "旧 第0轮提问", "已被覆盖的轮次不得再进摘要请求")
	require.NotContains(t, joined, "新提问", "最新一轮仍在保留窗口内，原样重放不进摘要")
}

func TestSmartCompressReportDoesNotClaimUnsummarizedRounds(t *testing.T) {
	cfg := SmartHistoryConfig{RecentRounds: 2, ChunkRounds: 6, MaxSummaryTokens: 800}
	ctx := context.Background()

	// 摘要器不可用（模型解析失败）：旧轮次既没有被摘要覆盖，就不能被声明为已覆盖，
	// 否则调用方会把它们写进检查点，下一轮既不被重放也没有摘要替代。
	summary, kept, report, err := SmartCompressWithReport(
		ctx, smartTurns(6, "旧"), "已有摘要", cfg, nil, NewCounter(VendorGeneric))
	require.NoError(t, err)
	require.Equal(t, "已有摘要", summary)
	require.Len(t, kept, 2)
	require.Zero(t, report.CompressedRounds)
	require.Zero(t, report.SummarizeCalls)
}

func TestSmartCompressReportCountsStickySurvivors(t *testing.T) {
	cfg := SmartHistoryConfig{RecentRounds: 2, ChunkRounds: 6, MaxSummaryTokens: 800}
	history := smartTurns(6, "旧")
	// 旧区间里的一轮带有关键数字，属于 sticky，必须原样保留而不是被摘要吃掉。
	history[1].Assistant = "预算定为 12 万元，11 月 30 日之前完成交付"

	summarizer := &countingSummarizer{reply: "运行摘要"}
	summary, kept, report, err := SmartCompressWithReport(
		context.Background(), history, "", cfg, summarizer.fn(), NewCounter(VendorGeneric))
	require.NoError(t, err)
	require.Equal(t, "运行摘要", summary)
	require.Equal(t, 1, report.StickyKeptRounds)
	require.Equal(t, 3, report.CompressedRounds, "旧区间四轮里只有三轮进了摘要")
	require.Len(t, kept, 3, "sticky 一轮 + 最新两轮")
	require.True(t, kept[0].Sticky)
}

func TestSmartHistoryConfigSplitPointMatchesCompress(t *testing.T) {
	cfg := SmartHistoryConfig{RecentRounds: 4}
	require.Equal(t, 4, cfg.Normalize().RecentRounds)
	require.Equal(t, 6, cfg.Normalize().ChunkRounds)
	require.Equal(t, 0, cfg.SplitPoint(3), "历史不足保留窗口时没有可压缩的旧轮次")
	require.Equal(t, 0, cfg.SplitPoint(4))
	require.Equal(t, 1, cfg.SplitPoint(5))
	require.Equal(t, 6, cfg.SplitPoint(10))
}

func TestSmartCompressKeepsLegacySignature(t *testing.T) {
	summarizer := &countingSummarizer{reply: "运行摘要"}
	summary, kept, err := SmartCompress(context.Background(), smartTurns(10, "旧"), "",
		SmartHistoryConfig{RecentRounds: 4}, summarizer.fn(), nil)
	require.NoError(t, err)
	require.Equal(t, "运行摘要", summary)
	require.Len(t, kept, 4)
	require.Greater(t, summarizer.calls, 0)
}
