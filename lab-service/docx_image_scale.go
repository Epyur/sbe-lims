package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"math"

	xdraw "golang.org/x/image/draw"
)

// ---- Уменьшение изображений в DOCX-отчёте (2026-10-19) ----
// Дизайн: docs/superpowers/specs/2026-10-19-sbe-lims-outbound-email-queue-design.md.
// Живой случай: отчёт заявки 494 весил 11,9 МБ — почти целиком шесть полноразмерных
// JPG (до 2,7 МБ каждый). Пережимаем крупные изображения до 1600 px по большей
// стороне (JPEG q80). Мелкие графики (PNG из charts.go) порог не превышают и
// остаются как есть — их чёткость важнее.

const (
	docxImageMaxSide     = 1600
	docxImageMaxBytes    = 250_000
	docxImageJPEGQuality = 80
)

// prepareDocxImage — уменьшает и пережимает картинку, если она крупная. Любая
// проблема (не декодировалось, не закодировалось) — возвращаем вход без
// изменений: рендер отчёта не должен ломаться из-за одного файла.
func prepareDocxImage(data []byte, ext, contentType string) ([]byte, string, string) {
	if len(data) == 0 {
		return data, ext, contentType
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return data, ext, contentType
	}
	if len(data) <= docxImageMaxBytes && cfg.Width <= docxImageMaxSide && cfg.Height <= docxImageMaxSide {
		return data, ext, contentType
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, ext, contentType
	}
	w, h := cfg.Width, cfg.Height
	scale := float64(docxImageMaxSide) / float64(max(w, h))
	nw := int(math.Round(float64(w) * scale))
	nh := int(math.Round(float64(h) * scale))
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	// Белый фон: JPEG не хранит прозрачность, иначе прозрачные области станут чёрными.
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: docxImageJPEGQuality}); err != nil {
		return data, ext, contentType
	}
	return out.Bytes(), "jpg", "image/jpeg"
}
