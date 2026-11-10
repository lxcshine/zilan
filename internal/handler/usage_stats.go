package handler

import (
	goerrors "errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// ---------------------------------------------------------------------------
// FR-12 每轮 Token 用量归因与分层看板（管理员侧）
//
// 路由挂在 /token-usage 组下并统一套 g.Admin()（见 router/routes_usage_stats.go）；
// 服务层再校验一次管理员身份并拒绝 API Key 主体，双重保险。
//
// 本文件的响应只有聚合数字：token 趋势、五层平均占比、压缩触发频率与缓存命中率，
// 不含任何会话正文、用户内容或消息 ID（PRD §5.1）。
// 单轮明细不走这里——它随消息持久化，由 GET /messages/:session_id/load 带出。
//
// 跨工作区聚合入口没有实现，因此也不存在需要叠加 RequireCrossTenantAccess 的路径。
// 只读聚合按 PRD §5.2 免审计，理由见 service/usage_stats.go 的文件头注释。
// ---------------------------------------------------------------------------

// UsageStatsHandler 承载管理员用量看板的 HTTP 层。
type UsageStatsHandler struct {
	usageService interfaces.TokenUsageService
}

// NewUsageStatsHandler 构造看板 handler。
func NewUsageStatsHandler(usageService interfaces.TokenUsageService) *UsageStatsHandler {
	return &UsageStatsHandler{usageService: usageService}
}

// tokenUsageTimeLayouts 是 start / end 参数接受的时间格式，按精确度递减排列。
// 允许只给日期，是因为看板的时间粒度最细到小时、最粗到天，
// 让管理员手敲 "2026-11-01" 比要求完整 RFC3339 更实用。
var tokenUsageTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02T15",
	"2006-01-02",
}

// parseTokenUsageTime 解析一个时间参数。空串返回零值（由服务层补齐默认窗口）；
// 非空但解析不出时返回 error，让 handler 直接回 400 而不是悄悄按默认窗口出数。
func parseTokenUsageTime(raw string) (time.Time, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return time.Time{}, nil
	}
	for _, layout := range tokenUsageTimeLayouts {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, goerrors.New("invalid time format: " + text)
}

// GetTokenUsageOverview godoc
// @Summary      工作区 Token 用量看板
// @Description  返回本工作区在时间窗内的每轮 token 用量聚合：按天/小时分桶的输入输出趋势、L0–L4 五层平均占比、压缩与截断的触发频率与释放量、prompt 缓存命中率（并按 unsupported/unreported/miss/hit 分桶）。仅工作区管理员可访问，响应只有聚合数字，不含会话正文
// @Tags         用量看板
// @Produce      json
// @Param        start        query     string  false  "起始时间（含），RFC3339 或 YYYY-MM-DD，默认窗口起点为 end 前 7 天"
// @Param        end          query     string  false  "结束时间（不含），RFC3339 或 YYYY-MM-DD，默认当前时间"
// @Param        granularity  query     string  false  "趋势分桶粒度：day（默认）或 hour"
// @Param        limit        query     int     false  "单次聚合最多载入的轮次数，默认 5000，最大 20000；超出时响应里的 truncated 为 true"
// @Success      200  {object}  map[string]interface{}  "用量看板"
// @Failure      400  {object}  errors.AppError         "请求参数错误"
// @Failure      403  {object}  errors.AppError         "需要工作区管理员"
// @Security     Bearer
// @Router       /token-usage/overview [get]
func (h *UsageStatsHandler) GetTokenUsageOverview(c *gin.Context) {
	ctx := c.Request.Context()

	start, err := parseTokenUsageTime(c.Query("start"))
	if err != nil {
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}
	end, err := parseTokenUsageTime(c.Query("end"))
	if err != nil {
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}
	granularity := strings.ToLower(strings.TrimSpace(c.Query("granularity")))
	// limit 非法时回落默认值：看板是只读聚合，一个敲错的数字不值得 400。
	limit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))

	overview, err := h.usageService.GetTokenUsageOverview(ctx, start, end, granularity, limit)
	if err != nil {
		h.failTokenUsage(c, err, "Failed to load token usage overview")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": overview})
}

// failTokenUsage 把服务层错误映射成 HTTP 错误。
func (h *UsageStatsHandler) failTokenUsage(c *gin.Context, err error, message string) {
	switch {
	case err == nil:
		return
	case goerrors.Is(err, types.ErrTokenUsageForbidden):
		c.Error(errors.NewForbiddenError("token usage dashboard requires a workspace administrator"))
	case goerrors.Is(err, types.ErrTokenUsageBadRequest):
		c.Error(errors.NewBadRequestError(err.Error()))
	default:
		logger.ErrorWithFields(c.Request.Context(), err, nil)
		c.Error(errors.NewInternalServerError(message).WithDetails(err.Error()))
	}
}
