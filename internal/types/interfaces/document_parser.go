package interfaces

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

// DocReader is the core interface for reading documents into markdown.
type DocReader interface {
	Read(ctx context.Context, req *types.ReadRequest) (*types.ReadResult, error)
}

// ParseProgressFunc 报告解析进度（FR-16）。done/total 的单位由实现决定，
// docreader 用「已收到的图像帧数 / meta 帧声明的总数」；total 为 0 表示总量未知，
// 此时调用方只能拿到单调递增的 done。回调必须快速返回且不阻塞解析。
type ParseProgressFunc func(done, total int)

// DocReaderWithProgress 是**可选能力**接口：能边解析边上报进度的实现才实现它。
//
// 刻意不去改 DocReader —— 它有 9 个实现（gRPC / HTTP / Simple / WeKnoraCloud /
// MinerU / PaddleOCR-VL / Marker / Docling 等），其中大多数是一次性拿到完整结果，
// 根本没有细粒度进度可报。调用方用 AsProgressReader 做类型断言探测，与 Python 侧
// getattr(parser, "parse_stream", None) 的探测风格保持一致。
type DocReaderWithProgress interface {
	DocReader
	ReadWithProgress(
		ctx context.Context,
		req *types.ReadRequest,
		progress ParseProgressFunc,
	) (*types.ReadResult, error)
}

// AsProgressReader 探测 reader 是否支持进度上报；不支持时返回 nil。
// progress 为 nil 时实现方应等价于普通 Read（不产生额外开销）。
func AsProgressReader(reader DocReader) DocReaderWithProgress {
	if pr, ok := reader.(DocReaderWithProgress); ok {
		return pr
	}
	return nil
}

// DocumentReader extends DocReader with transport lifecycle management
// and remote engine discovery. Used by gRPC/HTTP clients that talk to
// the Python docreader service.
type DocumentReader interface {
	DocReader
	Reconnect(addr string) error
	IsConnected() bool
	// ListEngines queries the remote docreader for its registered parser engines.
	// Returns engines the remote service supports, allowing auto-discovery of
	// newly added engines without Go code changes.
	ListEngines(ctx context.Context, overrides map[string]string) ([]types.ParserEngineInfo, error)
}
