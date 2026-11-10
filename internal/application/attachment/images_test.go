package attachment

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pngBytes 生成一张纯色 PNG，供需要真实图片字节的用例使用。
func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 30), B: 40, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// memoryCache 是 DescriptionCache 的内存替身，同时记录命中/写入次数。
type memoryCache struct {
	mu   sync.Mutex
	data map[string]map[string]Description
	hits int
	puts int
}

func newMemoryCache() *memoryCache {
	return &memoryCache{data: map[string]map[string]Description{}}
}

func (c *memoryCache) Get(_ context.Context, documentID, key string) (*Description, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if value, ok := c.data[documentID][key]; ok {
		c.hits++
		return &value, nil
	}
	return nil, nil
}

func (c *memoryCache) Put(_ context.Context, documentID, key string, value Description) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts++
	if c.data[documentID] == nil {
		c.data[documentID] = map[string]Description{}
	}
	c.data[documentID][key] = value
	return nil
}

// countingDescribe 记录 VLM 调用次数，并可按 prompt 区分档位。
type countingDescribe struct {
	mu       sync.Mutex
	calls    int
	prompts  []string
	blockFor time.Duration
}

func (d *countingDescribe) fn() func(context.Context, []byte, string) (string, error) {
	return func(ctx context.Context, _ []byte, prompt string) (string, error) {
		d.mu.Lock()
		d.calls++
		d.prompts = append(d.prompts, prompt)
		d.mu.Unlock()
		if d.blockFor > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(d.blockFor):
			}
		}
		return "一张测试图片", nil
	}
}

func (d *countingDescribe) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// FR-22 ⑤：缓存键 = 模型 ID + 归一化后的提示词 + 图片字节 sha256。
// 三要素任一变化都不应命中同一份缓存。
func TestDescribeCacheKeyHasThreeFactors(t *testing.T) {
	img := pngBytes(t)
	other := pngBytes(t)
	other[20] ^= 0xFF // 改一个字节，内容不同但仍是合法 PNG
	cache := newMemoryCache()
	vlm := &countingDescribe{}

	newReader := func(model string) *ImageReader {
		return &ImageReader{Model: model, Describe: vlm.fn(), Cache: cache}
	}

	ctx := context.Background()
	_, keyA, err := newReader("vlm-a").describe(ctx, "doc-1", img, "brief", "")
	require.NoError(t, err)
	assert.Equal(t, 1, vlm.count(), "首次读取必须真的调用 VLM")

	// 同模型 / 同请求 / 同图片 → 命中缓存，不再调用。
	_, keyB, err := newReader("vlm-a").describe(ctx, "doc-1", img, "brief", "")
	require.NoError(t, err)
	assert.Equal(t, keyA, keyB)
	assert.Equal(t, 1, vlm.count(), "等价请求必须复用缓存")
	assert.Equal(t, 1, cache.hits)

	// 换模型 → 不命中。
	_, keyModel, err := newReader("vlm-b").describe(ctx, "doc-1", img, "brief", "")
	require.NoError(t, err)
	assert.NotEqual(t, keyA, keyModel)
	assert.Equal(t, 2, vlm.count())

	// 换 detail（brief → full）→ 提示词不同 → 不命中。
	_, keyDetail, err := newReader("vlm-a").describe(ctx, "doc-1", img, "full", "")
	require.NoError(t, err)
	assert.NotEqual(t, keyA, keyDetail)
	assert.Equal(t, 3, vlm.count())

	// 换 focus → 提示词不同 → 不命中。
	_, keyFocus, err := newReader("vlm-a").describe(ctx, "doc-1", img, "brief", "看左上角的数字")
	require.NoError(t, err)
	assert.NotEqual(t, keyA, keyFocus)
	assert.Equal(t, 4, vlm.count())

	// 换图片字节 → 不命中。
	_, keyImage, err := newReader("vlm-a").describe(ctx, "doc-1", other, "brief", "")
	require.NoError(t, err)
	assert.NotEqual(t, keyA, keyImage)
	assert.Equal(t, 5, vlm.count())

	// 换文档 ID 但三要素相同 → 键相同（缓存按文档分区，因此不会跨文档串用）。
	_, keyOtherDoc, err := newReader("vlm-a").describe(ctx, "doc-2", img, "brief", "")
	require.NoError(t, err)
	assert.Equal(t, keyA, keyOtherDoc)
	assert.Equal(t, 6, vlm.count(), "不同文档各自持有一份缓存条目")
	assert.Contains(t, cache.data, "doc-1")
	assert.Contains(t, cache.data, "doc-2")
}

