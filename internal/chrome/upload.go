package chrome

// The `upload` verb (RFC-0006): attach local files to a file input.
//
// It NEVER clicks the input. Clicking a file input opens the native OS file
// dialog — outside the page, invisible to CDP, blocking the browser's main
// thread, and with no CDP method that can dismiss it. That is a strictly worse
// wedge than the JavaScript dialogs --on-dialog exists for, and it is why any
// automation that reaches a file input by clicking is already broken.
// DOM.setFileInputFiles sets the input's FileList directly and fires `change`,
// which is the only correct way to do this.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// The upload failures that are the CALLER's bug rather than a timing problem.
// Each one happens AFTER the selector resolved, so retrying cannot help; the
// CLI maps them to usage / exit 2 so an agent can tell "fix your selector" from
// "wait longer" (RFC-0006 US-4).
var (
	// ErrNotFileInput reports that the selector resolved to something that is
	// not an <input type=file>. The message names the element actually found.
	ErrNotFileInput = errors.New("element is not a file input")

	// ErrNotMultiple reports that more than one file was given to an input
	// without the `multiple` attribute. It is raised BEFORE dispatch, so the
	// input is left untouched.
	ErrNotMultiple = errors.New("input does not accept multiple files")

	// ErrAppendUnknown reports that --append was asked for on an input whose
	// existing files this session did not set, and therefore cannot re-send.
	ErrAppendUnknown = errors.New("--append cannot be honoured: this input's current files were not set by this session")
)

// IsUploadUsage reports whether err is one of the upload failures the caller
// must fix rather than retry. It matches the sentinel first and then its
// message, because an error raised inside the daemon reaches the CLI as a plain
// string and has lost its Go type — the same reason IsOccluded does.
func IsUploadUsage(err error) bool {
	if err == nil {
		return false
	}
	for _, sentinel := range []error{ErrNotFileInput, ErrNotMultiple, ErrAppendUnknown} {
		if errors.Is(err, sentinel) || strings.Contains(err.Error(), sentinel.Error()) {
			return true
		}
	}
	return false
}

// changeWitnessWait bounds how long the read-back waits for the `change` event
// setFileInputFiles fires. The event is dispatched as a task, so it can trail
// the command's response by a tick; past this window the honest report is
// change_fired: false rather than a longer stall.
const changeWitnessWait = 500 * time.Millisecond

// Upload attaches local files to a file input via DOM.setFileInputFiles.
//
// The paths must be absolute and already validated (the CLI does that before
// connecting). Everything this method checks is something only the page can
// answer: is the resolved element really a file input, does it accept several
// files, and what does it hold afterwards.
func (c *CDP) Upload(ctx context.Context, id, selector string, paths []string, opts UploadOpts) (map[string]any, error) {
	// The drop form addresses a target that is not a file input at all, so it
	// shares none of the checks below (is it an input, does it take multiple).
	if opts.Drop != "" || opts.DropAt != nil {
		return c.uploadDrop(ctx, id, paths, opts)
	}
	var out map[string]any
	err := c.run(ctx, id, bringToFront(), chromedp.ActionFunc(func(actx context.Context) error {
		var nodes []*cdp.Node
		if err := chromedp.Nodes(selector, &nodes, query(selector, opts.Query)...).Do(actx); err != nil {
			return err
		}
		if len(nodes) == 0 {
			return fmt.Errorf("selector %q not found", selector)
		}
		node := nodes[0]
		obj, err := dom.ResolveNode().WithNodeID(node.NodeID).Do(actx)
		if err != nil {
			return err
		}
		if obj == nil || obj.ObjectID == "" {
			return fmt.Errorf("node has no remote object")
		}

		before, err := probeFileInput(actx, obj.ObjectID)
		if err != nil {
			return err
		}
		// The element-type check is cheap and it is what turns a confusing
		// timeout into a one-line fix: name the tag and type actually found.
		if !before.isFileInput() {
			return fmt.Errorf("%w: selector %q resolved to %s", ErrNotFileInput, selector, before.describe())
		}

		files := paths
		if opts.Append {
			prior, ok := uploadHistory.get(c, id, node.BackendNodeID)
			if !ok {
				return fmt.Errorf("%w (selector %q) — upload all the files in one call instead", ErrAppendUnknown, selector)
			}
			files = append(append(make([]string, 0, len(prior)+len(paths)), prior...), paths...)
		}
		if len(files) > 1 && !before.Multiple {
			return fmt.Errorf("%w: selector %q resolved to an <input type=file> without the `multiple` attribute, but %d files were given",
				ErrNotMultiple, selector, len(files))
		}

		// Arm the witness BEFORE dispatch, so change_fired is observed rather
		// than assumed (US-6).
		if err := armChangeWitness(actx, obj.ObjectID); err != nil {
			return err
		}
		if err := dom.SetFileInputFiles(files).WithNodeID(node.NodeID).Do(actx); err != nil {
			return fmt.Errorf("DOM.setFileInputFiles: %w", err)
		}

		after, err := readBackFileInput(actx, obj.ObjectID)
		if err != nil {
			return err
		}
		uploadHistory.set(c, id, node.BackendNodeID, files)
		out = uploadResult(after, files)
		return nil
	}))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// fileInputState is what the page says about the addressed element: enough to
// decide whether it is a valid target, and to report the result as evidence
// rather than an echo of the arguments.
type fileInputState struct {
	Tag      string           `json:"tag"`
	Type     string           `json:"type"`
	Multiple bool             `json:"multiple"`
	Accept   string           `json:"accept"`
	Files    []fileInputEntry `json:"files"`
	Changed  bool             `json:"changed"`
}

// fileInputEntry is one File as the page sees it. Note there is no path: the
// DOM deliberately does not expose one, which is what makes --append the
// limited thing it is.
type fileInputEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Type string `json:"type"`
}

