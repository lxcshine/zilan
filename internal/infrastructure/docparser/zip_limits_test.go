package docparser

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/utils"
)

// buildZip 在内存里造一个结果包，供各上界测试复用。
func buildZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip.Create(%q): %v", name, err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func openZip(t *testing.T, data []byte) *zip.Reader {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	return zr
}

// 整包下载超限：必须拒绝，并且错误里要写清上界与实际读到的字节数。
func TestReadResultPackage_RejectsOversized(t *testing.T) {
	const limit = 1024
	_, err := readResultPackage(strings.NewReader(strings.Repeat("a", limit+1)), limit)
	if !errors.Is(err, errResultPackageTooLarge) {
		t.Fatalf("err = %v, want errResultPackageTooLarge", err)
	}
	for _, want := range []string{"1024 bytes", "1025 bytes", resultPackageLimitEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// 刚好等于上界不算超限。
func TestReadResultPackage_AllowsExactlyAtLimit(t *testing.T) {
	const limit = 1024
	got, err := readResultPackage(strings.NewReader(strings.Repeat("a", limit)), limit)
	if err != nil {
		t.Fatalf("readResultPackage(at limit) = %v, want nil", err)
	}
	if len(got) != limit {
		t.Errorf("read %d bytes, want %d", len(got), limit)
	}
}

// limit<=0 回落到默认上界，而不是变成无上限。
func TestReadResultPackage_FallsBackToDefault(t *testing.T) {
	got, err := readResultPackage(strings.NewReader("zip-bytes"), 0)
	if err != nil {
		t.Fatalf("readResultPackage(_, 0) = %v, want nil", err)
	}
	if string(got) != "zip-bytes" {
		t.Errorf("body = %q", got)
	}
	if maxResultPackageBytes() <= 0 {
		t.Errorf("default package limit = %d, want positive", maxResultPackageBytes())
	}
}

// 单条目声明大小超限：不解压就拒绝。
func TestReadZipEntryCapped_RejectsDeclaredOversize(t *testing.T) {
	content := []byte(strings.Repeat("x", 2000))
	zr := openZip(t, buildZip(t, map[string][]byte{"doc/full.md": content}))
	entry := zr.File[0]

	_, err := readZipEntryCapped(entry, 1024)
	if !errors.Is(err, errResultPackageTooLarge) {
		t.Fatalf("err = %v, want errResultPackageTooLarge", err)
	}
	for _, want := range []string{"declares 2000 bytes", "limit is 1024 bytes", "doc/full.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// 条目大小刚好等于上界时正常读出。
func TestReadZipEntryCapped_AllowsExactlyAtLimit(t *testing.T) {
	content := []byte(strings.Repeat("x", 2000))
	zr := openZip(t, buildZip(t, map[string][]byte{"doc/full.md": content}))

	got, err := readZipEntryCapped(zr.File[0], int64(len(content)))
	if err != nil {
		t.Fatalf("readZipEntryCapped(at limit) = %v, want nil", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("read %d bytes, want %d", len(got), len(content))
	}
}

// 伪造（低报）UncompressedSize64 不能绕过上界：要么被 LimitReader 拦住，
// 要么被 archive/zip 自身的大小校验拦住，总之不会交出超限数据。
func TestReadZipEntryCapped_ForgedUncompressedSize(t *testing.T) {
	content := []byte(strings.Repeat("x", 4096))
	zr := openZip(t, buildZip(t, map[string][]byte{"doc/full.md": content}))
	entry := zr.File[0]
	// 声明值撒谎：真实 4096 字节，头部只写 16 字节。
	entry.UncompressedSize64 = 16

	got, err := readZipEntryCapped(entry, 1024)
	if err == nil {
		t.Fatalf("forged header returned %d bytes, want an error", len(got))
	}
	if got != nil {
		t.Errorf("forged header returned %d bytes of data, want nil", len(got))
	}
}

// 结构预检：条目数、解压后总量、压缩比、嵌套归档。
func TestCheckZipArchiveLimits(t *testing.T) {
	synthetic := func(files ...zip.FileHeader) *zip.Reader {
		zr := &zip.Reader{}
		for i := range files {
			zr.File = append(zr.File, &zip.File{FileHeader: files[i]})
		}
		return zr
	}
	header := func(name string, compressed, uncompressed uint64) zip.FileHeader {
		return zip.FileHeader{Name: name, CompressedSize64: compressed, UncompressedSize64: uncompressed}
	}

	t.Run("entry count", func(t *testing.T) {
		t.Setenv(zipEntriesLimitEnv, "2")
		zr := synthetic(
			header("a.md", 100, 200),
			header("b.md", 100, 200),
			header("c.md", 100, 200),
		)
		err := checkZipArchiveLimits(zr)
		if !errors.Is(err, errResultPackageTooLarge) {
			t.Fatalf("err = %v, want errResultPackageTooLarge", err)
		}
		for _, want := range []string{"3 entries", "limit is 2 entries", zipEntriesLimitEnv} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})

	t.Run("total uncompressed", func(t *testing.T) {
		t.Setenv(zipTotalUncompressedEnv, "1") // 1 MiB
		zr := synthetic(header("a.md", 100, 2<<20))
		err := checkZipArchiveLimits(zr)
		if !errors.Is(err, errResultPackageTooLarge) {
			t.Fatalf("err = %v, want errResultPackageTooLarge", err)
		}
		if !strings.Contains(err.Error(), "uncompressed total exceeds limit") {
			t.Errorf("error %q does not explain which limit was hit", err)
		}
	})

	t.Run("compression ratio", func(t *testing.T) {
		t.Setenv(zipCompressionRatioEnv, "200")
		// 压缩比 1000，且压缩后大小超过比值判定的最小门槛。
		zr := synthetic(header("bomb.md", 8192, 8192*1000))
		err := checkZipArchiveLimits(zr)
		if !errors.Is(err, errResultPackageTooLarge) {
			t.Fatalf("err = %v, want errResultPackageTooLarge", err)
		}
		for _, want := range []string{"compression ratio 1000", "limit 200"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})

	t.Run("small entry skips ratio check", func(t *testing.T) {
		// 压缩后不足 zipRatioMinCompressedBytes 的小条目不做比值判定，避免误杀。
		zr := synthetic(header("small.md", 64, 64000))
		if err := checkZipArchiveLimits(zr); err != nil {
			t.Fatalf("small entry rejected: %v", err)
		}
	})

	t.Run("nested archive", func(t *testing.T) {
		zr := synthetic(header("inner.zip", 100, 200))
		err := checkZipArchiveLimits(zr)
		if !errors.Is(err, errResultPackageTooLarge) {
			t.Fatalf("err = %v, want errResultPackageTooLarge", err)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("nesting depth limit is %d", maxZipNestingDepth)) {
			t.Errorf("error %q does not mention the nesting limit", err)
		}
	})

	t.Run("normal package passes", func(t *testing.T) {
		zr := synthetic(
			header("auto/full.md", 4096, 20480),
			header("auto/images/a.png", 50000, 50000),
		)
		if err := checkZipArchiveLimits(zr); err != nil {
			t.Fatalf("normal package rejected: %v", err)
		}
	})
}

// 图片总量预算：超出即整体拒绝，而不是悄悄少给几张图。
func TestDownloadAndExtractZip_ImageBudgetExceeded(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1,::1,localhost")
	utils.ResetSSRFWhitelistForTest()
	t.Cleanup(utils.ResetSSRFWhitelistForTest)

	// 两张各 4096 字节的图，预算只给 6144 字节：第二张必然超预算。
	t.Setenv(zipImagesTotalLimitEnv, "6144")
	image := bytes.Repeat([]byte{0xAB}, 4096)
	zipBytes := buildZip(t, map[string][]byte{
		"auto/full.md":        []byte("# t\n\n![](images/a.png)\n\n![](images/b.png)\n"),
		"auto/images/a.png":   image,
		"auto/images/b.png":   image,
		"auto/_content.jsonl": []byte("{}"),
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	_, _, _, err := downloadAndExtractZip(context.Background(), srv.URL+"/result.zip")
	if !errors.Is(err, errResultPackageTooLarge) {
		t.Fatalf("err = %v, want errResultPackageTooLarge", err)
	}
	for _, want := range []string{"images reached", "8192 bytes", "6144 bytes", zipImagesTotalLimitEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// 预算充足时同一个包必须能正常解析出两张图。
func TestDownloadAndExtractZip_WithinBudget(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1,::1,localhost")
	utils.ResetSSRFWhitelistForTest()
	t.Cleanup(utils.ResetSSRFWhitelistForTest)
	t.Setenv(zipImagesTotalLimitEnv, "")

	image := bytes.Repeat([]byte{0xAB}, 4096)
	zipBytes := buildZip(t, map[string][]byte{
		"auto/full.md":      []byte("# t\n\n![](images/a.png)\n\n![](images/b.png)\n"),
		"auto/images/a.png": image,
		"auto/images/b.png": image,
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	md, refs, _, err := downloadAndExtractZip(context.Background(), srv.URL+"/result.zip")
	if err != nil {
		t.Fatalf("downloadAndExtractZip: %v", err)
	}
	if !strings.Contains(md, "images/a.png") {
		t.Errorf("markdown = %q", md)
	}
	if len(refs) != 2 {
		t.Errorf("got %d image refs, want 2", len(refs))
	}
}

// 结果包整包超限也要在解析前就被拒绝。
func TestDownloadAndExtractZip_PackageTooLarge(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1,::1,localhost")
	utils.ResetSSRFWhitelistForTest()
	t.Cleanup(utils.ResetSSRFWhitelistForTest)
	t.Setenv(resultPackageLimitEnv, "1") // 1 MiB

	// 用不可压缩的随机内容，否则 2 MiB 正文压完只剩几 KB，压不到上界。
	payload := make([]byte, 2<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	zipBytes := buildZip(t, map[string][]byte{"auto/full.md": payload})
	if len(zipBytes) <= 1<<20 {
		t.Fatalf("test fixture too small: %d bytes", len(zipBytes))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	_, _, _, err := downloadAndExtractZip(context.Background(), srv.URL+"/result.zip")
	if !errors.Is(err, errResultPackageTooLarge) {
		t.Fatalf("err = %v, want errResultPackageTooLarge", err)
	}
}

// 上界必须可配置，非法值回落默认。
func TestZipLimitEnvOverrides(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		t.Setenv(resultPackageLimitEnv, "2")
		t.Setenv(zipEntryLimitEnv, "4")
		t.Setenv(zipImagesTotalLimitEnv, "8192")
		if got := maxResultPackageBytes(); got != 2<<20 {
			t.Errorf("maxResultPackageBytes() = %d, want %d", got, 2<<20)
		}
		if got := maxZipEntryBytes(); got != 4<<20 {
			t.Errorf("maxZipEntryBytes() = %d, want %d", got, 4<<20)
		}
		if got := maxZipImagesTotalBytes(); got != 8192 {
			t.Errorf("maxZipImagesTotalBytes() = %d, want 8192", got)
		}
	})
	for _, bad := range []string{"", " ", "0", "-1", "invalid", "2GiB", "1.5"} {
		t.Run("invalid "+bad, func(t *testing.T) {
			t.Setenv(zipImagesTotalLimitEnv, bad)
			if got := maxZipImagesTotalBytes(); got != defaultMaxZipImagesTotalBytes {
				t.Errorf("maxZipImagesTotalBytes(%q) = %d, want default %d", bad, got, defaultMaxZipImagesTotalBytes)
			}
		})
	}
}
