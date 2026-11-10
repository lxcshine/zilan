package docparser

import (
	"context"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/docreader/proto"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"google.golang.org/grpc"
)

// --- FR-16：流式解析的接收端 ---------------------------------------------

// fakeReadStream 按顺序回放预置的帧，回放到尾部返回 io.EOF。
type fakeReadStream struct {
	grpc.ClientStream
	frames []*proto.ReadStreamResponse
	idx    int
}

func (f *fakeReadStream) Recv() (*proto.ReadStreamResponse, error) {
	if f.idx >= len(f.frames) {
		return nil, io.EOF
	}
	frame := f.frames[f.idx]
	f.idx++
	return frame, nil
}

// fakeDocReaderClient 只实现 ReadStream；其余方法通过嵌入接口保持 nil-panic，
// 一旦测试越界就会立刻炸掉而不是静默通过。
type fakeDocReaderClient struct {
	proto.DocReaderClient
	frames    []*proto.ReadStreamResponse
	gotReq    *proto.ReadRequest
	streamErr error
}

func (f *fakeDocReaderClient) ReadStream(
	_ context.Context, in *proto.ReadRequest, _ ...grpc.CallOption,
) (grpc.ServerStreamingClient[proto.ReadStreamResponse], error) {
	f.gotReq = in
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	return &fakeReadStream{frames: f.frames}, nil
}

func imageFrame(ref string) *proto.ReadStreamResponse {
	return &proto.ReadStreamResponse{
		Payload: &proto.ReadStreamResponse_Image{
			Image: &proto.ImageRef{
				Filename:    ref,
				OriginalRef: ref,
				MimeType:    "image/jpeg",
				ImageData:   []byte("jpeg"),
			},
		},
	}
}

func TestReadStreamKeepsOrderAndSourceAssociation(t *testing.T) {
	frames := []*proto.ReadStreamResponse{
		{Payload: &proto.ReadStreamResponse_Meta{Meta: &proto.ReadStreamMeta{
			MarkdownContent: "![p1](images/p_page_1.jpg)",
			ImageCount:      2,
			PagesTotal:      2,
			SourceBlocks: []*proto.SourceBlock{
				{Start: 0, End: 4, LocatorJson: `{"type":"pdf","page":1}`},
			},
			Metadata: map[string]string{"page_count": "2"},
		}}},
		imageFrame("images/p_page_1.jpg"),
		imageFrame("images/p_page_2.jpg"),
	}
	client := &fakeDocReaderClient{frames: frames}
	p := &GRPCDocumentReader{}

	var gotRefs []string
	result, err := p.readStream(context.Background(), client, &proto.ReadRequest{FileName: "p.pdf"},
		func(done, total int) { gotRefs = append(gotRefs, strconv.Itoa(done)+"/"+strconv.Itoa(total)) })
	if err != nil {
		t.Fatalf("readStream: %v", err)
	}

	// ①顺序：图像帧的接收顺序被完整保留。
	want := []string{"images/p_page_1.jpg", "images/p_page_2.jpg"}
	if len(result.ImageRefs) != len(want) {
		t.Fatalf("image refs = %d, want %d", len(result.ImageRefs), len(want))
	}
	for i, w := range want {
		if result.ImageRefs[i].OriginalRef != w {
			t.Errorf("image[%d] = %q, want %q", i, result.ImageRefs[i].OriginalRef, w)
		}
	}
	// ②来源关联：首帧的 source_blocks 与正文一起到达，早于任何图像。
	if len(result.SourceBlocks) != 1 || result.SourceBlocks[0].End != 4 {
		t.Errorf("source blocks = %+v, want one block ending at 4", result.SourceBlocks)
	}
	if result.PagesTotal != 2 {
		t.Errorf("pages total = %d, want 2", result.PagesTotal)
	}
	if result.ParseIncomplete {
		t.Error("complete stream must not be flagged incomplete")
	}
	// 进度回调：每帧一次，携带 meta 声明的总量。
	if len(gotRefs) != 2 || gotRefs[0] != "1/2" || gotRefs[1] != "2/2" {
		t.Errorf("progress callbacks = %v, want [1/2 2/2]", gotRefs)
	}
}

