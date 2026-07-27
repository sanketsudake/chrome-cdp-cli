package chrome

// Coordinate-space interaction (RFC-0014): acting at a viewport point instead
// of at a resolved element.
//
// Every other addressing mode answers "where is the thing I named"; this one
// takes the answer as given. That is what makes canvas, WebGL, map, and
// drawing surfaces drivable at all — the accessibility tree sees one node
// there, so no selector can reach inside it — and it is the shape a
// screenshot-reading agent already thinks in.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	cdpruntime "github.com/chromedp/cdproto/runtime"
)

// ErrCoordinateOOB is a coordinate outside the current viewport.
//
// It is an error rather than a clamp on purpose: the usual cause is a window
// that is not the size the caller thought, and clamping would convert a
// detectable mistake into a click on whatever happens to sit at the edge.
var ErrCoordinateOOB = errors.New("coordinate is outside the viewport")

// IsCoordinateOOB reports whether err is ErrCoordinateOOB, including after the
// daemon RPC has flattened it to a plain message.
func IsCoordinateOOB(err error) bool { return errIs(err, ErrCoordinateOOB) }

// viewportSize reads the layout viewport in CSS pixels.
func viewportSize(ctx context.Context) (w, h float64, err error) {
	var v struct{ W, H float64 }
	res, exc, err := cdpruntime.Evaluate(`({W: window.innerWidth, H: window.innerHeight})`).
		WithReturnByValue(true).Do(ctx)
	if err != nil {
		return 0, 0, err
	}
	if exc != nil {
		return 0, 0, fmt.Errorf("viewport probe: %s", exc.Text)
	}
	if res == nil || len(res.Value) == 0 {
		return 0, 0, errors.New("viewport probe returned no value")
	}
	if err := json.Unmarshal([]byte(res.Value), &v); err != nil {
		return 0, 0, err
	}
	return v.W, v.H, nil
}

// checkInViewport rejects a coordinate outside the viewport BEFORE anything is
// dispatched, so a refused gesture leaves the page untouched.
func checkInViewport(ctx context.Context, p Point) error {
	w, h, err := viewportSize(ctx)
	if err != nil {
		// A viewport we cannot measure is not grounds to refuse the caller's
		// explicit instruction; dispatch and let Chrome decide.
		return nil
	}
	if p.X < 0 || p.Y < 0 || p.X > w || p.Y > h {
		return fmt.Errorf("%w: (%g,%g) is outside the %gx%g viewport", ErrCoordinateOOB, p.X, p.Y, w, h)
	}
	return nil
}

// hitAt reports what sits under a coordinate: the tag, id, ARIA role, and
// accessible name of the topmost element there.
//
// It is observability, never a precondition. A canvas app's every coordinate
// resolves to the same <canvas>, which is exactly the case coordinate
// addressing exists to serve — so this describes the target, and refuses
// nothing.
func hitAt(ctx context.Context, p Point) map[string]any {
	expr := fmt.Sprintf(`(() => {
  const el = document.elementFromPoint(%g, %g);
  if (!el) return null;`+axNameHelpersJS+`
  return {tag: el.tagName, id: el.id || undefined,
          role: roleOf(el) || undefined, name: norm(accName(el)) || undefined};
})()`, p.X, p.Y)
	res, exc, err := cdpruntime.Evaluate(expr).WithReturnByValue(true).Do(ctx)
	if err != nil || exc != nil || res == nil || len(res.Value) == 0 {
		return nil
	}
	var hit map[string]any
	if json.Unmarshal([]byte(res.Value), &hit) != nil {
		return nil
	}
	return hit
}
