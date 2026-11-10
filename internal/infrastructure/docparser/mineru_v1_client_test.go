package docparser

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

// allowLoopbackSSRF 放开 127.0.0.1，让 httptest 服务能通过 SSRF 校验。
// 与 zip_limits_test.go 采用同一手法（改 env + 重置单例），测试结束自动还原。
func allowLoopbackSSRF(t *testing.T) {
	t.Helper()
	t.Setenv("SSRF_WHITELIST", "127.0.0.1,::1,localhost")
	utils.ResetSSRFWhitelistForTest()
	t.Cleanup(utils.ResetSSRFWhitelistForTest)
}

// fakeMinerUV1 是内存里的最小 MinerU 4.0 api-server。
type fakeMinerUV1 struct {
	t      *testing.T
	apiKey string

	mu           sync.Mutex
	uploadURL    string // 绝对或相对；默认同源相对路径
	dedupHit     bool   // POST /v1/uploads 直接返回 completed（sha256 秒传）
	uploaded     []byte
	putAuth      string
	uploadMime   string
	jobPayload   map[string]any
	pollStatuses []string // 连续 GET job 返回的状态
	polls        int
	fileError    *map[string]string
	canceled     bool
	deletedFiles []string
	zipData      []byte
	downloadCnt  int
}

func (f *fakeMinerUV1) handler() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if f.apiKey != "" && r.Header.Get("Authorization") != "Bearer "+f.apiKey {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error": map[string]string{"message": "bad key", "type": "auth_error", "code": "invalid_api_key"},
				})
				return
			}
			next(w, r)
		}
	}

	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": "1.0.0"})
	})
	mux.HandleFunc("POST /v1/uploads", authed(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(f.t, json.NewDecoder(r.Body).Decode(&body))
		f.mu.Lock()
		defer f.mu.Unlock()
		f.uploadMime, _ = body["mime_type"].(string)
		if f.dedupHit {
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "upload_1", "status": "completed", "file": map[string]any{"id": "file-src"},
			})
			return
		}
		uploadURL := f.uploadURL
		if uploadURL == "" {
			uploadURL = "/v1/uploads/upload_1/content"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "upload_1", "status": "pending", "upload_url": uploadURL, "upload_method": "PUT",
			"upload_headers": map[string]string{"Content-Type": f.uploadMime},
		})
	}))
	mux.HandleFunc("PUT /v1/uploads/upload_1/content", f.handlePut)
	mux.HandleFunc("POST /v1/uploads/upload_1/complete", authed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "upload_1", "status": "completed", "file": map[string]any{"id": "file-src"},
		})
	}))
	mux.HandleFunc("POST /v1/parse/jobs", authed(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(f.t, json.NewDecoder(r.Body).Decode(&body))
		f.mu.Lock()
		f.jobPayload = body
		f.mu.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]any{"job_id": "job_1", "status": "queued"})
	}))
	mux.HandleFunc("GET /v1/parse/jobs/job_1", authed(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		status := "completed"
		if f.polls < len(f.pollStatuses) {
			status = f.pollStatuses[f.polls]
		}
		f.polls++
		file := map[string]any{"name": "report.pdf", "status": status}
		switch status {
		case "completed":
			file["output_files"] = map[string]any{"zip": map[string]any{"file_id": "file-zip", "bytes": len(f.zipData)}}
		case "failed":
			file["error"] = *f.fileError
		}
		writeJSON(w, http.StatusOK, map[string]any{"job_id": "job_1", "status": status, "files": []any{file}})
	}))
	mux.HandleFunc("DELETE /v1/parse/jobs/job_1", authed(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.canceled = true
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"job_id": "job_1", "status": "canceled"})
	}))
	// FR-14 ④：失败/取消路径要删掉远端已上传的文件，不留孤儿产物。
	mux.HandleFunc("DELETE /v1/files/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deletedFiles = append(f.deletedFiles, r.PathValue("id"))
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "status": "deleted"})
	}))
	mux.HandleFunc("GET /v1/files/file-zip/content", authed(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.downloadCnt++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(f.zipData)
	}))
	return mux
}

