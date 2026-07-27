package cli

// The coordinate grammar behind `--at` and friends (RFC-0014), and the
// comma-separated number parsing `--region` shares with it.
//
// Parsing lives in the CLI, before any connection, because a malformed
// coordinate is a usage error the caller should not pay a Chrome round trip to
// discover — and because the driver should receive numbers, never a grammar.

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
)

// parseFloats parses exactly n comma-separated CSS-pixel numbers.
//
// NaN and the infinities are rejected rather than passed through: they would
// serialize into the CDP call and fail somewhere far less legible than here.
// The caller supplies the wording, because "--at 10;10" and "--region 1,2,3"
// are different mistakes and deserve different messages.
func parseFloats(s string, n int) ([]float64, bool) {
	parts := strings.Split(strings.TrimSpace(s), ",")
	if len(parts) != n {
		return nil, false
	}
	out := make([]float64, n)
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		out[i] = f
	}
	return out, true
}

// parsePoint parses "x,y" in CSS pixels. Whitespace around either component is
// allowed because a shell-quoted coordinate often carries it.
func parsePoint(s string) (chrome.Point, error) {
	v, ok := parseFloats(s, 2)
	if !ok {
		return chrome.Point{}, fmt.Errorf("coordinate must be x,y in CSS pixels (got %q)", s)
	}
	return chrome.Point{X: v[0], Y: v[1]}, nil
}
