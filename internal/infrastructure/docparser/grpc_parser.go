package docparser

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	docclient "github.com/Tencent/WeKnora/docreader/client"
	"github.com/Tencent/WeKnora/docreader/proto"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/status"
)

func getMaxMessageSize() int {
	if sizeStr := os.Getenv("MAX_FILE_SIZE_MB"); sizeStr != "" {
		if size, err := strconv.Atoi(sizeStr); err == nil && size > 0 {
			return size * 1024 * 1024
		}
	}
	return 50 * 1024 * 1024
}

// GRPCDocumentReader implements DocumentReader over gRPC.
type GRPCDocumentReader struct {
	mu     sync.RWMutex
	conn   *grpc.ClientConn
	client proto.DocReaderClient
	addr   string
}

func NewGRPCDocumentReader(addr string) (*GRPCDocumentReader, error) {
	p := &GRPCDocumentReader{}
	if addr != "" {
		if err := p.connect(addr); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *GRPCDocumentReader) connect(addr string) error {
	authConfig := docclient.LoadAuthConfigFromEnv()
	opts, err := authConfig.BuildDialOptions(getMaxMessageSize())
	if err != nil {
		return fmt.Errorf("failed to build docreader dial options: %w", err)
	}
	if authConfig.TLSEnabled {
		logger.Infof(context.Background(), "TLS enabled for docreader gRPC client")
	}
	if authConfig.AuthToken != "" {
		logger.Infof(context.Background(),
			"Token authentication enabled for docreader gRPC client (TLS=%v)",
			authConfig.TLSEnabled,
		)
	}

	resolver.SetDefaultScheme("dns")

	start := time.Now()
	conn, err := grpc.Dial("dns:///"+addr, opts...)
	if err != nil {
		return fmt.Errorf("failed to connect to docreader: %w", err)
	}
	logger.Infof(context.Background(), "Connected to docreader in %v", time.Since(start))

	p.conn = conn
	p.client = proto.NewDocReaderClient(conn)
	p.addr = addr
	return nil
}

func (p *GRPCDocumentReader) Reconnect(addr string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
		p.client = nil
		p.addr = ""
	}
	return p.connect(addr)
}

func (p *GRPCDocumentReader) IsConnected() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.conn != nil
}

func (p *GRPCDocumentReader) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil {
		return p.conn.Close()
	}
	return nil
}

var errNotConnected = fmt.Errorf("docreader service not connected")

// parseDeadlineGrace 是留给「回传已解析部分 + 尾部 incomplete 帧」的余量（FR-16）。
// Python 侧必须比 Go 的 ctx deadline 早一点停下来：一旦 ctx 到期，gRPC 流会被
// 直接取消，已经渲染好的页也就跟着丢了 —— 那正好是 PRD 要求避免的
// 「要么全有、要么全无」。
const parseDeadlineGrace = 30 * time.Second

// parseDeadlineOverride 把 ctx 的 deadline 换算成下发给 docreader 的 Unix 秒。
// 余量不足（剩余时间已经短于 grace）时返回 ok=false：此时让 Go 侧的 ctx 超时
// 正常兜底，比让 Python 立刻中止、一页都不产出更有用。
func parseDeadlineOverride(ctx context.Context) (string, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return "", false
	}
	soft := deadline.Add(-parseDeadlineGrace)
	if time.Until(soft) <= 0 {
		return "", false
	}
	return strconv.FormatInt(soft.Unix(), 10), true
}

// withParseDeadline 复制一份 overrides 并写入解析截止时刻。
// 绝不就地改写入参 map —— 重试链路会复用同一份 overrides。
func withParseDeadline(ctx context.Context, overrides map[string]string) map[string]string {
	value, ok := parseDeadlineOverride(ctx)
	if !ok {
		return overrides
	}
	out := make(map[string]string, len(overrides)+1)
	for k, v := range overrides {
		out[k] = v
	}
	out[types.ParseDeadlineOverrideKey] = value
	return out
}

func (p *GRPCDocumentReader) Read(ctx context.Context, req *types.ReadRequest) (*types.ReadResult, error) {
	return p.ReadWithProgress(ctx, req, nil)
}