func (f *fakeMinerUV1) handlePut(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	require.NoError(f.t, err)
	f.mu.Lock()
	f.uploaded = data
	f.putAuth = r.Header.Get("Authorization")
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func fastV1Client(baseURL, apiKey string) *minerUV1Client {
	c := newMinerUV1Client(baseURL, apiKey, 10*time.Second)
	c.pollInitial = time.Millisecond
	c.pollMax = 5 * time.Millisecond
	return c
}

const v1TestMarkdown = "# Report\n\n![](images/page_0_image_1.png)\n\n" +
	"<table><tr><td><img src=\"images/page_0_table_image_2_1.png\"></td></tr></table>\n"

func v1TestZip(t *testing.T) []byte {
	return buildMinerUZip(t, map[string]string{
		"markdown.md":                       v1TestMarkdown,
		"doc_content_list.json":             `[{"type":"text","text":"Report","page_idx":0,"bbox":[0,0,1000,100]}]`,
		"images/page_0_image_1.png":         "png-bytes-1",
		"images/page_0_table_image_2_1.png": "png-bytes-2",
		"images/page_1_unreferenced.png":    "unused",
	})
}

func TestMinerUV1ClientParseFullFlow(t *testing.T) {
	allowLoopbackSSRF(t)
	fake := &fakeMinerUV1{
		t:            t,
		apiKey:       "server-key",
		pollStatuses: []string{"queued", "running", "completed"},
		zipData:      v1TestZip(t),
	}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	client := fastV1Client(server.URL, "server-key")
	md, refs, contentList, err := client.ParseWithLayout(context.Background(), []byte("%PDF-1.7 body"),
		"report.pdf", minerUV1ParseOptions{Tier: "advanced", OCRMode: "txt"})
	require.NoError(t, err)

	assert.Equal(t, v1TestMarkdown, md)
	require.Len(t, refs, 2)
	byRef := map[string]types.ImageRef{}
	for _, ref := range refs {
		byRef[ref.OriginalRef] = ref
	}
	assert.Equal(t, []byte("png-bytes-1"), byRef["images/page_0_image_1.png"].ImageData)
	assert.Equal(t, []byte("png-bytes-2"), byRef["images/page_0_table_image_2_1.png"].ImageData)
	assert.Equal(t, "image/png", byRef["images/page_0_image_1.png"].MimeType)
	// 布局产物（FR-14 ①）要随结果一起交出来。
	assert.JSONEq(t, `[{"type":"text","text":"Report","page_idx":0,"bbox":[0,0,1000,100]}]`, string(contentList))

	assert.Equal(t, []byte("%PDF-1.7 body"), fake.uploaded)
	assert.Equal(t, "Bearer server-key", fake.putAuth, "same-origin upload must carry the API key")
	assert.Equal(t, "application/pdf", fake.uploadMime)
	assert.Equal(t, 3, fake.polls)
	assert.False(t, fake.canceled)
	assert.Empty(t, fake.deletedFiles, "a successful parse must not delete its artifacts")

	assert.Equal(t, "advanced", fake.jobPayload["tier"])
	assert.Equal(t, "txt", fake.jobPayload["ocr_mode"])
	assert.Equal(t, []any{"zip"}, fake.jobPayload["output_formats"])
	files := fake.jobPayload["files"].([]any)
	source := files[0].(map[string]any)["source"].(map[string]any)
	assert.Equal(t, map[string]any{"type": "file_id", "file_id": "file-src"}, source)
}

func TestMinerUV1ClientOmitsTierForServerDefault(t *testing.T) {
	allowLoopbackSSRF(t)
	fake := &fakeMinerUV1{t: t, zipData: v1TestZip(t)}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	_, _, _, err := fastV1Client(server.URL, "").ParseWithLayout(context.Background(), []byte("x"), "a.pdf",
		minerUV1ParseOptions{OCRMode: "auto"})
	require.NoError(t, err)
	_, hasTier := fake.jobPayload["tier"]
	assert.False(t, hasTier)
}

// 预签名对象存储 URL（官方 API 下发的绝对地址）与 base 不同源，绝不能收到
// 我们的 API Key；同源地址则必须带上，否则网关会拒绝上传。
func TestMinerUV1ClientCrossOriginUploadOmitsAPIKey(t *testing.T) {
	allowLoopbackSSRF(t)
	fake := &fakeMinerUV1{t: t, apiKey: "server-key", zipData: v1TestZip(t)}

	// 同一主机不同端口即为不同源，等价于云端下发的预签名存储地址。
	storage := httptest.NewServer(http.HandlerFunc(fake.handlePut))
	defer storage.Close()
	fake.uploadURL = storage.URL + "/v1/uploads/upload_1/content?Signature=abc"

	server := httptest.NewServer(fake.handler())
	defer server.Close()

	_, _, _, err := fastV1Client(server.URL, "server-key").ParseWithLayout(context.Background(),
		[]byte("bytes"), "a.pdf", minerUV1ParseOptions{})
	require.NoError(t, err)
	assert.Equal(t, []byte("bytes"), fake.uploaded)
	assert.Empty(t, fake.putAuth, "API key must not leak to a cross-origin upload URL")
}

func TestMinerUV1ClientResolveURLRejectsOtherSchemes(t *testing.T) {
	client := newMinerUV1Client("https://mineru.example.com", "", time.Second)
	for _, raw := range []string{"file:///etc/passwd", "ftp://mineru.example.com/x", "gopher://127.0.0.1:25/"} {
		_, err := client.resolveURL(raw)
		require.Error(t, err, raw)
		assert.Contains(t, err.Error(), "unsupported scheme")
	}
	resolved, err := client.resolveURL("/v1/uploads/abc/content")
	require.NoError(t, err)
	assert.Equal(t, "https://mineru.example.com/v1/uploads/abc/content", resolved.String())
}

func TestMinerUV1SameOrigin(t *testing.T) {
	client := newMinerUV1Client("https://mineru.example.com", "", time.Second)
	mustParse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		return u
	}
	assert.True(t, client.sameOrigin(mustParse("https://mineru.example.com/x")))
	assert.True(t, client.sameOrigin(mustParse("https://mineru.example.com:443/x")), "default port is implicit")
	assert.False(t, client.sameOrigin(mustParse("http://mineru.example.com/x")), "scheme differs")
	assert.False(t, client.sameOrigin(mustParse("https://mineru.example.com:8443/x")), "port differs")
	assert.False(t, client.sameOrigin(mustParse("https://storage.example.net/x")), "host differs")
}