func (s fileInputState) isFileInput() bool { return s.Tag == "input" && s.Type == "file" }

// describe names the element in the form a selector-fixing human or agent can
// act on: `input[type=text]`, or a bare tag for anything that is not an input.
func (s fileInputState) describe() string {
	if s.Tag == "input" {
		return fmt.Sprintf("input[type=%s]", s.Type)
	}
	if s.Tag == "" {
		return "a non-element node"
	}
	return s.Tag
}

// fileInputStateJS reports the addressed element's identity and current
// FileList. `changed` reads the witness armed by armChangeWitnessJS.
const fileInputStateJS = `function() {
  const files = [];
  if (this.files) { for (const f of this.files) files.push({name: f.name, size: f.size, type: f.type}); }
  const attrType = (this.getAttribute && this.getAttribute("type")) || "";
  return {
    tag: (this.tagName || "").toLowerCase(),
    type: String(attrType || this.type || "").toLowerCase(),
    multiple: !!this.multiple,
    accept: (this.getAttribute && this.getAttribute("accept")) || "",
    files: files,
    changed: !!this.__cdpChangeSeen,
  };
}`

// armChangeWitnessJS records, on the element itself, that a `change` event
// fired. It is a property rather than a captured closure so it survives the
// handlers a real uploader runs — including one that clears the input, which is
// exactly the case the read-back has to report faithfully (VS-11).
const armChangeWitnessJS = `function() {
  const el = this;
  el.__cdpChangeSeen = false;
  el.addEventListener("change", function once() { el.__cdpChangeSeen = true; }, {once: true});
  return true;
}`

func probeFileInput(ctx context.Context, objID runtime.RemoteObjectID) (fileInputState, error) {
	var s fileInputState
	res, err := callOnObject(ctx, objID, fileInputStateJS)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(res, &s); err != nil {
		return s, fmt.Errorf("reading the file input's state: %w", err)
	}
	return s, nil
}

// readBackFileInput re-reads the input after the dispatch, waiting briefly for
// the `change` event to land. The FileList is already final when
// setFileInputFiles returns; only the event is asynchronous, so a missing
// witness is the sole reason to poll — and it gives up rather than hanging.
func readBackFileInput(ctx context.Context, objID runtime.RemoteObjectID) (fileInputState, error) {
	deadline := time.Now().Add(changeWitnessWait)
	for {
		s, err := probeFileInput(ctx, objID)
		if err != nil || s.Changed || time.Now().After(deadline) {
			return s, err
		}
		t := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return s, nil
		case <-t.C:
		}
	}
}

