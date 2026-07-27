package chrome

// Drop-zone upload (RFC-0014): deliver files to a target that has no
// <input type=file> behind it.
//
// RFC-0006 deliberately limited `upload` to real file inputs. Modern apps
// increasingly offer only a drop zone — a div with a `drop` listener — and for
// those there is no input to set, so the ordinary path has nothing to address.
//
// MECHANISM, and why this one (RFC-0014 open question 1, settled by fixture
// evidence rather than up front): a file input is created but NEVER attached to
// the document, the files are put on it with DOM.setFileInputFiles — the same
// real CDP file attachment RFC-0006 uses — and a function bound to the drop
// target moves those File objects into a DataTransfer and dispatches
// dragenter → dragover → drop.
//
// The drag events are untrusted, but the files are real, and a live fixture
// confirmed this satisfies every shape a drop handler reads: dataTransfer.files,
// dataTransfer.items (with the correct MIME kind), items[0].getAsFile(), and the
// dataTransfer.types "Files" guard most libraries gate on. Input.dispatchDragEvent
// would produce trusted events but cannot carry a FileList, so it is not used.
//
// THE PAGE IS NEVER MUTATED. Nothing is appended to the document and no
// attribute is written to the caller's element: the input lives only as a
// remote object handle, and the dispatch runs bound to the target node itself.
// That matters for three reasons beyond tidiness.
//   - An attached input would fire `change` on setFileInputFiles, and `change`
//     BUBBLES — so any page-global listener (an analytics script, a compromised
//     dependency, an XSS payload) would receive the user's real files, which a
//     native drag never permits. A detached node's events reach no one.
//   - Binding the dispatch to the resolved node runs it in THAT node's frame,
//     so a target inside a same-origin iframe (what --pierce exists for) works.
//   - There is no marker attribute to leak, so no failed run can misdirect a
//     later drop onto a stale-marked element.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/chromedp/cdproto/dom"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// ErrDropFailed is a drop the page could not accept — the target went away, or
// the files did not reach the dispatch. It is the caller's address to fix, not
// a protocol fault, so the CLI classifies it as a target failure rather than
// letting it fall through to the generic cdp_error an agent reads as "retry".
var ErrDropFailed = errors.New("the drop could not be delivered")

// IsDropFailure reports whether err is ErrDropFailed, including after the
// daemon RPC has flattened it to a plain message.
func IsDropFailure(err error) bool { return errIs(err, ErrDropFailed) }

// dropResult is what the page reports back about the delivery.
type dropResult struct {
	Target  string           `json:"target"`
	Name    string           `json:"name"`
	Handled bool             `json:"handled"`
	Files   []fileInputEntry `json:"files"`
}

// uploadDrop delivers files by synthesized drag-and-drop.
func (c *CDP) uploadDrop(ctx context.Context, id string, paths []string, opts UploadOpts) (map[string]any, error) {
	var res dropResult
	err := c.run(ctx, id, bringToFront(), chromedp.ActionFunc(func(actx context.Context) error {
		// Resolve the target first: a selector that never resolves should fail
		// before any file is attached to anything.
		target, err := dropTargetObject(actx, opts)
		if err != nil {
			return err
		}
		defer releaseObject(actx, target)

		input, err := detachedFileInput(actx)
		if err != nil {
			return err
		}
		// The handle is released on every exit. It is the only thing this verb
		// leaves behind even momentarily, and it is not reachable from the page.
		defer releaseObject(actx, input)

		if err := dom.SetFileInputFiles(paths).WithObjectID(input).Do(actx); err != nil {
			return fmt.Errorf("DOM.setFileInputFiles: %w", err)
		}

		out, exc, err := cdpruntime.CallFunctionOn(dispatchDropJS).
			WithObjectID(target).
			WithArguments([]*cdpruntime.CallArgument{{ObjectID: input}}).
			WithReturnByValue(true).
			Do(actx)
		if err != nil {
			return err
		}
		if exc != nil {
			// The page-side failures throw rather than returning a status, so
			// there is ONE error channel out of the dispatch, not two.
			return fmt.Errorf("%w: %s", ErrDropFailed, exc.Text)
		}
		if out == nil || len(out.Value) == 0 {
			return fmt.Errorf("%w: the drop dispatch returned no result", ErrDropFailed)
		}
		return json.Unmarshal([]byte(out.Value), &res)
	}))
	if err != nil {
		return nil, err
	}

	return dropEnvelope(res), nil
}