// focus 里的空白与超长尾巴在归一化后应当产生同一个键。
func TestDescribeCacheKeyNormalisesFocus(t *testing.T) {
	img := pngBytes(t)
	cache := newMemoryCache()
	vlm := &countingDescribe{}
	reader := &ImageReader{Model: "vlm-a", Describe: vlm.fn(), Cache: cache}

	ctx := context.Background()
	_, key1, err := reader.describe(ctx, "doc-1", img, "brief", "  柱状图  ")
	require.NoError(t, err)
	_, key2, err := reader.describe(ctx, "doc-1", img, "", "柱状图")
	require.NoError(t, err)
	assert.Equal(t, key1, key2)
	assert.Equal(t, 1, vlm.count())
}

// 并发读同一张图只应触发一次 VLM 调用（singleflight 合并）。
func TestDescribeCoalescesConcurrentReads(t *testing.T) {
	img := pngBytes(t)
	vlm := &countingDescribe{blockFor: 60 * time.Millisecond}
	reader := &ImageReader{Model: "vlm-a", Describe: vlm.fn(), Cache: newMemoryCache()}

	const readers = 8
	var wg sync.WaitGroup
	results := make([]*Description, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			desc, _, err := reader.describe(context.Background(), "doc-1", img, "brief", "")
			require.NoError(t, err)
			results[i] = desc
		}(i)
	}
	wg.Wait()

	assert.Equal(t, 1, vlm.count(), "同一张图的并发读取只允许一次 VLM 调用")
	for _, desc := range results {
		require.NotNil(t, desc)
		assert.Equal(t, "一张测试图片", desc.Text)
	}
}

// 一个调用者放弃不能拖垮其他等待同一张图的调用者：
// 共享调用用 context.WithoutCancel 与发起者脱钩。
func TestDescribeSurvivesCallerCancellation(t *testing.T) {
	img := pngBytes(t)
	vlm := &countingDescribe{blockFor: 120 * time.Millisecond}
	cache := newMemoryCache()
	reader := &ImageReader{Model: "vlm-a", Describe: vlm.fn(), Cache: cache}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, key, err := reader.describe(ctx, "doc-1", img, "brief", "")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotEmpty(t, key)

	// 放弃的那个调用者仍然让共享调用跑完并写进了缓存。
	require.Eventually(t, func() bool {
		cached, cacheErr := cache.Get(context.Background(), "doc-1", key)
		return cacheErr == nil && cached != nil
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, 1, vlm.count())
}

// VLM 返回空串不算成功，不写缓存。
func TestDescribeRejectsEmptyModelOutput(t *testing.T) {
	img := pngBytes(t)
	cache := newMemoryCache()
	reader := &ImageReader{Model: "vlm-a", Describe: func(context.Context, []byte, string) (string, error) {
		return "   ", nil
	}, Cache: cache}

	desc, key, err := reader.describe(context.Background(), "doc-1", img, "brief", "")
	require.Error(t, err)
	assert.Nil(t, desc)
	assert.NotEmpty(t, key)
	assert.Equal(t, 0, cache.puts)
}

// detail=full 用更长的超时档位。
func TestDescribeFullDetailUsesLongerTimeout(t *testing.T) {
	assert.Greater(t, imageDescribeFullTimeout, imageDescribeTimeout)
	assert.Positive(t, imageDescribeSlots)
}

func TestPrepareImage(t *testing.T) {
	png := pngBytes(t)

	t.Run("passes through an acceptable image", func(t *testing.T) {
		prepared, err := prepareImage(png)
		require.NoError(t, err)
		assert.Equal(t, "image/png", prepared.MIMEType)
		assert.Equal(t, png, prepared.Data)
		assert.Contains(t, prepared.DataURI(), "data:image/png;base64,")
	})

	t.Run("rejects empty bytes", func(t *testing.T) {
		_, err := prepareImage(nil)
		require.Error(t, err)
	})

	t.Run("rejects non-image bytes", func(t *testing.T) {
		_, err := prepareImage([]byte("not an image at all"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not an image")
	})

	t.Run("re-encodes an unsupported format", func(t *testing.T) {
		bmp := []byte("BM")
		bmp = append(bmp, make([]byte, 64)...)
		_, err := prepareImage(bmp)
		// 没有 BMP 解码器，必须报可读的解码错误而不是把垃圾字节发给模型。
		require.Error(t, err)
	})
}
