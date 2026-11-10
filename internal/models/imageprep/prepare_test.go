package imageprep

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPreparePassthrough(t *testing.T) {
	data := pngBytes(t, 8, 8)
	img, err := Prepare(data, Limits{MaxBytes: len(data) + 1, MaxDimension: 16})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if img.MIMEType != "image/png" {
		t.Fatalf("mime = %s", img.MIMEType)
	}
	if !bytes.Equal(img.Data, data) {
		t.Fatal("small image should pass through byte-identical")
	}
	if img.DataURI() == "" || img.Base64() == "" {
		t.Fatal("DataURI/Base64 must be non-empty")
	}
}

func TestPrepareShrinksOversizedDimensions(t *testing.T) {
	data := pngBytes(t, 64, 64)
	img, err := Prepare(data, Limits{MaxDimension: 16})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(img.Data))
	if err != nil {
		t.Fatalf("decode prepared: %v", err)
	}
	if format != "png" {
		t.Fatalf("format = %s", format)
	}
	b := decoded.Bounds()
	if b.Dx() > 16 || b.Dy() > 16 {
		t.Fatalf("dimensions %dx%d exceed MaxDimension", b.Dx(), b.Dy())
	}
}

func TestPrepareRejectsUnknown(t *testing.T) {
	if _, err := Prepare([]byte("not an image"), Limits{}); err == nil {
		t.Fatal("expected error for unrecognized format")
	}
	if _, err := Prepare(nil, Limits{}); err == nil {
		t.Fatal("expected error for empty input")
	}
	if _, err := Prepare(pngBytes(t, 4, 4), Limits{MIMETypes: []string{"image/jpeg"}}); err == nil {
		t.Fatal("expected error for rejected mime")
	}
}

func TestPrepareMaxPixels(t *testing.T) {
	data := pngBytes(t, 32, 32)
	img, err := Prepare(data, Limits{MaxPixels: 64})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(img.Data))
	if err != nil {
		t.Fatal(err)
	}
	b := decoded.Bounds()
	if b.Dx()*b.Dy() > 64 {
		t.Fatalf("pixels %d exceed MaxPixels", b.Dx()*b.Dy())
	}
}
