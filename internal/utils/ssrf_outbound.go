package utils

import (
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// outboundSSRFValidationTTL 限制同源的出站校验结果可复用的时长。
// SSRFValidatingRoundTripper 位于每个出站请求的热路径上，而
// ValidateURLForSSRF 内部会做 DNS 解析；不缓存就等于每个模型调用
// 都多一次解析。DNS rebinding 仍由 SSRFSafeDialContext 在每次新建
// 连接时兜住，所以这里的缓存不会削弱连接层防护。
const outboundSSRFValidationTTL = 60 * time.Second

var (
	// ssrfOutboundCacheGen 在白名单变更时自增，使旧缓存整体失效。
	ssrfOutboundCacheGen atomic.Uint64
	ssrfOutboundCache    sync.Map // originKey -> *ssrfOutboundCacheEntry
)

type ssrfOutboundCacheEntry struct {
	gen       uint64
	err       error
	expiresAt time.Time
}

// validateURLForSSRFForOutbound 是出站热路径（RoundTripper、重定向校验）
// 使用的 URL 校验入口，按 origin 做短 TTL 缓存。
// 输入边界（handler、连接器构造请求处）仍应直接调用 ValidateURLForSSRF。
func validateURLForSSRFForOutbound(rawURL string) error {
	if rawURL == "" {
		return nil
	}
	key, ok := outboundSSRFCacheKey(rawURL)
	if !ok {
		// URL 解析不出 origin（畸形地址），不做缓存，直接走完整校验。
		return ValidateURLForSSRF(rawURL)
	}

	gen := ssrfOutboundCacheGen.Load()
	now := time.Now()
	if cached, ok := ssrfOutboundCache.Load(key); ok {
		entry := cached.(*ssrfOutboundCacheEntry)
		if entry.gen == gen && now.Before(entry.expiresAt) {
			return entry.err
		}
		ssrfOutboundCache.Delete(key)
	}

	err := ValidateURLForSSRF(rawURL)
	ssrfOutboundCache.Store(key, &ssrfOutboundCacheEntry{
		gen:       gen,
		err:       err,
		expiresAt: now.Add(outboundSSRFValidationTTL),
	})
	return err
}

// outboundSSRFCacheKey 以 scheme + host（含端口）为键：SSRF 判定只看
// 协议与目标主机，path/query 不参与，因此同源请求可以共享结果。
func outboundSSRFCacheKey(rawURL string) (string, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), true
}

// invalidateSSRFOutboundValidationCache 使全部出站校验缓存失效。
func invalidateSSRFOutboundValidationCache() {
	ssrfOutboundCacheGen.Add(1)
	ssrfOutboundCache.Range(func(key, _ any) bool {
		ssrfOutboundCache.Delete(key)
		return true
	})
}
