// Package encode's annotate.go is the RFC-0016 label-drawing unit: a pure
// AnnotateImage over the same disc/mark primitives encode.go's recording
// pipeline uses (markRed, markWhite, markRadius, disc live there), plus the
// badge, digit font, and rectangle fill only labels need. Split out the way
// har.go is: a coherent, dependency-light unit with its own bitmap font.

package encode

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"strconv"
)

// Label is a numbered position marker: the disc-and-ring of a Mark plus the
// number N. X, Y are CSS pixels from the capture's top-left, Mark's space.
type Label struct {
	N    int
	X, Y float64
}

// AnnotateImage decodes data (png or jpeg), draws each label, and re-encodes
// as format ("png" | "jpeg"; quality applies to jpeg only). It reports, per
// label, whether it put any pixel on the canvas — the meaning Annotated has
// for a recording. It is pure: synthetic-image tests pin the drawing.
//
// A label whose centre falls outside the decoded image draws nothing (disc,
// ring, AND badge) rather than clamping the badge onto the canvas anyway —
// internal/chrome has already dropped every candidate outside the capture's
// clip, so this only matters for a caller (a test, a future format) that
// hands in a point it never checked.
func AnnotateImage(data []byte, format string, quality int, cssW, cssH float64, labels []Label) ([]byte, []bool, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)

	// CSS pixels -> canvas pixels, the same mapping drawMarks uses (kx, ky
	// there), so a screenshot label and a recording's pointer mark scale
	// identically at any device pixel ratio.
	kx, ky := float64(b.Dx()), float64(b.Dy())
	if cssW > 0 {
		kx /= cssW
	}
	if cssH > 0 {
		ky /= cssH
	}
	r := max(int(math.Round(markRadius*math.Min(kx, ky))), 3)

	drawn := make([]bool, len(labels))
	db := dst.Bounds()
	for i, l := range labels {
		cx := int(math.Round(l.X * kx))
		cy := int(math.Round(l.Y * ky))
		if cx < db.Min.X || cx >= db.Max.X || cy < db.Min.Y || cy >= db.Max.Y {
			continue
		}
		ring := disc(dst, cx, cy, r+2, markWhite)
		disc(dst, cx, cy, r, markRed)
		badge := drawBadge(dst, cx, cy, r, l.N, markRed, markWhite)
		drawn[i] = ring || badge
	}

	var out bytes.Buffer
	switch format {
	case "jpeg":
		q := quality
		if q <= 0 {
			q = 80
		}
		if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: q}); err != nil {
			return nil, nil, err
		}
	default:
		if err := png.Encode(&out, dst); err != nil {
			return nil, nil, err
		}
	}
	return out.Bytes(), drawn, nil
}

// digitFont5x7 is a built-in bitmap font for '0'-'9': five bits wide (bit 4 is
// the leftmost column) by seven rows tall. Just enough to make a label's
// number legible on its badge, with no font dependency — a screenshot label
// draws a digit or two, never prose.
var digitFont5x7 = map[byte][7]uint8{
	'0': {0b01110, 0b10001, 0b10011, 0b10101, 0b11001, 0b10001, 0b01110},
	'1': {0b00100, 0b01100, 0b00100, 0b00100, 0b00100, 0b00100, 0b01110},
	'2': {0b01110, 0b10001, 0b00001, 0b00010, 0b00100, 0b01000, 0b11111},
	'3': {0b11111, 0b00010, 0b00100, 0b00010, 0b00001, 0b10001, 0b01110},
	'4': {0b00010, 0b00110, 0b01010, 0b10010, 0b11111, 0b00010, 0b00010},
	'5': {0b11111, 0b10000, 0b11110, 0b00001, 0b00001, 0b10001, 0b01110},
	'6': {0b00110, 0b01000, 0b10000, 0b11110, 0b10001, 0b10001, 0b01110},
	'7': {0b11111, 0b00001, 0b00010, 0b00100, 0b01000, 0b01000, 0b01000},
	'8': {0b01110, 0b10001, 0b10001, 0b01110, 0b10001, 0b10001, 0b01110},
	'9': {0b01110, 0b10001, 0b10001, 0b01111, 0b00001, 0b00010, 0b01100},
}

// drawBadge draws a label's number: a filled rectangle in c (the marker's red)
// with a 1px border in border (white), anchored at the disc's upper-right
// (+r, -r from the centre) and shifted inward when it would otherwise leave
// the image. It reports whether any pixel landed inside the canvas.
func drawBadge(dst *image.RGBA, cx, cy, r, n int, c, border color.RGBA) bool {
	digits := strconv.Itoa(n)
	k := max(1, int(math.Round(float64(r)/6)))
	digitW, digitH := 5*k, 7*k
	gap, pad, bw := k, k, 1

	textW := len(digits)*digitW + (len(digits)-1)*gap
	boxW := textW + 2*pad + 2*bw
	boxH := digitH + 2*pad + 2*bw

	bx, by := cx+r, cy-r-boxH
	b := dst.Bounds()
	if bx+boxW > b.Max.X {
		bx = b.Max.X - boxW
	}
	if bx < b.Min.X {
		bx = b.Min.X
	}
	if by < b.Min.Y {
		by = b.Min.Y
	}
	if by+boxH > b.Max.Y {
		by = b.Max.Y - boxH
	}

	drew := fillRect(dst, bx, by, boxW, boxH, border)
	if fillRect(dst, bx+bw, by+bw, boxW-2*bw, boxH-2*bw, c) {
		drew = true
	}
	for i, ch := range digits {
		gx := bx + bw + pad + i*(digitW+gap)
		gy := by + bw + pad
		if drawDigit(dst, gx, gy, k, byte(ch), border) {
			drew = true
		}
	}
	return drew
}

// drawDigit draws one glyph from digitFont5x7 at integer scale k, top-left at
// (x,y). Reports whether any pixel landed inside the image.
func drawDigit(dst *image.RGBA, x, y, k int, ch byte, c color.RGBA) bool {
	rows, ok := digitFont5x7[ch]
	if !ok {
		return false
	}
	drew := false
	for row, bits := range rows {
		for col := 0; col < 5; col++ {
			if bits&(1<<uint(4-col)) == 0 {
				continue
			}
			if fillRect(dst, x+col*k, y+row*k, k, k, c) {
				drew = true
			}
		}
	}
	return drew
}

// fillRect fills a rectangle, clipped to the image, reporting whether any
// pixel landed inside it — the same contract disc keeps.
func fillRect(dst *image.RGBA, x, y, w, h int, c color.RGBA) bool {
	drew := false
	b := dst.Bounds()
	for yy := y; yy < y+h; yy++ {
		if yy < b.Min.Y || yy >= b.Max.Y {
			continue
		}
		for xx := x; xx < x+w; xx++ {
			if xx < b.Min.X || xx >= b.Max.X {
				continue
			}
			dst.SetRGBA(xx, yy, c)
			drew = true
		}
	}
	return drew
}