func armChangeWitness(ctx context.Context, objID runtime.RemoteObjectID) error {
	_, err := callOnObject(ctx, objID, armChangeWitnessJS)
	return err
}

// uploadResult builds the envelope payload from the state read back off the
// element — not from the arguments. `accept` and `multiple` are reported
// because an accept/multiple mismatch is the most common reason an upload
// appears to succeed and then silently does nothing.
func uploadResult(s fileInputState, sent []string) map[string]any {
	files := make([]any, 0, len(s.Files))
	for _, f := range s.Files {
		files = append(files, map[string]any{"name": f.Name, "size": f.Size, "type": f.Type})
	}
	out := map[string]any{
		"files":        files,
		"count":        len(files),
		"multiple":     s.Multiple,
		"accept":       s.Accept,
		"change_fired": s.Changed,
	}
	// A warning, never a refusal: `accept` is advisory in HTML and plenty of
	// apps set it loosely, so refusing here would break working uploads.
	if acceptMismatch(s.Accept, sent) {
		out["accept_mismatch"] = true
	}
	return out
}

// acceptMismatch reports whether any sent file falls outside the input's
// `accept` list. An empty or unparseable list covers everything.
func acceptMismatch(accept string, paths []string) bool {
	tokens := acceptTokens(accept)
	if len(tokens) == 0 {
		return false
	}
	for _, p := range paths {
		if !acceptCovers(tokens, p) {
			return true
		}
	}
	return false
}

func acceptTokens(accept string) []string {
	var out []string
	for _, t := range strings.Split(accept, ",") {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// acceptCovers matches one path against the `accept` grammar: an extension
// (".pdf"), a MIME type ("application/pdf"), or a MIME wildcard ("image/*").
func acceptCovers(tokens []string, path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	mediaType := ""
	if ext != "" {
		mediaType = strings.ToLower(strings.TrimSpace(strings.SplitN(mime.TypeByExtension(ext), ";", 2)[0]))
	}
	for _, t := range tokens {
		switch {
		case t == "*" || t == "*/*":
			return true
		case strings.HasPrefix(t, "."):
			if t == ext {
				return true
			}
		case strings.HasSuffix(t, "/*"):
			if mediaType != "" && strings.HasPrefix(mediaType, strings.TrimSuffix(t, "*")) {
				return true
			}
		default:
			if mediaType != "" && t == mediaType {
				return true
			}
		}
	}
	return false
}

// uploadKey identifies one file input on one tab of one connection.
type uploadKey struct {
	conn   *CDP
	target string
	node   cdp.BackendNodeID
}

// uploadLog remembers the absolute paths this process last set on each file
// input.
//
// It exists because --append is otherwise unimplementable rather than merely
// awkward: setFileInputFiles replaces the FileList wholesale, and the existing
// entries' paths are NOT readable back from the DOM (File.name is the bare name
// by design, for the obvious security reason). So the only files that can
// honestly be re-sent are the ones this CLI sent itself, and an append onto
// anything else has to be refused instead of silently dropping what was there.
//
// The state belongs to the connection — the daemon holds one for a whole
// session, which is exactly the window in which "add this to what I just
// uploaded" is meaningful — so it is keyed by the *CDP that set it and dies
// with the process. The backend node id keys the input itself: it is stable
// while the node lives, and a reload mints a new one, which correctly makes an
// append onto a freshly reset input "unknown" rather than wrong.
type uploadLog struct {
	mu sync.Mutex
	m  map[uploadKey][]string
}

var uploadHistory = &uploadLog{m: map[uploadKey][]string{}}

func (l *uploadLog) get(c *CDP, target string, node cdp.BackendNodeID) ([]string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	paths, ok := l.m[uploadKey{conn: c, target: target, node: node}]
	return paths, ok
}

func (l *uploadLog) set(c *CDP, target string, node cdp.BackendNodeID, paths []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[uploadKey{conn: c, target: target, node: node}] = append([]string(nil), paths...)
}
