package utils

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var defaultHTTPClient = &http.Client{Timeout: 60 * time.Second}

// DownloadBytes fetches the content at the given HTTP(S) URL and returns the
// raw bytes. It reuses a package-level http.Client with a 60-second timeout.
// 响应体受 MAX_FILE_SIZE_MB 上界约束（FR-07）：先看 Content-Length 声明值，
// 再用 LimitReader 兜底，因为声明值可以被伪造或缺失（chunked）。
func DownloadBytes(url string) ([]byte, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("unsupported URL scheme: %s", url)
	}
	resp, err := defaultHTTPClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	maxBytes := GetMaxFileSize()
	if resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("download size exceeds limit: Content-Length %d bytes > limit %d bytes (MAX_FILE_SIZE_MB)",
			resp.ContentLength, maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("download size exceeds limit: body %d bytes > limit %d bytes (MAX_FILE_SIZE_MB)",
			len(data), maxBytes)
	}
	return data, nil
}