// ReadWithProgress 实现 interfaces.DocReaderWithProgress（FR-16 可选能力）：
// 边接收流式解析结果边回调进度，让「解析中」不再是一个没有百分比的黑盒。
// progress 为 nil 时行为与 Read 完全一致。
func (p *GRPCDocumentReader) ReadWithProgress(
	ctx context.Context, req *types.ReadRequest, progress interfaces.ParseProgressFunc,
) (*types.ReadResult, error) {
	p.mu.RLock()
	client := p.client
	p.mu.RUnlock()
	if client == nil {
		return nil, errNotConnected
	}

	protoReq := &proto.ReadRequest{
		FileContent: req.FileContent,
		FileName:    req.FileName,
		FileType:    req.FileType,
		Url:         req.URL,
		Title:       req.Title,
		RequestId:   req.RequestID,
		Config: &proto.ReadConfig{
			ParserEngine: req.ParserEngine,
			// FR-16：把时长上界随请求下发，Python 侧据此在逐页渲染循环里自行中止，
			// 保留已解析部分而不是让整个调用被 ctx 掐断。
			ParserEngineOverrides: withParseDeadline(ctx, req.ParserEngineOverrides),
		},
	}

	// Use the streaming RPC so documents with many page images (large scanned
	// PDFs) are not capped by the unary message-size limit. The meta frame
	// arrives first, followed by one frame per image.
	result, err := p.readStream(ctx, client, protoReq, progress)
	if err != nil {
		// An older docreader build may not implement ReadStream. Fall back to
		// the unary Read RPC so a version-skewed deployment still parses
		// documents (small/medium docs only — the unary path remains capped by
		// the gRPC message-size limit, which is exactly what streaming avoids).
		if status.Code(err) == codes.Unimplemented {
			logger.Warnf(ctx, "docreader ReadStream unimplemented, falling back to unary Read: %v", err)
			return p.readUnary(ctx, client, protoReq)
		}
		return nil, err
	}
	return result, nil
}

// readStream consumes the server-streaming ReadStream RPC: one meta frame
// followed by one frame per image. Errors are returned verbatim so the caller
// can inspect the gRPC status code (e.g. Unimplemented) for fallback.
//
// FR-16：解析被资源上界中止时，服务端会在最后一帧图像之后**再补一个 meta 帧**，
// 只承载 pages_done / pages_total / incomplete / incomplete_reason。因此这里对
// meta 帧是「首帧全量赋值、后续帧只合并进度与未完成标记」，绝不能让尾帧把已经
// 收到的正文和 metadata 覆盖成空。
func (p *GRPCDocumentReader) readStream(
	ctx context.Context,
	client proto.DocReaderClient,
	protoReq *proto.ReadRequest,
	progress interfaces.ParseProgressFunc,
) (*types.ReadResult, error) {
	stream, err := client.ReadStream(ctx, protoReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC ReadStream failed: %w", err)
	}

	result := &types.ReadResult{}
	gotMeta := false
	imageTotal := 0
	imagesDone := 0
	for {
		frame, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return nil, fmt.Errorf("gRPC ReadStream recv failed: %w", recvErr)
		}

		if meta := frame.GetMeta(); meta != nil {
			if !gotMeta {
				gotMeta = true
				result.MarkdownContent = meta.GetMarkdownContent()
				result.ImageDirPath = meta.GetImageDirPath()
				// 复制一份：applyIncompleteMeta 之后要往里写「解析未完成」标注，
				// 不能就地改写 protobuf 消息持有的 map。
				result.Metadata = copyStringMap(meta.GetMetadata())
				result.Error = meta.GetError()
				result.SourceBlocks = decodeSourceBlocks(meta.GetSourceBlocks())
				imageTotal = int(meta.GetImageCount())
				if n := meta.GetImageCount(); n > 0 {
					result.ImageRefs = make([]types.ImageRef, 0, n)
				}
				// 首帧就带上 pages_total，便于调用方在解析刚开始时显示总量。
				result.PagesTotal = int(meta.GetPagesTotal())
				continue
			}
			// 尾部帧：只合并进度与「解析未完成」标注。
			applyIncompleteMeta(result, meta)
			continue
		}

		if img := frame.GetImage(); img != nil {
			result.ImageRefs = append(result.ImageRefs, types.ImageRef{
				Filename:    img.GetFilename(),
				OriginalRef: img.GetOriginalRef(),
				MimeType:    img.GetMimeType(),
				StorageKey:  img.GetStorageKey(),
				ImageData:   img.GetImageData(),
			})
			imagesDone++
			if progress != nil {
				progress(imagesDone, imageTotal)
			}
		}
	}

	if !gotMeta {
		return nil, fmt.Errorf("gRPC ReadStream returned no metadata frame")
	}
	if result.ParseIncomplete {
		logger.Warnf(ctx,
			"docreader returned a partial parse for %q: reason=%s pages=%d/%d images=%d/%d",
			protoReq.GetFileName(), result.ParseIncompleteReason,
			result.PagesDone, result.PagesTotal, imagesDone, imageTotal)
	}
	return result, nil
}

