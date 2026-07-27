package cli

// The coordinate grammar behind `--at` and friends (RFC-0014).
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

// parsePoint parses "x,y" in CSS pixels. Whitespace around either component is
// allowed because a shell-quoted coordinate often carries it.
//
// NaN and infinities are rejected rather than passed through: they would
// serialize into the CDP call and fail somewhere far less legible than here.
func parsePoint(s string) (chrome.Point, error) {
	x, y, ok := strings.Cut(s, ",")
	if !ok {
		return chrome.Point{}, fmt.Errorf("coordinate must be x,y in CSS pixels (got %q)", s)
	}
	px, err := parseCoord(x, s)
	if err != nil {
		return chrome.Point{}, err
	}
	py, err := parseCoord(y, s)
	if err != nil {
		return chrome.Point{}, err
	}
	return chrome.Point{X: px, Y: py}, nil
}

func parseCoord(part, whole string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("coordinate must be x,y in CSS pixels (got %q)", whole)
	}
	return v, nil
}