func TestMinerUV1ClientDedupSkipsByteUpload(t *testing.T) {
	allowLoopbackSSRF(t)
	fake := &fakeMinerUV1{t: t, dedupHit: true, zipData: v1TestZip(t)}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	_, _, _, err := fastV1Client(server.URL, "").ParseWithLayout(context.Background(), []byte("bytes"), "a.pdf",
		minerUV1ParseOptions{})
	require.NoError(t, err)
	assert.Nil(t, fake.uploaded)
}

func TestMinerUV1ClientFailedJobReportsFileErrorAndCleansUp(t *testing.T) {
	allowLoopbackSSRF(t)
	fake := &fakeMinerUV1{
		t:            t,
		pollStatuses: []string{"failed"},
		fileError:    &map[string]string{"code": "parse_failed", "message": "corrupt pdf"},
	}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	_, _, _, err := fastV1Client(server.URL, "").ParseWithLayout(context.Background(), []byte("x"), "a.pdf",
		minerUV1ParseOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse_failed: corrupt pdf")
	assert.Zero(t, fake.downloadCnt)
	// FR-14 ④：解析失败时远端 job 与已上传文件都要清理。
	assert.True(t, fake.canceled, "failed job should be canceled")
	assert.Equal(t, []string{"file-src"}, fake.deletedFiles, "uploaded file should be deleted")
}

func TestMinerUV1ClientCancelsJobOnTimeout(t *testing.T) {
	allowLoopbackSSRF(t)
	statuses := make([]string, 1000)
	for i := range statuses {
		statuses[i] = "running"
	}
	fake := &fakeMinerUV1{t: t, pollStatuses: statuses}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	client := fastV1Client(server.URL, "")
	client.jobTimeout = 100 * time.Millisecond
	_, _, _, err := client.ParseWithLayout(context.Background(), []byte("x"), "a.pdf", minerUV1ParseOptions{})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, fake.canceled, "abandoned job should be canceled on the server")
	assert.Equal(t, []string{"file-src"}, fake.deletedFiles, "abandoned upload should be deleted")
}

