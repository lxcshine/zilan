package docparser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/utils"
)

// FR-14 ⑤：云端返回的预签名下载地址属于服务端可控输入，必须与既有的
// SSRF 防护同等对待；ctx 取消后也不能继续下载（FR-14 ④）。

func TestCloudResultDownloadCancellation(t *testing.T) {
	allowLoopbackSSRF(t)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("cancelled download reached server")
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := downloadAndExtractZip(ctx, server.URL)
	require.True(t, errors.Is(err, context.Canceled), "download ignored cancellation: %v", err)
}

// 结果 zip 的地址由云端给出，指向内网时必须被拒绝，且属于确定性失败
// （不是网络抖动），上层不应重试。
func TestCloudResultDownloadRejectsInternalZipURL(t *testing.T) {
	utils.ResetSSRFWhitelistForTest()
	t.Cleanup(utils.ResetSSRFWhitelistForTest)

	for _, zipURL := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:9000/result.zip",
		"http://[::1]/result.zip",
	} {
		_, _, _, err := downloadAndExtractZip(context.Background(), zipURL)
		require.Error(t, err, zipURL)
		assert.True(t, strings.Contains(err.Error(), "SSRF"), "err = %v", err)
		assert.False(t, IsMinerUResultTooLarge(err), "SSRF rejections are not budget violations")
	}
}