func TestReadStreamMergesTrailingIncompleteMeta(t *testing.T) {
	frames := []*proto.ReadStreamResponse{
		{Payload: &proto.ReadStreamResponse_Meta{Meta: &proto.ReadStreamMeta{
			MarkdownContent: "partial text",
			ImageCount:      4,
			PagesTotal:      4,
			Metadata:        map[string]string{"page_count": "4"},
			SourceBlocks: []*proto.SourceBlock{
				{Start: 0, End: 5, LocatorJson: `{"type":"pdf","page":1}`},
			},
		}}},
		imageFrame("images/p_page_1.jpg"),
		imageFrame("images/p_page_2.jpg"),
		// 中止时补发的尾帧：只带进度与「解析未完成」标注。
		{Payload: &proto.ReadStreamResponse_Meta{Meta: &proto.ReadStreamMeta{
			PagesDone:        2,
			PagesTotal:       4,
			Incomplete:       true,
			IncompleteReason: types.ParseIncompleteTimeout,
		}}},
	}
	client := &fakeDocReaderClient{frames: frames}
	p := &GRPCDocumentReader{}

	result, err := p.readStream(context.Background(), client, &proto.ReadRequest{FileName: "p.pdf"}, nil)
	if err != nil {
		t.Fatalf("readStream: %v", err)
	}

	// ③保留已解析部分：正文、已收到的两帧图像与溯源块都不能被尾帧冲掉。
	if result.MarkdownContent != "partial text" {
		t.Errorf("markdown = %q, want %q", result.MarkdownContent, "partial text")
	}
	if len(result.ImageRefs) != 2 {
		t.Errorf("image refs = %d, want 2", len(result.ImageRefs))
	}
	if len(result.SourceBlocks) != 1 {
		t.Errorf("source blocks = %d, want 1", len(result.SourceBlocks))
	}
	if !result.ParseIncomplete {
		t.Fatal("result must be flagged incomplete")
	}
	if result.ParseIncompleteReason != types.ParseIncompleteTimeout {
		t.Errorf("reason = %q, want %q", result.ParseIncompleteReason, types.ParseIncompleteTimeout)
	}
	if result.PagesDone != 2 || result.PagesTotal != 4 {
		t.Errorf("pages = %d/%d, want 2/4", result.PagesDone, result.PagesTotal)
	}
	// 标注同时镜像进 metadata，让只看 result.Metadata 的下游也能感知降级。
	if result.Metadata["parse_incomplete"] != "true" {
		t.Errorf("metadata parse_incomplete = %q, want true", result.Metadata["parse_incomplete"])
	}
	if result.Metadata["parse_incomplete_reason"] != types.ParseIncompleteTimeout {
		t.Errorf("metadata reason = %q, want timeout", result.Metadata["parse_incomplete_reason"])
	}
	if result.Metadata["pages_done"] != "2" || result.Metadata["pages_total"] != "4" {
		t.Errorf("metadata pages = %q/%q, want 2/4",
			result.Metadata["pages_done"], result.Metadata["pages_total"])
	}
	// 首帧原有的 metadata 不能被覆盖。
	if result.Metadata["page_count"] != "4" {
		t.Errorf("metadata page_count = %q, want 4", result.Metadata["page_count"])
	}
}

func TestReadStreamWithoutMetaFrameFails(t *testing.T) {
	client := &fakeDocReaderClient{frames: []*proto.ReadStreamResponse{imageFrame("a.jpg")}}
	p := &GRPCDocumentReader{}

	if _, err := p.readStream(context.Background(), client, &proto.ReadRequest{}, nil); err == nil {
		t.Fatal("expected an error when no meta frame arrives")
	}
}

func TestApplyIncompleteMetaCreatesMetadataWhenAbsent(t *testing.T) {
	result := &types.ReadResult{}

	applyIncompleteMeta(result, &proto.ReadStreamMeta{
		PagesDone:        1,
		PagesTotal:       3,
		Incomplete:       true,
		IncompleteReason: types.ParseIncompleteMemory,
	})

	if result.Metadata["parse_incomplete"] != "true" {
		t.Fatalf("metadata = %+v", result.Metadata)
	}
	if result.ParseIncompleteReason != types.ParseIncompleteMemory {
		t.Errorf("reason = %q", result.ParseIncompleteReason)
	}
}

func TestApplyIncompleteMetaIgnoresProgressOnlyFrame(t *testing.T) {
	result := &types.ReadResult{MarkdownContent: "keep"}

	applyIncompleteMeta(result, &proto.ReadStreamMeta{PagesDone: 2, PagesTotal: 5})

	if result.ParseIncomplete {
		t.Error("progress-only frame must not flag the result incomplete")
	}
	if result.Metadata != nil {
		t.Errorf("metadata should stay untouched, got %+v", result.Metadata)
	}
	if result.PagesDone != 2 || result.PagesTotal != 5 {
		t.Errorf("pages = %d/%d, want 2/5", result.PagesDone, result.PagesTotal)
	}
}

// --- FR-16：时长上界的下发 ------------------------------------------------

func TestParseDeadlineOverrideSkippedWithoutDeadline(t *testing.T) {
	if _, ok := parseDeadlineOverride(context.Background()); ok {
		t.Fatal("no ctx deadline must not produce an override")
	}
}

func TestParseDeadlineOverrideLeavesGraceForPartialResults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	value, ok := parseDeadlineOverride(ctx)
	if !ok {
		t.Fatal("expected a deadline override")
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("override %q is not an int: %v", value, err)
	}
	// Python 侧必须比 Go 的 ctx 早停 parseDeadlineGrace，否则流被取消、部分结果全丢。
	remaining := time.Until(time.Unix(parsed, 0))
	if remaining > 10*time.Minute-parseDeadlineGrace+time.Minute {
		t.Errorf("remaining %s is not bounded by the grace window", remaining)
	}
	if remaining < 10*time.Minute-parseDeadlineGrace-time.Minute {
		t.Errorf("remaining %s shrank far more than the grace window", remaining)
	}
}

