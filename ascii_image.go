package main

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"strings"
	"sync"

	_ "golang.org/x/image/webp"
)

var renderedImageCache sync.Map

func decodeImageBytes(data []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err == nil {
		return img, nil
	}
	// Fallback for animated WebP or formats image.Decode doesn't handle natively.
	if ffmpegPath, lookErr := exec.LookPath("ffmpeg"); lookErr == nil {
		cmd := exec.Command(ffmpegPath, "-y", "-v", "error", "-i", "pipe:0", "-frames:v", "1", "-f", "image2pipe", "-vcodec", "png", "pipe:1")
		cmd.Stdin = bytes.NewReader(data)
		var out bytes.Buffer
		cmd.Stdout = &out
		if runErr := cmd.Run(); runErr == nil && out.Len() > 0 {
			if pngImg, _, decErr := image.Decode(&out); decErr == nil {
				return pngImg, nil
			}
		}
	}
	return nil, err
}

func sampleBoxRGBA(img image.Image, x0, y0, x1, y1 int) (uint8, uint8, uint8, uint8) {
	var sumR, sumG, sumB, sumA, count uint64
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			r16, g16, b16, a16 := img.At(x, y).RGBA()
			a8 := a16 >> 8
			if a16 > 0 {
				// Un-premultiply RGB so transparent sticker edges don't get dark fringes.
				r16 = (r16 * 0xffff) / a16
				g16 = (g16 * 0xffff) / a16
				b16 = (b16 * 0xffff) / a16
			}
			sumR += uint64(r16 >> 8)
			sumG += uint64(g16 >> 8)
			sumB += uint64(b16 >> 8)
			sumA += uint64(a8)
			count++
		}
	}
	if count == 0 {
		return 0, 0, 0, 0
	}
	return uint8(sumR / count), uint8(sumG / count), uint8(sumB / count), uint8(sumA / count)
}

func renderImageToTview(img image.Image, maxCols, maxRows int) string {
	if img == nil {
		return ""
	}
	if maxCols < 2 {
		maxCols = 2
	}
	if maxRows < 1 {
		maxRows = 1
	}

	bounds := img.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return ""
	}

	scaleX := float64(maxCols) / float64(srcW)
	scaleY := float64(maxRows*2) / float64(srcH)
	scale := scaleX
	if scaleY < scale {
		scale = scaleY
	}
	if scale > 1.0 {
		scale = 1.0
	}

	dstW := int(float64(srcW)*scale + 0.5)
	if dstW < 1 {
		dstW = 1
	}
	dstH := int(float64(srcH)*scale + 0.5)
	if dstH < 1 {
		dstH = 1
	}

	gridR := make([][]uint8, dstH)
	gridG := make([][]uint8, dstH)
	gridB := make([][]uint8, dstH)
	gridA := make([][]uint8, dstH)

	for dy := 0; dy < dstH; dy++ {
		gridR[dy] = make([]uint8, dstW)
		gridG[dy] = make([]uint8, dstW)
		gridB[dy] = make([]uint8, dstW)
		gridA[dy] = make([]uint8, dstW)

		sy0 := bounds.Min.Y + (dy*srcH)/dstH
		sy1 := bounds.Min.Y + ((dy+1)*srcH)/dstH
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		if sy1 > bounds.Max.Y {
			sy1 = bounds.Max.Y
		}

		for dx := 0; dx < dstW; dx++ {
			sx0 := bounds.Min.X + (dx*srcW)/dstW
			sx1 := bounds.Min.X + ((dx+1)*srcW)/dstW
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			if sx1 > bounds.Max.X {
				sx1 = bounds.Max.X
			}
			gridR[dy][dx], gridG[dy][dx], gridB[dy][dx], gridA[dy][dx] = sampleBoxRGBA(img, sx0, sy0, sx1, sy1)
		}
	}

	var sb strings.Builder
	for y := 0; y < dstH; y += 2 {
		styled := false
		for x := 0; x < dstW; x++ {
			r1, g1, b1, a1 := gridR[y][x], gridG[y][x], gridB[y][x], gridA[y][x]
			var r2, g2, b2, a2 uint8
			if y+1 < dstH {
				r2, g2, b2, a2 = gridR[y+1][x], gridG[y+1][x], gridB[y+1][x], gridA[y+1][x]
			}

			topOpaque := a1 >= 96
			botOpaque := a2 >= 96

			switch {
			case !topOpaque && !botOpaque:
				if styled {
					sb.WriteString("[-:-]")
					styled = false
				}
				sb.WriteByte(' ')
			case topOpaque && !botOpaque:
				fmt.Fprintf(&sb, "[#%02x%02x%02x:-]▀", r1, g1, b1)
				styled = true
			case !topOpaque && botOpaque:
				fmt.Fprintf(&sb, "[#%02x%02x%02x:-]▄", r2, g2, b2)
				styled = true
			default:
				fmt.Fprintf(&sb, "[#%02x%02x%02x:#%02x%02x%02x]▀", r1, g1, b1, r2, g2, b2)
				styled = true
			}
		}
		if styled {
			sb.WriteString("[-:-]")
		}
		if y+2 < dstH {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func renderBytesToTviewCached(cacheKey string, data []byte, maxCols, maxRows int) (string, error) {
	fullKey := fmt.Sprintf("%s:%dx%d", cacheKey, maxCols, maxRows)
	if v, ok := renderedImageCache.Load(fullKey); ok {
		return v.(string), nil
	}
	img, err := decodeImageBytes(data)
	if err != nil {
		return "", err
	}
	art := renderImageToTview(img, maxCols, maxRows)
	if art != "" {
		renderedImageCache.Store(fullKey, art)
	}
	return art, nil
}

func renderFileToTviewCached(path string, maxCols, maxRows int) (string, error) {
	fullKey := fmt.Sprintf("file:%s:%dx%d", path, maxCols, maxRows)
	if v, ok := renderedImageCache.Load(fullKey); ok {
		return v.(string), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	img, err := decodeImageBytes(data)
	if err != nil {
		return "", err
	}
	art := renderImageToTview(img, maxCols, maxRows)
	if art != "" {
		renderedImageCache.Store(fullKey, art)
	}
	return art, nil
}
