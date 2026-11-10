package docparser

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

// FR-14 大 PDF 分片：qpdf 退出码语义、页码平移与偏移重算、qpdf 缺失时的
// 安全回落，以及中间产物（临时目录 / 分片文件）必须清理干净。

// testPDF 生成一个合法的、指定页数的最小 PDF。
func testPDF(pages int) []byte {
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	offset := []int{0}
	obj := func(n int, s string) {
		offset = append(offset, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", n, s)
	}
	obj(1, "<< /Type /Catalog /Pages 2 0 R >>")
	var kids strings.Builder
	for i := 0; i < pages; i++ {
		fmt.Fprintf(&kids, "%d 0 R ", i+3)
	}
	obj(2, fmt.Sprintf("<< /Type /Pages /Count %d /Kids [%s] >>", pages, kids.String()))
	for i := 0; i < pages; i++ {
		obj(i+3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 400] /Resources << >> >>")
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offset))
	for _, o := range offset[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offset), xref)
	return []byte(b.String())
}

// exitCommand 返回一个以指定退出码结束的命令，用于验证退出码语义。
func exitCommand(code int) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd.exe", "/c", "exit "+strconv.Itoa(code))
	}
	return exec.Command("sh", "-c", "exit "+strconv.Itoa(code))
}

func TestQPDFSucceeded(t *testing.T) {
	assert.True(t, qpdfSucceeded(nil))

	// qpdf 的退出码 3 = "带警告修复成功"，产物可用。
	err := exitCommand(3).Run()
	require.Error(t, err)
	assert.True(t, qpdfSucceeded(err), "exit 3 must count as success")

	err = exitCommand(2).Run()
	require.Error(t, err)
	assert.False(t, qpdfSucceeded(err), "exit 2 is a real failure")

	assert.False(t, qpdfSucceeded(errors.New("boom")), "non-exit errors are failures")
	assert.False(t, qpdfSucceeded(exec.ErrNotFound))
}

// installFakeQPDF 放一个假的 qpdf 到 PATH 前面：--show-npages 报 pages 页，
// 分片时把输入原样复制成输出，两次调用都以 exitCode 退出。
func installFakeQPDF(t *testing.T, pages, exitCode int) {
	t.Helper()
	dir := t.TempDir()
	sep := ":"
	script := filepath.Join(dir, "qpdf")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--show-npages\" ]; then echo " + strconv.Itoa(pages) +
		"; exit " + strconv.Itoa(exitCode) + "; fi\n" +
		"cp \"$2\" \"$7\"\nexit " + strconv.Itoa(exitCode) + "\n"
	if runtime.GOOS == "windows" {
		sep = ";"
		script = filepath.Join(dir, "qpdf.bat")
		body = "@echo off\r\n" +
			"if \"%1\"==\"--show-npages\" (\r\n" +
			"echo " + strconv.Itoa(pages) + "\r\n" +
			"exit /b " + strconv.Itoa(exitCode) + "\r\n" +
			")\r\n" +
			"copy /b \"%2\" \"%7\" >nul\r\n" +
			"exit /b " + strconv.Itoa(exitCode) + "\r\n"
	}
	require.NoError(t, os.WriteFile(script, []byte(body), 0o755))
	t.Setenv("PATH", dir+sep+os.Getenv("PATH"))
}

// 退出码 3 时切片必须照常成功；同时验证按 100 页一片的切分与临时目录清理。
func TestSplitCloudPDFAcceptsQPDFExit3AndCleansTempFiles(t *testing.T) {
	installFakeQPDF(t, 205, 3)

	before, err := filepath.Glob(filepath.Join(os.TempDir(), "mineru-pdf-*"))
	require.NoError(t, err)

	parts, err := splitCloudPDF(context.Background(), testPDF(3))
	require.NoError(t, err)
	require.Len(t, parts, 3)
	assert.Equal(t, []int{1, 100}, []int{parts[0].first, parts[0].last})
	assert.Equal(t, []int{101, 200}, []int{parts[1].first, parts[1].last})
	assert.Equal(t, []int{201, 205}, []int{parts[2].first, parts[2].last})

	// FR-14 ④：中间产物不留孤儿文件。
	after, err := filepath.Glob(filepath.Join(os.TempDir(), "mineru-pdf-*"))
	require.NoError(t, err)
	assert.Equal(t, before, after, "split must not leave temp directories behind")
}

// 分片页数没超过配额且体积没超上限时不切片，直接整体解析。
func TestSplitCloudPDFKeepsSmallPDFIntact(t *testing.T) {
	installFakeQPDF(t, 3, 0)
	content := testPDF(3)

	parts, err := splitCloudPDF(context.Background(), content)
	require.NoError(t, err)
	require.Len(t, parts, 1)
	assert.Equal(t, 1, parts[0].first)
	assert.Equal(t, 3, parts[0].last)
	assert.Equal(t, content, parts[0].content)
}

// qpdf 真正失败（退出码 2）时必须报错，而不是把空产物当成一个分片。
func TestSplitCloudPDFReportsRealQPDFFailure(t *testing.T) {
	installFakeQPDF(t, 3, 2)
	_, err := splitCloudPDF(context.Background(), testPDF(3))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PDF page inspection")
}