func TestParseDeadlineOverrideSkippedWhenBudgetTooSmall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, ok := parseDeadlineOverride(ctx); ok {
		t.Fatal("a budget shorter than the grace window must not be forwarded")
	}
}

func TestWithParseDeadlineDoesNotMutateCallerOverrides(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	original := map[string]string{"ocr_pipeline": "smart"}

	out := withParseDeadline(ctx, original)

	if _, exists := original[types.ParseDeadlineOverrideKey]; exists {
		t.Fatal("caller overrides were mutated in place")
	}
	if out["ocr_pipeline"] != "smart" {
		t.Errorf("existing overrides lost: %+v", out)
	}
	if out[types.ParseDeadlineOverrideKey] == "" {
		t.Error("deadline override missing")
	}
}

func TestReadWithProgressSendsDeadlineOverride(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	client := &fakeDocReaderClient{frames: []*proto.ReadStreamResponse{
		{Payload: &proto.ReadStreamResponse_Meta{Meta: &proto.ReadStreamMeta{MarkdownContent: "ok"}}},
	}}
	p := &GRPCDocumentReader{client: client}

	if _, err := p.ReadWithProgress(ctx, &types.ReadRequest{
		FileName:              "p.pdf",
		ParserEngineOverrides: map[string]string{"ocr_pipeline": "smart"},
	}, nil); err != nil {
		t.Fatalf("ReadWithProgress: %v", err)
	}

	overrides := client.gotReq.GetConfig().GetParserEngineOverrides()
	if overrides[types.ParseDeadlineOverrideKey] == "" {
		t.Errorf("deadline override not sent: %+v", overrides)
	}
	if overrides["ocr_pipeline"] != "smart" {
		t.Errorf("existing overrides not forwarded: %+v", overrides)
	}
}

func TestReadWithProgressWithoutConnection(t *testing.T) {
	p := &GRPCDocumentReader{}

	if _, err := p.ReadWithProgress(context.Background(), &types.ReadRequest{}, nil); err == nil {
		t.Fatal("expected errNotConnected")
	}
	if _, err := p.Read(context.Background(), &types.ReadRequest{}); err == nil {
		t.Fatal("expected errNotConnected from Read")
	}
}

// --- FR-16：可选进度接口的类型断言 ---------------------------------------

// plainReader 只实现 DocReader（云端 / 命令行引擎的形态）。
type plainReader struct{}

func (plainReader) Read(context.Context, *types.ReadRequest) (*types.ReadResult, error) {
	return &types.ReadResult{MarkdownContent: "plain"}, nil
}

// progressReader 额外实现可选的进度能力。
type progressReader struct {
	progressCalls int
}

func (r *progressReader) Read(_ context.Context, _ *types.ReadRequest) (*types.ReadResult, error) {
	return &types.ReadResult{MarkdownContent: "plain"}, nil
}

func (r *progressReader) ReadWithProgress(
	_ context.Context, _ *types.ReadRequest, progress interfaces.ParseProgressFunc,
) (*types.ReadResult, error) {
	if progress != nil {
		progress(1, 2)
		r.progressCalls++
	}
	return &types.ReadResult{MarkdownContent: "with-progress"}, nil
}

func TestAsProgressReaderDetectsOptionalCapability(t *testing.T) {
	if got := interfaces.AsProgressReader(plainReader{}); got != nil {
		t.Error("a plain DocReader must not be detected as progress-capable")
	}
	pr := &progressReader{}
	got := interfaces.AsProgressReader(pr)
	if got == nil {
		t.Fatal("progress-capable reader was not detected")
	}
	if _, err := got.ReadWithProgress(context.Background(), &types.ReadRequest{}, func(int, int) {}); err != nil {
		t.Fatalf("ReadWithProgress: %v", err)
	}
	if pr.progressCalls != 1 {
		t.Errorf("progress calls = %d, want 1", pr.progressCalls)
	}
}

func TestGRPCDocumentReaderImplementsOptionalProgressInterface(t *testing.T) {
	// 9 个 DocReader 实现里只有 gRPC 客户端能给出细粒度进度；这里钉住它确实
	// 被探测到，避免以后重构悄悄退化成「没有百分比」。
	if interfaces.AsProgressReader(&GRPCDocumentReader{}) == nil {
		t.Error("GRPCDocumentReader must implement DocReaderWithProgress")
	}
	// 但 DocReader 接口签名不能变：普通实现仍然满足它。
	var _ interfaces.DocReader = plainReader{}
	var _ interfaces.DocReader = &GRPCDocumentReader{}
}
