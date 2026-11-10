package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// --- FR-16：解析未完成的处理与进度上报 ------------------------------------

// stubKnowledgeRepoForParse 只实现 UpdateKnowledge；其余方法保持 nil-panic，
// 一旦被测代码越界就会立刻失败。
type stubKnowledgeRepoForParse struct {
	interfaces.KnowledgeRepository
	updated []*types.Knowledge
	err     error
}

func (r *stubKnowledgeRepoForParse) UpdateKnowledge(
	_ context.Context, knowledge *types.Knowledge,
) error {
	r.updated = append(r.updated, knowledge)
	return r.err
}

func TestMarkParseIncompleteAnnotatesKnowledge(t *testing.T) {
	repo := &stubKnowledgeRepoForParse{}
	s := &knowledgeService{repo: repo}
	knowledge := &types.Knowledge{
		ID:          "k1",
		ParseStatus: types.ParseStatusProcessing,
		// 已有 metadata 不能被冲掉。
		Metadata: types.NewJSON(map[string]interface{}{"source": "upload"}),
	}
	result := &types.ReadResult{
		MarkdownContent:       "partial",
		ImageRefs:             []types.ImageRef{{OriginalRef: "a.jpg"}},
		ParseIncomplete:       true,
		ParseIncompleteReason: types.ParseIncompleteTimeout,
		PagesDone:             3,
		PagesTotal:            10,
	}

	s.markParseIncomplete(context.Background(), knowledge, result)

	if len(repo.updated) != 1 {
		t.Fatalf("UpdateKnowledge calls = %d, want 1", len(repo.updated))
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(knowledge.Metadata, &meta); err != nil {
		t.Fatalf("metadata is not valid JSON: %v", err)
	}
	if meta["parse_incomplete"] != true {
		t.Errorf("parse_incomplete = %v, want true", meta["parse_incomplete"])
	}
	if meta["parse_incomplete_reason"] != types.ParseIncompleteTimeout {
		t.Errorf("reason = %v, want timeout", meta["parse_incomplete_reason"])
	}
	if meta["parse_pages_done"] != float64(3) || meta["parse_pages_total"] != float64(10) {
		t.Errorf("pages = %v/%v, want 3/10", meta["parse_pages_done"], meta["parse_pages_total"])
	}
	if meta["source"] != "upload" {
		t.Errorf("existing metadata lost: %+v", meta)
	}
}

func TestMarkParseIncompleteIsNotAFailure(t *testing.T) {
	// 「解析未完成」是降级而不是失败：不能改 ParseStatus、不能写 ErrorMessage，
	// 否则 UI 会把已保留的部分结果当成彻底失败，用户拿不到任何内容（PRD §5.3 规则 3
	// 要求的是明确告知降级，而不是把降级升级成失败）。
	s := &knowledgeService{repo: &stubKnowledgeRepoForParse{}}
	knowledge := &types.Knowledge{ID: "k1", ParseStatus: types.ParseStatusProcessing}

	s.markParseIncomplete(context.Background(), knowledge, &types.ReadResult{
		ParseIncomplete:       true,
		ParseIncompleteReason: types.ParseIncompleteMemory,
	})

	if knowledge.ParseStatus != types.ParseStatusProcessing {
		t.Errorf("parse status = %q, want it untouched (%q)",
			knowledge.ParseStatus, types.ParseStatusProcessing)
	}
	if knowledge.ErrorMessage != "" {
		t.Errorf("error message = %q, want empty", knowledge.ErrorMessage)
	}
}

func TestParseProgressReporterWritesPercentToRedis(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	s := &knowledgeService{redisClient: rdb}
	ctx := context.Background()
	report := s.parseProgressReporter(ctx, "k1")
	if report == nil {
		t.Fatal("reporter must be available when Redis is configured")
	}

	// 逐帧回调：百分比不变时不写库（一个文档最多 100 次写入）。
	report(1, 4)
	report(1, 4)
	report(2, 4)
	report(4, 4)

	key := getParseProgressKey("k1")
	raw, err := rdb.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("progress key missing: %v", err)
	}
	var payload parseProgressPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v (%q)", err, raw)
	}
	if payload.Percent != 100 || payload.ImagesDone != 4 || payload.ImagesTotal != 4 {
		t.Errorf("payload = %+v, want 100%% at 4/4", payload)
	}
	if payload.KnowledgeID != "k1" || payload.Stage != types.StageDocReader {
		t.Errorf("payload identity = %+v", payload)
	}

	// 节流：百分比没变的重复回调不写 Redis。删掉键后再喂一次同样的进度，
	// 如果它写了就会重新出现。
	if err := rdb.Del(ctx, key).Err(); err != nil {
		t.Fatalf("del: %v", err)
	}
	report(4, 4)
	if mr.Exists(key) {
		t.Error("a repeated callback at the same percent must not write to Redis")
	}
	// 百分比变化时立刻恢复上报。
	report(3, 4)
	if !mr.Exists(key) {
		t.Error("a percent change must be reported")
	}

	s.clearParseProgress(ctx, "k1")
	if mr.Exists(key) {
		t.Error("progress key must be cleared once the parse stage is over")
	}
}

