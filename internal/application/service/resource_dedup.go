package service

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// FR-18 规则 5：相同图片字节的重复出现复用同一资源句柄，不重复存储。
// ReuseByContentHash 从上游 WeKnora-main internal/application/service/resource.go
// 照抄（拆为独立文件，便于并行开发下干净合入）：资源目录解析"这些字节
// 是否已经存过"。调用方（file 服务 SaveBytes）先问，命中即复用既有句柄，
// 未命中才落盘注册。
func (s *resourceCatalog) ReuseByContentHash(
	ctx context.Context, tenantID uint64, contentHash string,
) (string, bool, error) {
	// 分离清单还没有全部注册就写入重用引用，会让同一份数据被当作两条
	// 不同的资源展示（例如同一 PDF 的两页同一张 logo），所以空哈希直接
	// 视为未命中。
	if contentHash == "" {
		return "", false, nil
	}
	stored, err := s.repo.GetActiveByContentHash(ctx, tenantID, contentHash)
	if err != nil {
		return "", false, err
	}
	if stored == nil {
		return "", false, nil
	}
	return types.BuildResourcePath(stored.Handle), true, nil
}

// 确认 resourceCatalog 实现目录接口（含新增的按内容哈希复用）。
var _ interfaces.ResourceCatalog = (*resourceCatalog)(nil)
