package attachment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // 解码 GIF 源图
	"image/jpeg"
	_ "image/png" // 解码 PNG 源图
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"golang.org/x/sync/singleflight"
)

const (
	// 计时从拿到 VLM 槽位才开始，因此排在其他读取之后不会吃掉模型自己的时间。
	// 完整转写档跑得更久。
	imageDescribeTimeout     = 60 * time.Second
	imageDescribeFullTimeout = 150 * time.Second
	imageDescribeSlots       = 4

	// imageMaxBytes 是交给模型前的单张图片上限；超出就重编码并按比例缩小。
	imageMaxBytes = 4 << 20
	// imageJPEGQuality 是必须重编码时使用的质量。模型看的是内容不是压缩瑕疵。
	imageJPEGQuality = 85
	// imageScaleStep 是重编码后仍超限时每边的缩小比例。
	imageScaleStep = 0.75
	// imageMinSide 是缩小的下限：再小就看不出东西了。
	imageMinSide = 64
)

// Description 记录一次观察的范围；它绝不会被并入解析文本，也不代表图片被完整读过。
type Description struct {
	Text      string    `json:"text"`
	Detail    string    `json:"detail"`
	Focus     string    `json:"focus,omitempty"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
}

// DescriptionCache 把带范围的观察结果与它所属的文档存在一起。
type DescriptionCache interface {
	Get(ctx context.Context, documentID, key string) (*Description, error)
	Put(ctx context.Context, documentID, key string, value Description) error
}

// ImageReader 描述被请求的图片，并复用等价的历史观察结果。
type ImageReader struct {
	Model     string
	Describe  func(context.Context, []byte, string) (string, error)
	Cache     DescriptionCache
	calls     singleflight.Group
	slotsOnce sync.Once
	slots     chan struct{}
}

// preparedImage 是可以直接交给模型的图片字节与其 MIME。
type preparedImage struct {
	Data     []byte
	MIMEType string
}

func (p preparedImage) DataURI() string {
	return "data:" + p.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
}

// ReadImages 对文档鉴权一次，然后并发地准备并描述被请求的图片。
func (r *Reader) ReadImages(
	ctx context.Context, documentID string, numbers []int, detail, focus string,
) ([]interfaces.AttachmentImageRead, error) {
	resolved, err := r.Resolve(ctx, documentID, false)
	if err != nil {
		return nil, err
	}
	doc := resolved.Document
	if doc == nil || doc.Status != types.TemporaryDocumentStatusReady {
		return nil, fmt.Errorf("attachment is not ready")
	}
	if r.readImage == nil {
		return nil, fmt.Errorf("attachment image storage is unavailable")
	}
	images := doc.ViewableImages()
	reads := make([]interfaces.AttachmentImageRead, len(numbers))
	var wg sync.WaitGroup
	for i, number := range numbers {
		reads[i].Number = number
		if number < 1 || number > len(images) {
			reads[i].Err = fmt.Errorf("image number out of range")
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			reads[i].Image, reads[i].Err = r.readImageAt(ctx, doc, images[number-1].URL, detail, focus)
		}()
	}
	wg.Wait()
	return reads, nil
}

func (r *Reader) readImageAt(
	ctx context.Context, doc *types.TemporaryDocument, url, detail, focus string,
) (*interfaces.AttachmentImage, error) {
	data, err := r.readImage(ctx, url)
	if err != nil {
		return nil, err
	}
	prepared, err := prepareImage(data)
	if err != nil {
		return nil, err
	}
	result := &interfaces.AttachmentImage{DataURI: prepared.DataURI()}
	if r.Images != nil && r.Images.Describe != nil {
		text := ""
		result.Description = &text
		description, _, err := r.Images.describe(ctx, doc.ID, prepared.Data, detail, focus)
		if description != nil {
			*result.Description = description.Text
		}
		if err != nil {
			result.Warning = "Image description: " + err.Error()
		}
	}
	return result, nil
}

// describe 生成（或复用）一次图片理解结果。
//
// 缓存键由三要素决定：模型 ID + 归一化后的提示词 + 图片字节的 sha256。
// 换模型、换 detail/focus、换图片内容都不会命中同一份缓存（PRD FR-22 ⑤）。
func (r *ImageReader) describe(
	ctx context.Context, documentID string, data []byte, detail, focus string,
) (*Description, string, error) {
	detail, focus = types.NormalizeImageRead(detail, focus)
	prompt := types.ImageReadPrompt(detail, focus)
	identity, _ := json.Marshal([]string{r.Model, prompt, fmt.Sprintf("%x", sha256.Sum256(data))})
	key := fmt.Sprintf("%x", sha256.Sum256(identity))
	timeout := imageDescribeTimeout
	if detail == types.ImageDetailFull {
		timeout = imageDescribeFullTimeout
	}
	// 合并同一次执行内的重复读取；持久化缓存还能服务后续轮次与分叉会话
	// （在它们各自完成鉴权之后）。共享调用与发起它的调用者脱钩，
	// 因此一个调用者放弃不会连累其他等待同一张图的人。
	shared := context.WithoutCancel(ctx)
	result := r.calls.DoChan(documentID+":"+key, func() (interface{}, error) {
		if r.Cache != nil {
			cached, err := r.Cache.Get(shared, documentID, key)
			if err == nil && cached != nil {
				return cached, nil
			}
		}
		r.slotsOnce.Do(func() { r.slots = make(chan struct{}, imageDescribeSlots) })
		r.slots <- struct{}{}
		defer func() { <-r.slots }()
		imageCtx, cancel := context.WithTimeout(shared, timeout)
		defer cancel()
		text, err := r.Describe(imageCtx, data, prompt)
		if err != nil {
			return nil, err
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return nil, fmt.Errorf("vision model returned no description")
		}
		value := &Description{
			Text: text, Detail: detail, Focus: focus, Model: r.Model, CreatedAt: time.Now().UTC(),
		}
		if r.Cache != nil {
			if err := r.Cache.Put(shared, documentID, key, *value); err != nil {
				return value, fmt.Errorf("save description: %w", err)
			}
		}
		return value, nil
	})
	select {
	case <-ctx.Done():
		return nil, key, ctx.Err()
	case value := <-result:
		description, _ := value.Val.(*Description)
		return description, key, value.Err
	}
}

// prepareImage 把存储里的图片字节整理成模型能收的形态：格式已在白名单内且体积
// 达标就原样使用，否则重编码为 JPEG 并按比例缩小直到放得下。
func prepareImage(data []byte) (preparedImage, error) {
	if len(data) == 0 {
		return preparedImage{}, fmt.Errorf("image is empty")
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return preparedImage{}, fmt.Errorf("not an image (%s)", mime)
	}
	if len(data) <= imageMaxBytes && modelAcceptsMIME(mime) {
		return preparedImage{Data: data, MIMEType: mime}, nil
	}
	src, srcMIME, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return preparedImage{}, fmt.Errorf("decode %s: %w", srcMIME, err)
	}
	img := src
	for {
		out, err := encodeJPEG(img)
		if err != nil {
			return preparedImage{}, err
		}
		if len(out) <= imageMaxBytes {
			return preparedImage{Data: out, MIMEType: "image/jpeg"}, nil
		}
		bounds := img.Bounds()
		w := int(float64(bounds.Dx()) * imageScaleStep)
		h := int(float64(bounds.Dy()) * imageScaleStep)
		if w < imageMinSide || h < imageMinSide {
			return preparedImage{}, fmt.Errorf("image does not fit %d bytes even scaled down", imageMaxBytes)
		}
		img = scaleImage(img, w, h)
	}
}

// modelAcceptsMIME 是无需转码就能被主流对话/视觉模型接受的格式白名单。
func modelAcceptsMIME(mime string) bool {
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

func encodeJPEG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, opaque(img), &jpeg.Options{Quality: imageJPEGQuality}); err != nil {
		return nil, fmt.Errorf("re-encode image: %w", err)
	}
	return buf.Bytes(), nil
}

// opaque 去掉 alpha 通道：JPEG 不支持透明，直接编码会把透明区域变成黑块。
func opaque(img image.Image) image.Image {
	if _, ok := img.(*image.YCbCr); ok {
		return img
	}
	bounds := img.Bounds()
	out := image.NewRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			if a == 0 {
				out.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
				continue
			}
			out.SetRGBA(x, y, color.RGBA{
				R: uint8(r * 255 / a), G: uint8(g * 255 / a), B: uint8(b * 255 / a), A: 255,
			})
		}
	}
	return out
}

// scaleImage 用最近邻采样缩小图片。附件图片的用途是给模型看结构，
// 这里优先保证耗时可控而不是插值质量。
func scaleImage(src image.Image, width, height int) image.Image {
	bounds := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	srcW, srcH := bounds.Dx(), bounds.Dy()
	for y := 0; y < height; y++ {
		sy := bounds.Min.Y + y*srcH/height
		for x := 0; x < width; x++ {
			sx := bounds.Min.X + x*srcW/width
			out.Set(x, y, src.At(sx, sy))
		}
	}
	return out
}
