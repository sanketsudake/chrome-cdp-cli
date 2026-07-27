package chrome

// Drop-zone upload (RFC-0014): deliver files to a target that has no
// <input type=file> behind it.
//
// RFC-0006 deliberately limited `upload` to real file inputs. Modern apps
// increasingly offer only a drop zone — a div with a `drop` listener — and for
// those there is no input to set, so the ordinary path has nothing to address.
//
// MECHANISM, and why this one (RFC-0014 open question 1, settled by fixture
// evidence rather than up front): a temporary hidden <input type=file> is
// injected, the files are attached to it with DOM.setFileInputFiles — the same
// real CDP file attachment RFC-0006 uses — and then page JS moves those File
// objects into a DataTransfer and dispatches dragenter → dragover → drop.
//
// The drag events are untrusted, but the files are real, and a live fixture
// confirmed this satisfies every shape a drop handler reads: dataTransfer.files,
// dataTransfer.items (with the correct MIME kind), items[0].getAsFile(), and the
// dataTransfer.types "Files" guard most libraries gate on. Input.dispatchDragEvent
// would produce trusted events but cannot carry a FileList, so it was not needed
// and is not used.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/chromedp/cdproto/dom"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// dropResult is what the page reports back about the delivery.
type dropResult struct {
	OK      bool   `json:"ok"`
	Why     string `json:"why"`
	Target  string `json:"target"`
	Name    string `json:"name"`
	Handled bool   `json:"handled"`
	Files   []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		Type string `json:"type"`
	} `json:"files"`
}

// uploadDrop delivers files by synthesized drag-and-drop.
func (c *CDP) uploadDrop(ctx context.Context, id string, paths []string, opts UploadOpts) (map[string]any, error) {
	var res dropResult
	err := c.run(ctx, id, bringToFront(), chromedp.ActionFunc(func(actx context.Context) error {
		// Resolve the drop target first: a selector that never resolves should
		// fail before any input is injected into the user's page.
		targetJS, err := dropTargetJS(actx, opts)
		if err != nil {
			return err
		}

		// The injected input is removed in a `finally`, so a throwing handler
		// cannot leave debris in the page the CLI is driving.
		doc, err := dom.GetDocument().Do(actx)
		if err != nil {
			return err
		}
		if err := evalVoid(actx, injectDropInputJS); err != nil {
			return err
		}
		nid, err := dom.QuerySelector(doc.NodeID, "#"+dropInputID).Do(actx)
		if err != nil || nid == 0 {
			_ = evalVoid(actx, removeDropInputJS)
			return fmt.Errorf("could not address the temporary upload input: %w", err)
		}
		if err := dom.SetFileInputFiles(paths).WithNodeID(nid).Do(actx); err != nil {
			_ = evalVoid(actx, removeDropInputJS)
			return fmt.Errorf("DOM.setFileInputFiles: %w", err)
		}

		out, exc, err := cdpruntime.Evaluate(fmt.Sprintf(dispatchDropJS, targetJS)).
			WithReturnByValue(true).Do(actx)
		if err != nil {
			return err
		}
		if exc != nil {
			return fmt.Errorf("dispatching the drop: %s", exc.Text)
		}
		if out == nil || len(out.Value) == 0 {
			return fmt.Errorf("the drop dispatch returned no result")
		}
		return json.Unmarshal([]byte(out.Value), &res)
	}))
	if err != nil {
		return nil, err
	}
	if !res.OK {
		return nil, fmt.Errorf("%s", res.Why)
	}

	files := make([]any, 0, len(res.Files))
	for _, f := range res.Files {
		files = append(files, map[string]any{"name": f.Name, "size": f.Size, "type": f.Type})
	}
	envelope := map[string]any{
		"mode":         "drop",
		"files":        files,
		"count":        len(files),
		"dropped_on":   map[string]any{"tag": res.Target, "name": res.Name},
		"drop_handled": res.Handled,
	}
	// A drop nothing consumed is reported, not treated as success-by-silence:
	// an unhandled drop looks identical to a handled one from the outside, and
	// the usual cause is addressing the wrong element.
	if !res.Handled {
		envelope["note"] = "the target did not consume the drop (no handler called preventDefault) — the files were delivered nowhere; check the drop target"
	}
	return envelope, nil
}