// 调用方 ctx 被取消时，清理必须仍然跑得动（它用的是独立的 background ctx）。
func TestMinerUV1ClientCleansUpAfterCallerCancellation(t *testing.T) {
	allowLoopbackSSRF(t)
	statuses := make([]string, 1000)
	for i := range statuses {
		statuses[i] = "running"
	}
	fake := &fakeMinerUV1{t: t, pollStatuses: statuses}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, _, _, err := fastV1Client(server.URL, "").ParseWithLayout(ctx, []byte("x"), "a.pdf", minerUV1ParseOptions{})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.True(t, fake.canceled, "canceled job should be cleaned up on the server")
	assert.Equal(t, []string{"file-src"}, fake.deletedFiles)
}

func TestMinerUV1ClientAPIErrorMessage(t *testing.T) {
	allowLoopbackSSRF(t)
	fake := &fakeMinerUV1{t: t, apiKey: "server-key"}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	_, _, _, err := fastV1Client(server.URL, "wrong").ParseWithLayout(context.Background(), []byte("x"), "a.pdf",
		minerUV1ParseOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.Contains(t, err.Error(), "invalid_api_key: bad key")
}

func TestDetectMinerUProtocol(t *testing.T) {
	allowLoopbackSSRF(t)
	tests := []struct {
		name       string
		apiKey     string
		requireKey string
		status     int
		body       string
		want       minerUProtocol
		wantErr    string
	}{
		{name: "v1 healthy", status: 200, body: `{"status":"ok","version":"1.0.0"}`, want: minerUProtocolV1},
		{
			name:       "v1 healthy behind gateway with api key",
			apiKey:     "test-key",
			requireKey: "test-key",
			status:     200,
			body:       `{"status":"ok"}`,
			want:       minerUProtocolV1,
		},
		{
			name:       "v1 behind gateway missing api key",
			requireKey: "test-key",
			status:     401,
			body: `{"error":{"message":"Authentication Error, No api key passed in.",` +
				`"type":"auth_error","code":"401"}}`,
			want:    minerUProtocolV1,
			wantErr: "requires authentication, please configure API key",
		},
		{
			name:       "v1 behind gateway invalid api key",
			apiKey:     "bad-key",
			requireKey: "test-key",
			status:     401,
			body:       `{"error":{"message":"Invalid API key","type":"auth_error","code":"401"}}`,
			want:       minerUProtocolV1,
			wantErr:    "401: Invalid API key",
		},
		{name: "legacy 404", status: 404, body: `{"detail":"Not Found"}`, want: minerUProtocolLegacy},
		{name: "legacy 405", status: 405, body: ``, want: minerUProtocolLegacy},
		{
			name:    "v1 preloading models",
			status:  503,
			body:    `{"error":{"message":"weights missing","type":"engine_error","code":"model_preload_files_missing"}}`,
			want:    minerUProtocolV1,
			wantErr: "model_preload_files_missing: weights missing",
		},
		{name: "proxy error", status: 502, body: `bad gateway`, wantErr: "status 502"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/health", r.URL.Path)
				if tt.requireKey != "" && r.Header.Get("Authorization") != "Bearer "+tt.requireKey {
					w.WriteHeader(http.StatusUnauthorized)
					if tt.body != "" {
						_, _ = w.Write([]byte(tt.body))
					}
					return
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			got, err := detectMinerUProtocol(context.Background(), http.DefaultClient, server.URL, tt.apiKey)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMinerUReaderUsesV1WhenAvailable(t *testing.T) {
	allowLoopbackSSRF(t)
	fake := &fakeMinerUV1{t: t, apiKey: "server-key", zipData: v1TestZip(t)}
	server := httptest.NewServer(fake.handler())
	defer server.Close()

	reader := NewMinerUReader(map[string]string{
		"mineru_endpoint":       server.URL,
		"mineru_server_api_key": "server-key",
		"mineru_tier":           "Standard",
		"mineru_parse_method":   "ocr",
		// Legacy-only knobs are ignored by the V1 path.
		"mineru_model":          "pipeline",
		"mineru_enable_formula": "false",
	})
	result, err := reader.Read(context.Background(), &types.ReadRequest{
		FileContent: []byte("%PDF"),
		FileName:    "dir/report.pdf",
		FileType:    "pdf",
	})
	require.NoError(t, err)
	require.Empty(t, result.Error)
	assert.True(t, strings.HasPrefix(result.MarkdownContent, "# Report"))
	assert.Len(t, result.ImageRefs, 2)
	assert.NotEmpty(t, result.SourceBlocks, "layout must survive into ReadResult.SourceBlocks")
	assert.Equal(t, "standard", fake.jobPayload["tier"])
	assert.Equal(t, "ocr", fake.jobPayload["ocr_mode"])
}

// legacy 服务没有 /v1 路由，探测 404 后必须照常走 /file_parse，
// 并且请求里要带上 return_middle_json / return_content_list（FR-14 ①）。
func TestMinerUReaderFallsBackToLegacyAndRequestsLayout(t *testing.T) {
	allowLoopbackSSRF(t)
	var formFields map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			http.NotFound(w, r)
			return
		case "/file_parse":
			require.NoError(t, r.ParseMultipartForm(8<<20))
			formFields = map[string]string{}
			for key, values := range r.MultipartForm.Value {
				if len(values) > 0 {
					formFields[key] = values[0]
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"results":{"report":{"md_content":"# Report\n","images":{}}}}`)
			return
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	reader := NewMinerUReader(map[string]string{"mineru_endpoint": server.URL})
	result, err := reader.Read(context.Background(), &types.ReadRequest{
		FileContent: []byte("%PDF"),
		FileName:    "report.pdf",
		FileType:    "pdf",
	})
	require.NoError(t, err)
	require.Empty(t, result.Error)
	assert.Equal(t, "# Report\n", result.MarkdownContent)
	assert.Equal(t, "true", formFields["return_content_list"])
	assert.Equal(t, "true", formFields["return_middle_json"])
}

func TestResolveMinerUTier(t *testing.T) {
	assert.Equal(t, "", resolveMinerUTier(""))
	assert.Equal(t, "flash", resolveMinerUTier(" Flash "))
	assert.Equal(t, "advanced", resolveMinerUTier("advanced"))
	assert.Equal(t, "", resolveMinerUTier("pipeline"), "legacy backend names are not tiers")
	assert.Equal(t, "", resolveMinerUTier("auto"))
}

func TestResolveMinerUParseMethod(t *testing.T) {
	assert.Equal(t, "auto", resolveMinerUParseMethod(" Auto ", true))
	assert.Equal(t, "txt", resolveMinerUParseMethod("", false), "OCR off means text mode")
	assert.Equal(t, "ocr", resolveMinerUParseMethod("", true))
	assert.Equal(t, "ocr", resolveMinerUParseMethod("nonsense", true), "unknown values fall back")
}

func TestExtractMarkdownZipResolvesEncodedAndNestedPaths(t *testing.T) {
	data := buildMinerUZip(t, map[string]string{
		"report/report.md":        "![](images/%E7%AC%AC%201%20%E9%A1%B5.png)\n![](./images/b.jpg)",
		"report/images/第 1 页.png": "p1",
		"report/images/b.jpg":     "p2",
	})
	md, refs, err := extractMarkdownZip(data, "test")
	require.NoError(t, err)
	assert.Contains(t, md, "images/")
	require.Len(t, refs, 2)
	assert.Equal(t, []byte("p1"), refs[0].ImageData)
	assert.Equal(t, "image/jpeg", refs[1].MimeType)
}

func TestMinerUV1APIError_LongNonJSONBodyStaysValidUTF8(t *testing.T) {
	// 500 字节处正好落在一个三字节字符中间。
	body := []byte("<h1>" + strings.Repeat("服务暂时不可用", 30) + "</h1>")
	require.Greater(t, len(body), 500)

	err := newMinerUV1APIError(http.StatusBadGateway, body)
	assert.True(t, utf8.ValidString(err.Error()), "error is not valid UTF-8: %q", err.Error())
	assert.True(t, strings.HasSuffix(err.msg, "..."))
	assert.LessOrEqual(t, len(err.msg), 500+len("..."))
}
