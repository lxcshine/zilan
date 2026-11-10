package web_search

import (
	"io"

	"github.com/Tencent/WeKnora/internal/utils"
)

// 搜索结果是一页命中项之上的 JSON 元数据；比这更大的响应体意味着端点
// 配错或被攻击，而不是"更多结果"（FR-07）。上界与读取手法统一由
// internal/utils 提供，这里保留与上游同名的薄封装，使各 provider 的
// 调用点写法一致、便于与上游 diff。

// maxJSONResponseBytes 返回 provider 响应体上界，默认 16 MiB，
// 可用 WEKNORA_MAX_JSON_RESPONSE_MB 覆盖。
func maxJSONResponseBytes() int64 {
	return utils.JSONResponseLimit()
}

// readCapped 在上界内读取响应体，超限直接报错而不是截断：
// 被截断的响应会被当成完整结果去解析。limit 非正时回落默认上界。
func readCapped(body io.Reader, limit int64) ([]byte, error) {
	return utils.ReadResponseCapped(body, limit)
}

// readErrorPreview 只读取失败响应体的一小段用于日志或错误串。
// 这里截断而非拒绝：调用方已在错误路径上，紧接着就会报告状态码。
func readErrorPreview(body io.Reader) string {
	return utils.ReadErrorPreview(body)
}