// dropTargetJS returns a JS expression evaluating to the drop target, and fails
// early when a selector names nothing.
func dropTargetJS(ctx context.Context, opts UploadOpts) (string, error) {
	if opts.DropAt != nil {
		if err := (&viewportGate{}).check(ctx, *opts.DropAt); err != nil {
			return "", err
		}
		return fmt.Sprintf("document.elementFromPoint(%g, %g)", opts.DropAt.X, opts.DropAt.Y), nil
	}
	nid, err := resolveNodeReady(ctx, opts.Drop, opts.Query)
	if err != nil {
		return "", err
	}
	// Hand the resolved node to JS through a marker attribute rather than
	// re-querying by selector: --by name/cell/label have no CSS spelling, so a
	// second lookup could land somewhere else entirely.
	if err := dom.SetAttributeValue(nid, dropMarkerAttr, "1").Do(ctx); err != nil {
		return "", err
	}
	return fmt.Sprintf("document.querySelector('[%s]')", dropMarkerAttr), nil
}

func evalVoid(ctx context.Context, expr string) error {
	_, exc, err := cdpruntime.Evaluate(expr).Do(ctx)
	if err != nil {
		return err
	}
	if exc != nil {
		return fmt.Errorf("%s", exc.Text)
	}
	return nil
}

const (
	dropInputID    = "__chrome_cdp_drop_input"
	dropMarkerAttr = "data-chrome-cdp-drop"
)

const injectDropInputJS = `(() => {
  const old = document.getElementById("` + dropInputID + `");
  if (old) old.remove();
  const i = document.createElement("input");
  i.type = "file";
  i.multiple = true;
  i.id = "` + dropInputID + `";
  i.style.display = "none";
  document.body.appendChild(i);
})()`

const removeDropInputJS = `(() => {
  const i = document.getElementById("` + dropInputID + `");
  if (i) i.remove();
  for (const el of document.querySelectorAll("[` + dropMarkerAttr + `]")) el.removeAttribute("` + dropMarkerAttr + `");
})()`

// dispatchDropJS moves the real Files onto a DataTransfer and dispatches the
// drag sequence at the target. %s is the target expression.
//
// `handled` records whether anything consumed the drop: a drop handler that
// means to accept files calls preventDefault, so an uncancelled drop is the
// signal that the files went nowhere. Reporting that is the difference between
// "delivered" and "dispatched into the void".
const dispatchDropJS = `(() => {
  const clean = () => {
    const i = document.getElementById("` + dropInputID + `");
    if (i) i.remove();
    for (const el of document.querySelectorAll("[` + dropMarkerAttr + `]")) el.removeAttribute("` + dropMarkerAttr + `");
  };
  try {
    const input = document.getElementById("` + dropInputID + `");
    if (!input || !input.files || !input.files.length) {
      return {ok: false, why: "the files did not attach to the temporary input"};
    }
    const target = %s;
    if (!target) return {ok: false, why: "the drop target is not on the page"};

    const dt = new DataTransfer();
    const files = [];
    for (const f of input.files) {
      dt.items.add(f);
      files.push({name: f.name, size: f.size, type: f.type});
    }
    const mk = (type) => new DragEvent(type, {bubbles: true, cancelable: true, dataTransfer: dt});
    target.dispatchEvent(mk("dragenter"));
    target.dispatchEvent(mk("dragover"));
    const drop = mk("drop");
    target.dispatchEvent(drop);

    const name = (target.getAttribute && (target.getAttribute("aria-label") || target.id)) ||
                 (target.textContent || "").replace(/\s+/g, " ").trim().slice(0, 60);
    return {ok: true, target: (target.tagName || "").toLowerCase(), name: name,
            handled: drop.defaultPrevented, files: files};
  } finally {
    clean();
  }
})()`