func TestSplitCloudPDFPageCoverage(t *testing.T) {
	if _, e := exec.LookPath("qpdf"); e != nil {
		t.Skip("qpdf required")
	}
	parts, e := splitCloudPDF(context.Background(), testPDF(205))
	require.NoError(t, e)
	again, err := splitCloudPDF(context.Background(), testPDF(205))
	require.NoError(t, err)
	for i := range parts {
		assert.Equal(t, parts[i].content, again[i].content, "splitting must be deterministic")
	}
	require.Len(t, parts, 3)
	assert.Equal(t, []int{1, 100}, []int{parts[0].first, parts[0].last})
	assert.Equal(t, []int{101, 200}, []int{parts[1].first, parts[1].last})
	assert.Equal(t, []int{201, 205}, []int{parts[2].first, parts[2].last})
}

// 合并分片时：图片引用命名空间化避免同名冲突，SourceBlock 的页码平移回
// 原始 PDF 页号，rune 偏移按合并后的位置重算。用假 qpdf 驱动，因此在没有
// 安装 qpdf 的机器上同样能跑（真 qpdf 的字节级确定性由
// TestSplitCloudPDFPageCoverage 覆盖）。
func TestSplitCloudPDFMergesPartsWithoutImageCollision(t *testing.T) {
	installFakeQPDF(t, 105, 0)
	allowLoopbackSSRF(t)
	req := &types.ReadRequest{FileName: "book.pdf", FileType: "pdf", FileContent: testPDF(105)}
	c, uploads := mockSplitCloud(t)
	result, err := c.readSplitPDF(context.Background(), req)
	require.NoError(t, err)
	assert.EqualValues(t, 2, uploads.Load())
	assert.Equal(t, "105", result.Metadata["pages"])
	assert.Len(t, result.ImageRefs, 2)

	md := result.MarkdownContent
	require.Len(t, result.SourceBlocks, 2)
	assert.Equal(t, 101, result.SourceBlocks[1].Locator.Page, "page numbers must shift back to the original PDF")
	for i, block := range result.SourceBlocks {
		text := string([]rune(md)[block.Start:block.End])
		assert.True(t, strings.HasPrefix(text, "pages"), "block range must cover its text, got %q", text)
		assert.True(t, strings.HasPrefix(block.Locator.SourceID, fmt.Sprintf("part-%d/", i+1)))
	}
	assert.Contains(t, md, `src="part-2/images/0.jpg"`)
	assert.Contains(t, md, "](part-1/images/0.jpg)")
	assert.Contains(t, md, "](part-2/images/0.jpg)")
	assert.Less(t, strings.Index(md, "pages 1-100"), strings.Index(md, "pages 101-105"),
		"parts must be merged in order")
}

// 没有 qpdf 的部署（本地/单机）仍然要能解析小 PDF：安全回落而不是直接失败。
func TestSmallCloudPDFWithoutQPDFPreservesExistingParsing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	allowLoopbackSSRF(t)
	req := &types.ReadRequest{FileName: "small.pdf", FileType: "pdf", FileContent: testPDF(2)}
	c, uploads := mockSplitCloud(t)

	result, err := c.readSplitPDF(context.Background(), req)
	require.NoError(t, err)
	assert.EqualValues(t, 1, uploads.Load(), "small PDF must be parsed in one go")
	assert.Contains(t, result.MarkdownContent, "pages 1-100")
}

// mockSplitCloud 是一个能跑通完整云端流程（申请上传地址 → PUT → 轮询 →
// 下载 zip）的假服务，无需持久化夹具。
func mockSplitCloud(t *testing.T) (*MinerUCloudReader, *atomic.Int32) {
	t.Helper()
	var uploads atomic.Int32
	var batches atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			id := batches.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "data": map[string]any{
					"batch_id": fmt.Sprint(id), "file_urls": []string{server.URL + "/upload"},
				},
			})
		case r.Method == http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil || !bytes.HasPrefix(data, []byte("%PDF")) {
				t.Error("missing PDF upload")
			}
			uploads.Add(1)
		case strings.HasPrefix(r.URL.Path, "/extract-results/batch/"):
			id := strings.TrimPrefix(r.URL.Path, "/extract-results/batch/")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "data": map[string]any{
					"extract_result": []map[string]string{{"state": "done", "full_zip_url": server.URL + "/zip/" + id}},
				},
			})
		case strings.HasPrefix(r.URL.Path, "/zip/"):
			first, last := 1, 100
			if strings.HasSuffix(r.URL.Path, "/2") {
				first, last = 101, 105
			}
			text := fmt.Sprintf("pages %d-%d 庄子齐物论研究", first, last)
			md := "![image](images/0.jpg) <img src=\"images/0.jpg\">\n\n" + text
			list, _ := json.Marshal([]map[string]any{{
				"type": "text", "text": text, "page_idx": 0, "bbox": []int{0, 0, 100, 100},
			}})
			z := zip.NewWriter(w)
			for name, data := range map[string][]byte{
				"full.md": []byte(md), "images/0.jpg": []byte("image"), "book_content_list.json": list,
			} {
				f, err := z.Create(name)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = f.Write(data)
			}
			if err := z.Close(); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return &MinerUCloudReader{baseURL: server.URL, timeout: time.Second, pollInterval: time.Millisecond}, &uploads
}
