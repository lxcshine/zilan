package docparser

import (
	"io"

	"github.com/Tencent/WeKnora/internal/utils"
)

// 解析 API 的响应分两类：一类是控制面小 JSON（任务号、批次状态、引擎列表），
// 另一类是承载解析结果本身的正文，有时把抽取出的图片全部以 base64 内联。
// 两类过去都用无上限的 io.ReadAll 读取（FR-07）。
//
// 上界与读取手法统一由 internal/utils 提供（避免在仓库里再养一份同构实现），
// 这里只保留解析引擎侧的语义名，便于与上游对齐、便于 grep。

// maxJSONResponseBytes 返回控制面 JSON 响应体上界，默认 16 MiB，
// 可用 WEKNORA_MAX_JSON_RESPONSE_MB 覆盖。任务提交与状态轮询只有几百字节，
// 16 MiB 远高于它们的正常返回。
func maxJSONResponseBytes() int64 {
	return utils.JSONResponseLimit()
}

// maxResultResponseBytes 返回承载解析结果的响应体上界。
// 同一份结果走 zip 下载时受 maxResultPackageBytes 约束，内联成 base64
// 时不应反而没有约束：512 MiB 图片 base64 后约 683 MiB，1 GiB 的包上界
// 仍能覆盖合法最坏情况外加一个满尺寸 markdown 条目。
func maxResultResponseBytes() int64 {
	return maxResultPackageBytes()
}

// readCapped 读取响应体，超限即报错而不是截断——被截断的文档会被当成
// 完整回复去解析。limit 非正时回落 utils 的默认值。
func readCapped(body io.Reader, limit int64) ([]byte, error) {
	return utils.ReadResponseCapped(body, limit)
}

// readErrorPreview 只读取失败响应体的一小段用于日志或错误串。
// 这里截断而非拒绝：调用方已在错误路径上，大错误体的开头才是诊断价值所在。
func readErrorPreview(body io.Reader) string {
	return utils.ReadErrorPreview(body)
}
