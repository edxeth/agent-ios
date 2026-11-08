//go:build ignore

// Writes the hand-made PNG fixtures of internal/devtools/testdata/diff.
// Every pixel is listed by hand below; the expected diff counts in
// imagediff_test.go come from these lists, not from the diff code.
package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

var white = color.NRGBA{255, 255, 255, 255}

func write(dir, name string, w, h int, set map[[2]int]color.NRGBA) {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, white)
		}
	}
	for p, c := range set {
		img.SetNRGBA(p[0], p[1], c)
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		panic(err)
	}
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
	f.Close()
}

func main() {
	dir := os.Args[1]
	write(dir, "base.png", 4, 3, nil)
	write(dir, "same.png", 4, 3, nil)
	write(dir, "two_px.png", 4, 3, map[[2]int]color.NRGBA{{0, 0}: {0, 0, 0, 255}, {3, 2}: {255, 0, 0, 255}})
	write(dir, "faint.png", 4, 3, map[[2]int]color.NRGBA{{1, 1}: {240, 240, 240, 255}, {2, 1}: {200, 255, 255, 255}})
	write(dir, "transparent.png", 4, 3, map[[2]int]color.NRGBA{{0, 2}: {255, 255, 255, 0}})
	write(dir, "small.png", 3, 3, nil)
}
