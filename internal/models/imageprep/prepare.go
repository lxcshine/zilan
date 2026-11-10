// Package imageprep turns raw image bytes into model-ready images: it sniffs
// the format, enforces per-model limits (byte size, pixel dimensions), and
// shrinks oversized images so a single huge picture cannot blow up an
// embedding or vision-chat request.
//
// 照抄自上游 WeKnora v0.8 internal/models/imageprep/prepare.go 并适配
// Zilan 的模块布局（上游的 EmbedImage 类型在 api 包，Zilan 未引入该包，
// 故连同 Limits/Image 一并落在本包；缩放仅用标准库实现，WebP 仅做格式
// 嗅探、超限时按“不可处理”跳过——§5.3 单图失败不阻断整批）。
package imageprep

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// Limits are the constraints a target model places on one image.
type Limits struct {
	// MaxBytes caps the encoded image size accepted by the model. 0 means no
	// explicit byte limit.
	MaxBytes int
	// MaxDimension caps width and height, each independently. 0 disables the
	// dimension check.
	MaxDimension int
	// MaxPixels caps width*height. 0 disables the pixel check.
	MaxPixels int
	// MIMETypes lists the formats the model accepts; nil/empty accepts the
	// four formats this package understands (PNG, JPEG, WebP, GIF).
	MIMETypes []string
}

// ErrImagesUnsupported reports an image this package cannot prepare (e.g. a
// WebP that needs shrinking, or an undecodable file). Callers treat it as
// "skip this image", never a batch failure.
var ErrImagesUnsupported = errors.New("image cannot be prepared for the model")

var sniffedFormats = []struct {
	magic []byte
	mime  string
}{
	{[]byte{0x89, 'P', 'N', 'G'}, "image/png"},
	{[]byte{0xFF, 0xD8, 0xFF}, "image/jpeg"},
	{[]byte{'R', 'I', 'F', 'F'}, "image/webp"}, // RIFF....WEBP
	{[]byte{'G', 'I', 'F', '8'}, "image/gif"},
}

func sniffMIME(data []byte) string {
	for _, f := range sniffedFormats {
		if len(data) >= len(f.magic) && bytes.Equal(data[:len(f.magic)], f.magic) {
			if f.mime == "image/webp" && (len(data) < 12 || !bytes.Equal(data[8:12], []byte("WEBP"))) {
				continue
			}
			return f.mime
		}
	}
	return ""
}

// Image is one model-ready image: the (possibly re-encoded) bytes plus the
// MIME type the model should be told about.
type Image struct {
	Data     []byte
	MIMEType string
}

// Base64 returns the image bytes in standard base64 encoding.
func (img Image) Base64() string {
	return base64.StdEncoding.EncodeToString(img.Data)
}

// DataURI returns the image as a data: URI, the form chat and embedding APIs
// accept inline.
func (img Image) DataURI() string {
	return "data:" + img.MIMEType + ";base64," + img.Base64()
}

// Prepare validates data against limits and returns a model-ready image.
// Images that already fit are returned untouched (byte-identical); oversized
// dimensions are healed by proportional nearest-neighbour downscaling.
// Images that still exceed MaxBytes, or that fail to decode, return an error
// the caller treats as "skip this image" (§5.3).
func Prepare(data []byte, limits Limits) (Image, error) {
	if len(data) == 0 {
		return Image{}, errors.New("empty image")
	}
	mime := sniffMIME(data)
	if mime == "" {
		return Image{}, errors.New("unrecognized image format")
	}
	if !acceptsMIME(limits, mime) {
		return Image{}, fmt.Errorf("model does not accept %s images", mime)
	}

	// 无任何尺寸约束：原样透传（已通过格式嗅探）。
	needShrink := limits.MaxDimension > 0 || limits.MaxPixels > 0 || limits.MaxBytes > 0
	if !needShrink {
		return Image{Data: data, MIMEType: mime}, nil
	}
	// 尺寸均达标且字节数达标：原样透传。
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Image{}, fmt.Errorf("decode %s image: %w", mime, err)
	}
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if limits.MaxBytes > 0 && len(data) <= limits.MaxBytes &&
		fitsDimensions(width, height, limits) {
		return Image{Data: data, MIMEType: mime}, nil
	}

	// 需要缩小的图片必须可解码重编码；WebP 标准库不解码，按不可处理跳过。
	if mime == "image/webp" {
		return Image{}, ErrImagesUnsupported
	}
	dst := src
	if !fitsDimensions(width, height, limits) {
		dst = shrink(src, limits)
	}

	// 重编码并按需降 JPEG 质量，直至字节数达标。
	outMIME := mime
	if outMIME == "image/gif" {
		outMIME = "image/png" // 静帧化，GIF 动图对嵌入/视觉模型无额外价值
	}
	for attempt := 0; ; attempt++ {
		var buf bytes.Buffer
		switch outMIME {
		case "image/jpeg":
			quality := 90 - attempt*15
			if quality < 35 {
				quality = 35
			}
			if err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
				return Image{}, fmt.Errorf("re-encode image: %w", err)
			}
		default:
			if err = png.Encode(&buf, dst); err != nil {
				return Image{}, fmt.Errorf("re-encode image: %w", err)
			}
			// PNG 无法压到限内时换 JPEG 再试一轮。
			if attempt == 0 && limits.MaxBytes > 0 && buf.Len() > limits.MaxBytes && outMIME == "image/png" && mime != "image/gif" {
				outMIME = "image/jpeg"
				continue
			}
		}
		if limits.MaxBytes <= 0 || buf.Len() <= limits.MaxBytes || attempt >= 3 {
			if limits.MaxBytes > 0 && buf.Len() > limits.MaxBytes {
				return Image{}, ErrImagesUnsupported
			}
			return Image{Data: buf.Bytes(), MIMEType: outMIME}, nil
		}
	}
}

func fitsDimensions(width, height int, limits Limits) bool {
	if limits.MaxDimension > 0 && (width > limits.MaxDimension || height > limits.MaxDimension) {
		return false
	}
	if limits.MaxPixels > 0 && width*height > limits.MaxPixels {
		return false
	}
	return true
}

// shrink 等比缩小到限制内（最近邻采样：嵌入/视觉模型对采样质量不敏感，
// 标准库实现零额外依赖）。
func shrink(src image.Image, limits Limits) image.Image {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	scale := 1.0
	if limits.MaxDimension > 0 {
		if s := float64(limits.MaxDimension) / float64(max(width, height)); s < scale {
			scale = s
		}
	}
	if limits.MaxPixels > 0 {
		if s := float64(limits.MaxPixels) / float64(width*height); s < scale {
			scale = s
		}
	}
	if scale >= 1.0 {
		return src
	}
	nw := max(1, int(float64(width)*scale))
	nh := max(1, int(float64(height)*scale))
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		sy := bounds.Min.Y + y*height/nh
		for x := 0; x < nw; x++ {
			sx := bounds.Min.X + x*width/nw
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

func acceptsMIME(limits Limits, mime string) bool {
	if len(limits.MIMETypes) == 0 {
		return true
	}
	for _, m := range limits.MIMETypes {
		if m == mime {
			return true
		}
	}
	return false
}