func TestParseProgressReporterUnknownTotalReportsZeroPercent(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	s := &knowledgeService{redisClient: rdb}

	report := s.parseProgressReporter(context.Background(), "k2")
	report(7, 0)

	raw, err := rdb.Get(context.Background(), getParseProgressKey("k2")).Result()
	if err != nil {
		t.Fatalf("progress key missing: %v", err)
	}
	var payload parseProgressPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if payload.Percent != 0 || payload.ImagesDone != 7 || payload.ImagesTotal != 0 {
		t.Errorf("payload = %+v, want done=7 total=0 percent=0", payload)
	}
}

func TestParseProgressReporterDegradesWithoutRedis(t *testing.T) {
	// Lite 模式没有 Redis：进度上报必须安静地退化为 nil，绝不能让解析失败。
	s := &knowledgeService{}
	if got := s.parseProgressReporter(context.Background(), "k1"); got != nil {
		t.Error("reporter must be nil without a Redis client")
	}
	// 清理同样是 no-op。
	s.clearParseProgress(context.Background(), "k1")
}

// --- FR-16：可选进度接口的分派 -------------------------------------------

type progressCapableReader struct {
	readCalls         int
	readProgressCalls int
	progress          interfaces.ParseProgressFunc
	result            *types.ReadResult
}

func (r *progressCapableReader) Read(context.Context, *types.ReadRequest) (*types.ReadResult, error) {
	r.readCalls++
	return r.result, nil
}

func (r *progressCapableReader) ReadWithProgress(
	_ context.Context, _ *types.ReadRequest, progress interfaces.ParseProgressFunc,
) (*types.ReadResult, error) {
	r.readProgressCalls++
	if progress != nil {
		progress(1, 2)
		progress(2, 2)
	}
	return r.result, nil
}

type plainDocReader struct {
	readCalls int
	result    *types.ReadResult
}

func (r *plainDocReader) Read(context.Context, *types.ReadRequest) (*types.ReadResult, error) {
	r.readCalls++
	return r.result, nil
}

func TestCallDocReaderWithTimeoutUsesProgressPathWhenSupported(t *testing.T) {
	s := &knowledgeService{}
	reader := &progressCapableReader{result: &types.ReadResult{MarkdownContent: "ok"}}

	var seen [][2]int
	_, err := s.callDocReaderWithTimeout(context.Background(), reader,
		&types.ReadRequest{FileName: "p.pdf"},
		func(done, total int) { seen = append(seen, [2]int{done, total}) })
	if err != nil {
		t.Fatalf("callDocReaderWithTimeout: %v", err)
	}
	if reader.readProgressCalls != 1 || reader.readCalls != 0 {
		t.Errorf("progress path = %d, plain path = %d; want 1/0",
			reader.readProgressCalls, reader.readCalls)
	}
	if len(seen) != 2 || seen[0] != [2]int{1, 2} || seen[1] != [2]int{2, 2} {
		t.Errorf("progress callbacks = %v, want [[1 2] [2 2]]", seen)
	}
}

func TestCallDocReaderWithTimeoutFallsBackToPlainRead(t *testing.T) {
	s := &knowledgeService{}
	reader := &plainDocReader{result: &types.ReadResult{MarkdownContent: "ok"}}

	if _, err := s.callDocReaderWithTimeout(context.Background(), reader,
		&types.ReadRequest{FileName: "p.pdf"}, func(int, int) {}); err != nil {
		t.Fatalf("callDocReaderWithTimeout: %v", err)
	}
	if reader.readCalls != 1 {
		t.Errorf("plain Read calls = %d, want 1", reader.readCalls)
	}
}

func TestCallDocReaderWithTimeoutNilProgressUsesPlainRead(t *testing.T) {
	// progress 为 nil（例如质量重试那次调用）时不必走带回调的路径。
	s := &knowledgeService{}
	reader := &progressCapableReader{result: &types.ReadResult{MarkdownContent: "ok"}}

	if _, err := s.callDocReaderWithTimeout(context.Background(), reader,
		&types.ReadRequest{FileName: "p.pdf"}, nil); err != nil {
		t.Fatalf("callDocReaderWithTimeout: %v", err)
	}
	if reader.readCalls != 1 || reader.readProgressCalls != 0 {
		t.Errorf("plain = %d, progress = %d; want 1/0", reader.readCalls, reader.readProgressCalls)
	}
}

func TestCallDocReaderWithTimeoutAnnotatesDeadline(t *testing.T) {
	// 超时仍然要能被上层识别成 ErrCodeDocReaderTimeout 的形态：错误里带
	// "docreader call timeout"，供 convert 区分「docreader 慢」与「docreader 报错」。
	s := &knowledgeService{config: &config.Config{
		KnowledgeBase: &config.KnowledgeBaseConfig{DocReaderCallTimeout: 20 * time.Millisecond},
	}}

	// 让 Read 阻塞到调用级 deadline 到期，模拟 docreader 卡死。父 ctx 不设
	// deadline，这样超时归因才能落在 DocReaderCallTimeout 上。
	_, err := s.callDocReaderWithTimeout(context.Background(), &blockingReader{},
		&types.ReadRequest{FileName: "p.pdf"}, nil)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "docreader call timeout") {
		t.Errorf("error = %v, want it annotated as a docreader timeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want it to unwrap to DeadlineExceeded", err)
	}
}

// blockingReader 阻塞到 ctx 结束再返回错误。
type blockingReader struct{}

func (blockingReader) Read(ctx context.Context, _ *types.ReadRequest) (*types.ReadResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
