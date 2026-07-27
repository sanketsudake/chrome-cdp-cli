package cli

// CLI-level contract for coordinate addressing (RFC-0014): --at is mutually
// exclusive with every element-addressing form, and every malformed
// combination is exit 2 before Chrome is contacted.

import (
	"context"
	"fmt"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

type pointerAtCapture struct {
	fakeBrowser
	selector string
	opts     chrome.PointerOpts
	scroll   chrome.ScrollOpts
	window   chrome.WindowOpts
}

func (p *pointerAtCapture) Pointer(_ context.Context, _, selector string, opts chrome.PointerOpts) (map[string]any, error) {
	p.selector, p.opts = selector, opts
	return map[string]any{"action": string(opts.Action)}, nil
}

func (p *pointerAtCapture) Scroll(_ context.Context, _, _ string, opts chrome.ScrollOpts) (map[string]any, error) {
	p.scroll = opts
	return map[string]any{"scrolled": "ok"}, nil
}

func (p *pointerAtCapture) Window(_ context.Context, _ string, opts chrome.WindowOpts) (chrome.WindowBounds, error) {
	p.window = opts
	return chrome.WindowBounds{Left: 0, Top: 25, Width: 1280, Height: 800, State: "normal"}, nil
}

func atCapture() *pointerAtCapture {
	return &pointerAtCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "t1", Title: "T", URL: "u"}}}}
}

// VS-4: --at with a selector or any element-addressing flag never connects.
func TestCoordinateValidationBeforeConnect(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"at with selector":       {"click", "#x", "--at", "10,10"},
		"at with by":             {"click", "--at", "10,10", "--by", "name"},
		"at with role":           {"click", "--at", "10,10", "--role", "button"},
		"at with nth":            {"click", "--at", "10,10", "--nth", "2"},
		"at with match":          {"click", "--at", "10,10", "--match", "contains"},
		"at with in-row":         {"click", "--at", "10,10", "--in-row", "r"},
		"malformed at":           {"click", "--at", "10;10"},
		"no selector and no at":  {"click"},
		"drag to and to-at":      {"drag", "--at", "1,1", "--to", "#b", "--to-at", "2,2"},
		"drag to-at and dx":      {"drag", "--at", "1,1", "--to-at", "2,2", "--dx", "5"},
		"drag no destination":    {"drag", "--at", "1,1"},
		"wheel at without wheel": {"scroll", "--at", "10,10", "--dy", "-100"},
		"window bad size":        {"window", "size", "0", "600"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, _, code := run(t, noCall(t), append(args, "--json")...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (env %v)", code, env)
			}
			if env["error"].(map[string]any)["code"] != "usage" {
				t.Errorf("error.code = %v, want usage", env["error"])
			}
		})
	}
}

