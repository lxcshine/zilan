package docparser

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/utils"
)

// bigReader 按需产出字节，避免为了测超限真的在内存里堆一个大 body。
type bigReader struct {
	remaining int64
}

func (r *bigReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	for i := int64(0); i < n; i++ {
		p[i] = 'a'
	}
	r.remaining -= n
	return int(n), nil
}

// 响应体超限必须报错而不是截断：被截断的文档会被当成完整回复去解析。
func TestReadCapped_RejectsOversized(t *testing.T) {
	const limit = 4096
	src := &bigReader{remaining: limit + 1}
	_, err := readCapped(src, limit)
	if !errors.Is(err, utils.ErrResponseTooLarge) {
		t.Fatalf("err = %v, want utils.ErrResponseTooLarge", err)
	}
	for _, want := range []string{"4096 bytes", "4097 bytes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if consumed := limit + 1 - src.remaining; consumed > limit+1 {
		t.Errorf("consumed %d bytes, want at most %d", consumed, limit+1)
	}
}

func TestReadCapped_AllowsExactlyAtLimit(t *testing.T) {
	const limit = 4096
	got, err := readCapped(strings.NewReader(strings.Repeat("a", limit)), limit)
	if err != nil {
		t.Fatalf("readCapped(at limit) = %v, want nil", err)
	}
	if len(got) != limit {
		t.Errorf("read %d bytes, want %d", len(got), limit)
	}
}

// limit<=0 回落到默认控制面上界。
func TestReadCapped_FallsBackToDefault(t *testing.T) {
	got, err := readCapped(strings.NewReader(`{"ok":true}`), 0)
	if err != nil {
		t.Fatalf("readCapped(_, 0) = %v, want nil", err)
	}
	if string(got) != `{"ok":true}` {
		t.Errorf("body = %q", got)
	}
}

// 失败响应的预览是截断而非拒绝，且长度受 maxErrorPreviewBytes 约束。
func TestReadErrorPreview_Bounded(t *testing.T) {
	got := readErrorPreview(&bigReader{remaining: 1 << 20})
	if int64(len(got)) > utils.MaxErrorPreviewBytes {
		t.Errorf("preview len = %d, want <= %d", len(got), utils.MaxErrorPreviewBytes)
	}
}

// 上界之间的关系：承载解析结果的响应上界必须覆盖"图片全部内联成 base64"
// 的合法最坏情况，否则正常的云端解析结果会被自己的上界拒掉。
func TestResultResponseLimitCoversInlineImages(t *testing.T) {
	t.Setenv(resultPackageLimitEnv, "")
	t.Setenv(zipImagesTotalLimitEnv, "")
	t.Setenv(zipEntryLimitEnv, "")
	t.Setenv(jsonResponseLimitEnvForTest(), "")

	need := maxZipImagesTotalBytes()*4/3 + maxZipEntryBytes()
	if got := maxResultResponseBytes(); got < need {
		t.Errorf("maxResultResponseBytes() = %d, want at least %d (inline images + markdown)", got, need)
	}
	if got := maxResultResponseBytes(); got <= maxJSONResponseBytes() {
		t.Errorf("maxResultResponseBytes() = %d, want > control-plane limit %d", got, maxJSONResponseBytes())
	}
}

// jsonResponseLimitEnvForTest 返回控制面上界对应的环境变量名。
// 放在测试里而不是生产代码里：生产侧只需要 utils.JSONResponseLimit()。
func jsonResponseLimitEnvForTest() string {
	return "WEKNORA_MAX_JSON_RESPONSE_MB"
}
