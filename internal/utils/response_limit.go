package utils

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// FR-07 外部输入资源边界：外部 HTTP 响应体上界的共享实现。
//
// models/{chat,embedding,rerank,vlm}、infrastructure/web_search、
// infrastructure/docparser 三处都从这里取上界与读取函数，避免出现第四份
// 同构实现（上游 WeKnora 就存在 4 份，各自漂移）。
//
// 统一手法：io.ReadAll(io.LimitReader(body, limit+1)) —— 多读 1 字节，
// 这样"刚好等于上限"与"超限"可以被区分开；超限一律报错而不截断，因为
// 被截断的响应体会被当成完整回复去解析，产出看似正常实则残缺的结果
// （PRD §5.3 第 2 条：FR-07 超限属正确性能力，必须拒绝不得降级放行）。
const (
	// DefaultJSONResponseBytes 控制面 JSON 响应体默认上界（16 MiB）。
	// 适用于任务状态轮询、检索结果元数据、模型配置等小报文。
	DefaultJSONResponseBytes int64 = 16 << 20

	// DefaultModelResponseBytes 模型 provider 非流式响应体默认上界（64 MiB）。
	// 流式响应逐块装配，不在此列。
	DefaultModelResponseBytes int64 = 64 << 20

	// MaxErrorPreviewBytes 失败响应写入日志或错误串的预览上界（4 KiB）。
	// 没有它，一个返回巨大错误页的端点就能把无界内容注入日志与 API 错误响应。
	MaxErrorPreviewBytes int64 = 4 << 10
)

// 上界的环境变量覆盖开关（单位 MiB）。非法值与非正值一律回落默认值，
// 保证任何配置错误都只会更严格而不会放开限制。
const (
	jsonResponseLimitEnv  = "WEKNORA_MAX_JSON_RESPONSE_MB"
	modelResponseLimitEnv = "WEKNORA_MAX_MODEL_RESPONSE_MB"
)

// ErrResponseTooLarge 标记响应体超出上界。它是确定性的：同一请求会返回
// 同样的超大响应，因此调用方不应重试。
var ErrResponseTooLarge = errors.New("response exceeds maximum size")

// JSONResponseLimit 返回控制面 JSON 响应体上界，可用
// WEKNORA_MAX_JSON_RESPONSE_MB 覆盖，默认 16 MiB。
func JSONResponseLimit() int64 {
	return EnvMBLimit(jsonResponseLimitEnv, DefaultJSONResponseBytes)
}

// ModelResponseLimit 返回模型 provider 非流式响应体上界，可用
// WEKNORA_MAX_MODEL_RESPONSE_MB 覆盖，默认 64 MiB。
func ModelResponseLimit() int64 {
	return EnvMBLimit(modelResponseLimitEnv, DefaultModelResponseBytes)
}

// EnvMBLimit 读取以 MiB 为单位的环境变量上界；未设置、非法或非正值时
// 回落 fallback。
func EnvMBLimit(key string, fallback int64) int64 {
	mb, ok := parseEnvInt(key)
	if !ok {
		return fallback
	}
	return mb << 20
}

// EnvByteLimit 读取以字节为单位的环境变量上界；未设置、非法或非正值时
// 回落 fallback。
func EnvByteLimit(key string, fallback int64) int64 {
	v, ok := parseEnvInt(key)
	if !ok {
		return fallback
	}
	return v
}

// parseEnvInt 只接受十进制正整数，不接受 "2GiB"、"1.5" 这类写法：
// 解析失败的配置必须回落到安全默认值，而不是被静默放大。
func parseEnvInt(key string) (int64, bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// ReadResponseCapped 在上界内读取响应体，超限即报错而不是先缓冲再判断。
// limit 非正时回落 DefaultJSONResponseBytes。
//
// 错误信息按 PRD FR-07 要求给出"哪一项超限、当前值与上界各是多少"。
func ReadResponseCapped(body io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultJSONResponseBytes
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: body exceeds limit %d bytes (read %d bytes)",
			ErrResponseTooLarge, limit, len(data))
	}
	return data, nil
}

// ReadErrorPreview 只读取失败响应体的一小段用于日志或错误串。
// 这里是截断而非拒绝：调用方已经在错误路径上，大错误体的开头才是诊断价值所在。
// 读取错误被丢弃，因为调用方紧接着就会报告状态码。
func ReadErrorPreview(body io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(body, MaxErrorPreviewBytes))
	return TrimPartialUTF8(string(data))
}

// ErrorPreviewString 把已读入的失败响应体渲染成错误信息片段，
// 长度受 MaxErrorPreviewBytes 约束，且不会在 UTF-8 字符中间断开。
func ErrorPreviewString(body []byte) string {
	detail := strings.TrimSpace(string(body))
	if int64(len(detail)) > MaxErrorPreviewBytes {
		detail = detail[:MaxErrorPreviewBytes]
	}
	return TrimPartialUTF8(detail)
}

// TrimPartialUTF8 丢弃结尾的不完整字符。io.LimitReader 按字节数停止，
// 中文错误体可能在字符中间被截断，直接入日志会产生非法 UTF-8。
func TrimPartialUTF8(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