// dropEnvelope builds the result payload, the way uploadResult does for the
// ordinary path — pure, so the shape (which is public API) can be reasoned
// about without a browser.
func dropEnvelope(res dropResult) map[string]any {
	out := map[string]any{
		"mode":         "drop",
		"files":        filesToAny(res.Files),
		"count":        len(res.Files),
		"dropped_on":   map[string]any{"tag": res.Target, "name": res.Name},
		"drop_handled": res.Handled,
	}
	// A drop nothing consumed is reported, not treated as success-by-silence:
	// an unhandled drop looks identical to a handled one from the outside, and
	// the usual cause is addressing the wrong element.
	if !res.Handled {
		out["note"] = "the target did not consume the drop (no handler called preventDefault) — the files were delivered nowhere; check the drop target"
	}
	return out
}

// dropTargetObject resolves the drop target to a remote object handle.
//
// A handle rather than a selector string, because the dispatch is then bound to
// the node itself: no re-query that --by name/cell/label could not spell, no
// marker attribute written to the caller's page, and the function runs in the
// target's own frame.
func dropTargetObject(ctx context.Context, opts UploadOpts) (cdpruntime.RemoteObjectID, error) {
	if opts.DropAt != nil {
		if err := (&viewportGate{}).check(ctx, *opts.DropAt); err != nil {
			return "", err
		}
		expr := fmt.Sprintf(`document.elementFromPoint(%g, %g)`, opts.DropAt.X, opts.DropAt.Y)
		res, exc, err := cdpruntime.Evaluate(expr).Do(ctx)
		if err != nil {
			return "", err
		}
		if exc != nil {
			return "", fmt.Errorf("resolving the drop coordinate: %s", exc.Text)
		}
		if res == nil || res.ObjectID == "" {
			return "", fmt.Errorf("%w: no element at (%g,%g)", ErrDropFailed, opts.DropAt.X, opts.DropAt.Y)
		}
		return res.ObjectID, nil
	}
	nid, err := resolveNodeReady(ctx, opts.Drop, opts.Query)
	if err != nil {
		return "", err
	}
	obj, err := dom.ResolveNode().WithNodeID(nid).Do(ctx)
	if err != nil {
		return "", err
	}
	if obj == nil || obj.ObjectID == "" {
		return "", fmt.Errorf("%w: the drop target has no remote object", ErrDropFailed)
	}
	return obj.ObjectID, nil
}

// detachedFileInput creates a file input that is never added to the document.
func detachedFileInput(ctx context.Context) (cdpruntime.RemoteObjectID, error) {
	res, exc, err := cdpruntime.Evaluate(
		`(() => { const i = document.createElement("input"); i.type = "file"; i.multiple = true; return i; })()`).
		Do(ctx)
	if err != nil {
		return "", err
	}
	if exc != nil {
		return "", fmt.Errorf("creating the temporary file input: %s", exc.Text)
	}
	if res == nil || res.ObjectID == "" {
		return "", errors.New("creating the temporary file input returned no handle")
	}
	return res.ObjectID, nil
}

// releaseObject drops a remote object handle. Best-effort: a handle that
// outlives the call is collected with the page, and failing a delivered upload
// over its release would be the wrong trade.
func releaseObject(ctx context.Context, id cdpruntime.RemoteObjectID) {
	if id != "" {
		_ = cdpruntime.ReleaseObject(id).Do(ctx)
	}
}

// filesToAny renders file entries for the envelope. Shared with the ordinary
// upload path so both report a file the same way.
func filesToAny(files []fileInputEntry) []any {
	out := make([]any, 0, len(files))
	for _, f := range files {
		out = append(out, map[string]any{"name": f.Name, "size": f.Size, "type": f.Type})
	}
	return out
}

// dispatchDropJS runs bound to the drop target, with the detached input passed
// as an argument.
//
// `handled` records whether anything consumed the drop: a handler that means to
// accept files calls preventDefault, so an uncancelled drop is the signal that
// the files went nowhere. Reporting that is the difference between "delivered"
// and "dispatched into the void".
const dispatchDropJS = `function(input) {
  if (!input || !input.files || !input.files.length) {
    throw new Error("the files did not attach to the temporary input");
  }
  if (!this || !this.dispatchEvent) {
    throw new Error("the drop target is not an element");
  }
  const dt = new DataTransfer();
  const files = [];
  for (const f of input.files) {
    dt.items.add(f);
    files.push({name: f.name, size: f.size, type: f.type});
  }
  const mk = (type) => new DragEvent(type, {bubbles: true, cancelable: true, dataTransfer: dt});
  this.dispatchEvent(mk("dragenter"));
  this.dispatchEvent(mk("dragover"));
  const drop = mk("drop");
  this.dispatchEvent(drop);

  const name = (this.getAttribute && (this.getAttribute("aria-label") || this.id)) ||
               (this.textContent || "").replace(/\s+/g, " ").trim().slice(0, 60);
  return {target: (this.tagName || "").toLowerCase(), name: name,
          handled: drop.defaultPrevented, files: files};
}`