// copyStringMap 复制一份 string map；入参为 nil 时返回 nil（保持「无 metadata」语义）。
func copyStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// applyIncompleteMeta 把一个只承载进度 / 未完成标记的 meta 帧合并进结果，
// 同时把标注镜像到 Metadata，让下游只看 result.Metadata 的旧代码也能感知降级。
func applyIncompleteMeta(result *types.ReadResult, meta *proto.ReadStreamMeta) {
	if n := meta.GetPagesTotal(); n > 0 {
		result.PagesTotal = int(n)
	}
	result.PagesDone = int(meta.GetPagesDone())
	if !meta.GetIncomplete() {
		return
	}
	result.ParseIncomplete = true
	result.ParseIncompleteReason = meta.GetIncompleteReason()
	if result.Metadata == nil {
		result.Metadata = make(map[string]string, 4)
	}
	result.Metadata["parse_incomplete"] = "true"
	result.Metadata["parse_incomplete_reason"] = meta.GetIncompleteReason()
	result.Metadata["pages_done"] = strconv.Itoa(int(meta.GetPagesDone()))
	result.Metadata["pages_total"] = strconv.Itoa(int(meta.GetPagesTotal()))
}

// readUnary calls the legacy unary Read RPC. Used only as a compatibility
// fallback when the connected docreader does not implement ReadStream.
func (p *GRPCDocumentReader) readUnary(
	ctx context.Context, client proto.DocReaderClient, protoReq *proto.ReadRequest,
) (*types.ReadResult, error) {
	resp, err := client.Read(ctx, protoReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC Read failed: %w", err)
	}

	result := &types.ReadResult{
		MarkdownContent: resp.GetMarkdownContent(),
		ImageDirPath:    resp.GetImageDirPath(),
		Metadata:        resp.GetMetadata(),
		Error:           resp.GetError(),
		SourceBlocks:    decodeSourceBlocks(resp.GetSourceBlocks()),
		// FR-16：unary 路径同样可能带回「解析未完成」标注。
		ParseIncomplete:       resp.GetIncomplete(),
		ParseIncompleteReason: resp.GetIncompleteReason(),
	}
	if refs := resp.GetImageRefs(); len(refs) > 0 {
		result.ImageRefs = make([]types.ImageRef, 0, len(refs))
		for _, img := range refs {
			result.ImageRefs = append(result.ImageRefs, types.ImageRef{
				Filename:    img.GetFilename(),
				OriginalRef: img.GetOriginalRef(),
				MimeType:    img.GetMimeType(),
				StorageKey:  img.GetStorageKey(),
				ImageData:   img.GetImageData(),
			})
		}
	}
	return result, nil
}

func (p *GRPCDocumentReader) ListEngines(ctx context.Context, overrides map[string]string) ([]types.ParserEngineInfo, error) {
	p.mu.RLock()
	client := p.client
	p.mu.RUnlock()
	if client == nil {
		return nil, errNotConnected
	}

	resp, err := client.ListEngines(ctx, &proto.ListEnginesRequest{ConfigOverrides: overrides})
	if err != nil {
		return nil, fmt.Errorf("gRPC ListEngines failed: %w", err)
	}

	result := make([]types.ParserEngineInfo, 0, len(resp.GetEngines()))
	for _, e := range resp.GetEngines() {
		result = append(result, types.ParserEngineInfo{
			Name:              e.GetName(),
			Description:       e.GetDescription(),
			FileTypes:         e.GetFileTypes(),
			Available:         e.GetAvailable(),
			UnavailableReason: e.GetUnavailableReason(),
		})
	}
	return result, nil
}
