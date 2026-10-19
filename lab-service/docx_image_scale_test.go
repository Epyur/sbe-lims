package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func TestPrepareDocxImageDownscalesLargeJPEG(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2000, 1500))
	for x := 0; x < 2000; x++ {
		for y := 0; y < 1500; y++ {
			src.Set(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	in := buf.Bytes()

	out, ext, ct := prepareDocxImage(in, "jpg", "image/jpeg")
	if len(out) >= len(in) {
		t.Fatalf("expected smaller image: in=%d out=%d", len(in), len(out))
	}
	if ext != "jpg" || ct != "image/jpeg" {
		t.Fatalf("type: %s %s", ext, ct)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width > docxImageMaxSide || cfg.Height > docxImageMaxSide {
		t.Fatalf("not downscaled: %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.Width != 1600 || cfg.Height != 1200 {
		t.Fatalf("expected 1600x1200, got %dx%d", cfg.Width, cfg.Height)
	}
}

func TestPrepareDocxImageKeepsSmallAndBroken(t *testing.T) {
	small := image.NewRGBA(image.Rect(0, 0, 200, 150))
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, small, &jpeg.Options{Quality: 90})
	in := buf.Bytes()
	out, ext, _ := prepareDocxImage(in, "jpg", "image/jpeg")
	if !bytes.Equal(out, in) || ext != "jpg" {
		t.Fatal("small image must be returned unchanged")
	}
	broken := []byte("not an image")
	out, ext, _ = prepareDocxImage(broken, "png", "image/png")
	if !bytes.Equal(out, broken) || ext != "png" {
		t.Fatal("undecodable image must be returned unchanged")
	}
}
