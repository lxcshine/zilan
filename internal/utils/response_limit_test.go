package utils

import (
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

// fillerReader 按需产出字节，避免为了测超限而在内存里真的堆一个大 body。
type fillerReader struct {
	remaining int64
}

func (r *fillerReader) Read(p []byte) (int, error) {
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

// 超限必须报错而不是截断，且错误信息要同时给出上界与实际读到的字节数
// （PRD FR-07：说明哪一项超限、当前值与上界各是多少）。
func TestReadResponseCapped_RejectsOversizedBody(t *testing.T) {
	const limit = 1024
	_, err := ReadResponseCapped(&fillerReader{remaining: limit + 1}, limit)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("ReadResponseCapped(over limit) = %v, want ErrResponseTooLarge", err)
	}
	for _, want := range []string{"1024 bytes", "1025 bytes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// 刚好等于上界不算超限：多读 1 字节的手法必须能区分这两种情况。
func TestReadResponseCapped_AllowsExactlyAtLimit(t *testing.T) {
	const limit = 1024
	got, err := ReadResponseCapped(strings.NewReader(strings.Repeat("a", limit)), limit)
	if err != nil {
		t.Fatalf("ReadResponseCapped(at limit) = %v, want nil", err)
	}
	if len(got) != limit {
		t.Errorf("read %d bytes, want %d", len(got), limit)
	}
}

// 非正 limit 回落到安全默认值，而不是变成"无上限"。
func TestReadResponseCapped_FallsBackToDefault(t *testing.T) {
	got, err := ReadResponseCapped(strings.NewReader(`{"ok":true}`), 0)
	if err != nil {
		t.Fatalf("ReadResponseCapped(_, 0) = %v, want nil", err)
	}
	if string(got) != `{"ok":true}` {
		t.Errorf("body = %q", got)
	}
}

// 默认上界必须在超限前就真的把读取截住，不能先把整个 body 读进内存。
func TestReadResponseCapped_DoesNotBufferBeyondLimit(t *testing.T) {
	const limit = 4096
	src := &fillerReader{remaining: limit * 8}
	if _, err := ReadResponseCapped(src, limit); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
	if consumed := limit*8 - src.remaining; consumed > limit+1 {
		t.Errorf("reader consumed %d bytes, want at most %d (limit+1)", consumed, limit+1)
	}
}

func TestReadErrorPreview_TruncatesInsteadOfRefusing(t *testing.T) {
	got := ReadErrorPreview(&fillerReader{remaining: 1 << 20})
	if int64(len(got)) > MaxErrorPreviewBytes {
		t.Errorf("preview len = %d, want <= %d", len(got), MaxErrorPreviewBytes)
	}
	if got == "" {
		t.Error("preview is empty, want the start of the body")
	}
}

// 预览按字节截断，中文错误体可能停在字符中间；必须修掉，否则日志里出现非法 UTF-8。
func TestReadErrorPreview_TrimsPartialUTF8(t *testing.T) {
	got := ReadErrorPreview(strings.NewReader(strings.Repeat("错", int(MaxErrorPreviewBytes))))
	if !isValidUTF8(got) {
		t.Errorf("preview is not valid UTF-8: %q", got)
	}
	if int64(len(got)) > MaxErrorPreviewBytes {
		t.Errorf("preview len = %d, want <= %d", len(got), MaxErrorPreviewBytes)
	}
}

func TestErrorPreviewString_BoundsAndTrims(t *testing.T) {
	got := ErrorPreviewString([]byte(strings.Repeat("错", int(MaxErrorPreviewBytes)*2)))
	if int64(len(got)) > MaxErrorPreviewBytes {
		t.Errorf("len = %d, want <= %d", len(got), MaxErrorPreviewBytes)
	}
	if !isValidUTF8(got) {
		t.Errorf("not valid UTF-8: %q", got)
	}
	if s := ErrorPreviewString([]byte("  trimmed  ")); s != "trimmed" {
		t.Errorf("ErrorPreviewString() = %q, want %q", s, "trimmed")
	}
}

func isValidUTF8(s string) bool {
	return utf8.ValidString(s)
}

// 上界必须可配置；非法值只能回落到安全默认值，绝不能放开限制。
func TestEnvLimits(t *testing.T) {
	const key = "TEST_RESPONSE_LIMIT_MB"
	t.Run("override", func(t *testing.T) {
		t.Setenv(key, "2")
		if got := EnvMBLimit(key, 16<<20); got != 2<<20 {
			t.Errorf("EnvMBLimit() = %d, want %d", got, 2<<20)
		}
	})
	t.Run("unset falls back", func(t *testing.T) {
		t.Setenv(key, "")
		if got := EnvMBLimit(key, 16<<20); got != 16<<20 {
			t.Errorf("EnvMBLimit() = %d, want default %d", got, 16<<20)
		}
	})
	for _, bad := range []string{"0", "-1", "abc", "2GiB", "1.5", "9223372036854775808"} {
		t.Run("invalid "+bad, func(t *testing.T) {
			t.Setenv(key, bad)
			if got := EnvMBLimit(key, 16<<20); got != 16<<20 {
				t.Errorf("EnvMBLimit(%q) = %d, want default %d", bad, got, 16<<20)
			}
			if got := EnvByteLimit(key, 512<<20); got != 512<<20 {
				t.Errorf("EnvByteLimit(%q) = %d, want default %d", bad, got, 512<<20)
			}
		})
	}
	t.Run("bytes override", func(t *testing.T) {
		t.Setenv(key, "4096")
		if got := EnvByteLimit(key, 512<<20); got != 4096 {
			t.Errorf("EnvByteLimit() = %d, want 4096", got)
		}
	})
}

func TestJSONAndModelResponseLimitsDefaults(t *testing.T) {
	t.Setenv(jsonResponseLimitEnv, "")
	t.Setenv(modelResponseLimitEnv, "")
	if got := JSONResponseLimit(); got != DefaultJSONResponseBytes {
		t.Errorf("JSONResponseLimit() = %d, want %d", got, DefaultJSONResponseBytes)
	}
	if got := ModelResponseLimit(); got != DefaultModelResponseBytes {
		t.Errorf("ModelResponseLimit() = %d, want %d", got, DefaultModelResponseBytes)
	}
	t.Setenv(modelResponseLimitEnv, "8")
	if got := ModelResponseLimit(); got != 8<<20 {
		t.Errorf("ModelResponseLimit() = %d, want %d", got, 8<<20)
	}
}
