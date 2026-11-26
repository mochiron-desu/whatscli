package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func TestRenderImageToTviewHalfBlockColors(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 2))
	red := color.NRGBA{R: 255, G: 0, B: 0, A: 255}
	blue := color.NRGBA{R: 0, G: 0, B: 255, A: 255}
	for x := 0; x < 3; x++ {
		img.SetNRGBA(x, 0, red)
		img.SetNRGBA(x, 1, blue)
	}

	got := renderImageToTview(img, 4, 1)
	if !strings.Contains(got, "[#ff0000:#0000ff]▀") {
		t.Fatalf("expected half-block red/blue tag, got %q", got)
	}
	if !strings.HasSuffix(got, "[-:-] ") {
		t.Fatalf("expected trailing transparent space after [-:-], got %q", got)
	}
}

func TestDecodeImageBytesJPEGAndPNG(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 30, G: 180, B: 90, A: 255})
		}
	}

	var pngBuf, jpgBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	if err := jpeg.Encode(&jpgBuf, img, nil); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}

	for name, data := range map[string][]byte{"png": pngBuf.Bytes(), "jpg": jpgBuf.Bytes()} {
		decoded, err := decodeImageBytes(data)
		if err != nil {
			t.Fatalf("%s decode failed: %v", name, err)
		}
		art := renderImageToTview(decoded, 4, 2)
		if !strings.Contains(art, "▀") || !strings.Contains(art, "[#") {
			t.Fatalf("%s expected truecolor half-block art, got %q", name, art)
		}
	}
}
