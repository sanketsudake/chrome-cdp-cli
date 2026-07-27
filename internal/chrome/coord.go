package chrome

// Coordinate-space interaction (RFC-0014): acting at a viewport point instead
// of at a resolved element.
//
// Every other addressing mode answers "where is the thing I named"; this one
// takes the answer as given. That is what makes canvas, WebGL, map, and
// drawing surfaces drivable at all — the accessibility tree sees one node
// there, so no selector can reach inside it — and it is the shape a
// screenshot-reading agent already thinks in.
//
// The measurement itself lives in geometry.go, with the element-geometry
// primitive, so there is one definition of the page's coordinate space.

import (
	"context"
	"errors"
	"fmt"
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

// viewportGate validates coordinates against the viewport, reading it at most
// once per gesture.
//
// A drag checks two points, and nothing between them can resize the window, so
// the second check reuses the first reading rather than paying for a second
// round trip.
type viewportGate struct {
	w, h   float64
	probed bool
	failed bool // the probe itself errored; see check
}

// check rejects p if it lies outside the viewport.
//
// A viewport that cannot be measured is not grounds to refuse the caller's
// explicit instruction: the check exists to catch a wrong-sized window, not to
// gate the gesture on a working probe. So a failed probe allows and lets Chrome
// decide, and — having failed once — is not retried for the second point.
func (g *viewportGate) check(ctx context.Context, p Point) error {
	if !g.probed {
		g.probed = true
		probe, err := probePoint(ctx, p, false)
		if err != nil {
			g.failed = true
		} else {
			g.w, g.h = probe.VW, probe.VH
		}
	}
	if g.failed {
		return nil
	}
	if p.X < 0 || p.Y < 0 || p.X > g.w || p.Y > g.h {
		return fmt.Errorf("%w: (%g,%g) is outside the %gx%g viewport", ErrCoordinateOOB, p.X, p.Y, g.w, g.h)
	}
	return nil
}

// checkAndDescribe validates p and, in the same round trip, reports what sits
// under it. Used for the gestures whose envelope carries a `hit`.
func (g *viewportGate) checkAndDescribe(ctx context.Context, p Point) (map[string]any, error) {
	probe, err := probePoint(ctx, p, true)
	if err != nil {
		// Probe failed: allow, and record that so a second point does not retry.
		g.probed, g.failed = true, true
		return nil, nil
	}
	g.probed, g.w, g.h = true, probe.VW, probe.VH
	if p.X < 0 || p.Y < 0 || p.X > g.w || p.Y > g.h {
		return nil, fmt.Errorf("%w: (%g,%g) is outside the %gx%g viewport", ErrCoordinateOOB, p.X, p.Y, g.w, g.h)
	}
	return probe.Hit, nil
}
