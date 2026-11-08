package devtools

import (
	"bytes"
	"fmt"
	"image/color"
	"image/png"
	"math"
)

// DefaultDiffThreshold is the per-pixel colour threshold of "diff
// screenshot" when --threshold is not given (agent-browser's default).
const DefaultDiffThreshold = 0.1

// PixelDiff is the result of a pixel comparison of two images.
type PixelDiff struct {
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Total     int     `json:"total"`     // pixels compared: Width * Height
	Different int     `json:"different"` // pixels over the threshold
	Ratio     float64 `json:"ratio"`     // Different / Total
}

// DiffPNG compares two PNG images pixel by pixel on the host (design 4.2,
// "diff screenshot"). A pixel differs when the largest difference of its
// red, green, blue or alpha value (non-premultiplied, 0 to 255) divided by
// 255 is above threshold; threshold 0 counts every change. The images must
// have the same size. NaN is refused: no comparison with it is true, so it
// would count no pixel.
func DiffPNG(baseline, screenshot []byte, threshold float64) (PixelDiff, error) {
	if math.IsNaN(threshold) || threshold < 0 || threshold >= 1 {
		return PixelDiff{}, fmt.Errorf("threshold %v: want a value from 0 up to (not including) 1", threshold)
	}
	a, err := png.Decode(bytes.NewReader(baseline))
	if err != nil {
		return PixelDiff{}, fmt.Errorf("decoding the baseline PNG: %w", err)
	}
	b, err := png.Decode(bytes.NewReader(screenshot))
	if err != nil {
		return PixelDiff{}, fmt.Errorf("decoding the screenshot PNG: %w", err)
	}
	ab, bb := a.Bounds(), b.Bounds()
	if ab.Dx() != bb.Dx() || ab.Dy() != bb.Dy() {
		return PixelDiff{}, fmt.Errorf("the baseline is %dx%d pixels and the screenshot %dx%d; take the baseline with the same viewport and screenshot kind", ab.Dx(), ab.Dy(), bb.Dx(), bb.Dy())
	}
	d := PixelDiff{Width: ab.Dx(), Height: ab.Dy(), Total: ab.Dx() * ab.Dy()}
	limit := threshold * 255
	for y := 0; y < d.Height; y++ {
		for x := 0; x < d.Width; x++ {
			pa := color.NRGBAModel.Convert(a.At(ab.Min.X+x, ab.Min.Y+y)).(color.NRGBA)
			pb := color.NRGBAModel.Convert(b.At(bb.Min.X+x, bb.Min.Y+y)).(color.NRGBA)
			if float64(maxChannelDiff(pa, pb)) > limit {
				d.Different++
			}
		}
	}
	if d.Total > 0 {
		d.Ratio = float64(d.Different) / float64(d.Total)
	}
	return d, nil
}

func maxChannelDiff(a, b color.NRGBA) uint8 {
	m := uint8(0)
	for _, p := range [][2]uint8{{a.R, b.R}, {a.G, b.G}, {a.B, b.B}, {a.A, b.A}} {
		d := p[0] - p[1]
		if p[1] > p[0] {
			d = p[1] - p[0]
		}
		m = max(m, d)
	}
	return m
}