// A negative dimension is caught by cobra's own flag parsing (it reads "-1" as
// a flag), which still means exit 2 and still never reaches Chrome — it just
// reports before --json is parsed, so only the exit code is assertable.
func TestWindowNegativeDimensionExitsUsage(t *testing.T) {
	t.Parallel()
	if _, _, code := run(t, noCall(t), "--json", "window", "size", "-1", "600"); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestCoordinateFlagsReachDriver(t *testing.T) {
	t.Parallel()

	t.Run("click --at", func(t *testing.T) {
		t.Parallel()
		b := atCapture()
		if _, _, code := run(t, b, "click", "--at", "512,340", "--target", "t1", "--json"); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if b.opts.At == nil || b.opts.At.X != 512 || b.opts.At.Y != 340 {
			t.Errorf("At = %v, want 512,340", b.opts.At)
		}
		if b.selector != "" {
			t.Errorf("selector = %q, want empty for the coordinate form", b.selector)
		}
	})

	t.Run("drag --at --to-at", func(t *testing.T) {
		t.Parallel()
		b := atCapture()
		if _, _, code := run(t, b, "drag", "--at", "10,20", "--to-at", "30,40", "--target", "t1", "--json"); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if b.opts.At == nil || b.opts.ToAt == nil || b.opts.ToAt.X != 30 || b.opts.ToAt.Y != 40 {
			t.Errorf("At=%v ToAt=%v", b.opts.At, b.opts.ToAt)
		}
	})

	t.Run("mixed drag: element start, coordinate end", func(t *testing.T) {
		t.Parallel()
		b := atCapture()
		if _, _, code := run(t, b, "drag", "#card", "--to-at", "900,300", "--target", "t1", "--json"); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if b.selector != "#card" || b.opts.At != nil || b.opts.ToAt == nil {
			t.Errorf("selector=%q At=%v ToAt=%v", b.selector, b.opts.At, b.opts.ToAt)
		}
	})

	t.Run("tripleclick maps to the action", func(t *testing.T) {
		t.Parallel()
		b := atCapture()
		if _, _, code := run(t, b, "tripleclick", "p.abstract", "--target", "t1", "--json"); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if b.opts.Action != chrome.PointerTripleClick || b.selector != "p.abstract" {
			t.Errorf("action=%v selector=%q", b.opts.Action, b.selector)
		}
	})

	t.Run("scroll --wheel --at", func(t *testing.T) {
		t.Parallel()
		b := atCapture()
		if _, _, code := run(t, b, "scroll", "--wheel", "--at", "512,340", "--dy", "-240", "--target", "t1", "--json"); code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if b.scroll.At == nil || b.scroll.At.X != 512 || b.scroll.Dy != -240 {
			t.Errorf("scroll opts = %+v", b.scroll)
		}
	})
}

func TestWindowVerb(t *testing.T) {
	t.Parallel()

	t.Run("size passes dimensions and reports settled bounds", func(t *testing.T) {
		t.Parallel()
		b := atCapture()
		env, _, code := run(t, b, "window", "size", "1280", "800", "--target", "t1", "--json")
		if code != 0 {
			t.Fatalf("exit = %d, env %v", code, env)
		}
		if b.window.Width != 1280 || b.window.Height != 800 {
			t.Errorf("WindowOpts = %+v", b.window)
		}
		res := env["result"].(map[string]any)
		if res["width"] != 1280.0 || res["height"] != 800.0 || res["state"] != "normal" {
			t.Errorf("result = %v", res)
		}
	})

	t.Run("info reports without resizing", func(t *testing.T) {
		t.Parallel()
		b := atCapture()
		env, _, code := run(t, b, "window", "info", "--target", "t1", "--json")
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		if b.window.Width != 0 || b.window.Height != 0 {
			t.Errorf("info resized: %+v", b.window)
		}
		if env["result"].(map[string]any)["left"] != 0.0 {
			t.Errorf("result = %v", env["result"])
		}
	})
}

// A driver-reported out-of-viewport coordinate becomes its own error code and
// exit 4 — not a generic CDP failure — with the viewport in the details so a
// caller can re-screenshot at the right size.
type oobBrowser struct {
	fakeBrowser
}

func (oobBrowser) Pointer(context.Context, string, string, chrome.PointerOpts) (map[string]any, error) {
	return nil, fmt.Errorf("%w: (9999,10) is outside the 1280x800 viewport", chrome.ErrCoordinateOOB)
}

func (oobBrowser) Eval(context.Context, string, string, chrome.EvalOpts) (any, error) {
	return map[string]any{"value": map[string]any{"w": 1280.0, "h": 800.0}}, nil
}

func TestCoordinateOutOfBoundsClassification(t *testing.T) {
	t.Parallel()
	b := &oobBrowser{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "t1", Title: "T", URL: "u"}}}}
	env, _, code := run(t, b, "click", "--at", "9999,10", "--target", "t1", "--json")
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (env %v)", code, env)
	}
	e, _ := env["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error object in envelope: %v", env)
	}
	if e["code"] != "coordinate_out_of_bounds" {
		t.Errorf("error.code = %v", e["code"])
	}
	// Details are flattened onto the error object, the way `occluded` and
	// `tab_hidden` already are.
	vp, _ := e["viewport"].(map[string]any)
	if vp == nil || vp["width"] != 1280.0 {
		t.Errorf("error.viewport = %v, want the measured viewport", e)
	}
}
