package chrome

// The `storage` verb (RFC-0019): read and write the tab's localStorage /
// sessionStorage, keyed by the top frame's security origin from
// Page.getFrameTree, over the DevTools DOMStorage domain. See
// docs/rfc/0019-web-storage.md.

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/chromedp/cdproto/domstorage"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/sanketsudake/chrome-cdp-cli/internal/eventbuf"
)

// DefaultStorageMaxValue caps ONE listed value, in bytes, before it leaves the
// driver. Exported so the CLI's --max-value default and this constant are one
// number, the way DefaultNetMaxBody is for `net --body`.
const DefaultStorageMaxValue = 4 << 10

// ErrOpaqueOrigin reports that the tab's top frame has no identifiable
// security origin — data:, about:blank, and a sandboxed document all report
// Page.getFrameTree's securityOrigin as the literal "://" — so it has no web
// storage area to read or write.
//
// Its message is part of the contract: match it with IsOpaqueOrigin rather
// than errors.Is at call sites, the way ErrNoHistory and ErrNoDialog are
// matched (see errIs in pointer.go) — the daemon RPC flattens it to a string.
var ErrOpaqueOrigin = errors.New("the tab's top frame has an opaque origin (data:, about:blank and sandboxed documents have no web storage)")

// IsOpaqueOrigin reports whether err is ErrOpaqueOrigin.
func IsOpaqueOrigin(err error) bool { return errIs(err, ErrOpaqueOrigin) }

// StorageListOpts are the render-time options of StorageList. Like NetOpts,
// they shape what is REPORTED, never what is read: redaction and the cap are
// applied to Chrome's answer before it leaves the driver.
type StorageListOpts struct {
	NoRedact bool // report values verbatim (the explicit opt-out)
	MaxValue int  // cut each value at this many bytes; 0 = no cap
}

// storageIsLocal maps the CLI's scope string onto DOMStorage's boolean. The
// CLI validates scope itself; this is the belt to that braces — the driver
// never trusts the RPC's lenient decoding of a daemon call it did not shape.
func storageIsLocal(scope string) (bool, error) {
	switch scope {
	case "local":
		return true, nil
	case "session":
		return false, nil
	default:
		return false, fmt.Errorf("storage: scope must be \"local\" or \"session\", got %q", scope)
	}
}

// storageID resolves the tab's top-frame origin with one Page.getFrameTree
// call and builds the StorageId every DOMStorage command takes, checking for
// an opaque origin BEFORE any DOMStorage command is ever issued — the
// pre-check named in the RFC's Design notes, so the message names the cause
// instead of Chrome's "Security origin cannot access local storage".
//
// The returned origin string is also what the envelope reports: one round
// trip serves both StorageId.securityOrigin and result.origin.
func (c *CDP) storageID(ctx context.Context, id, scope string) (*domstorage.StorageID, string, error) {
	isLocal, err := storageIsLocal(scope)
	if err != nil {
		return nil, "", err
	}
	var tree *page.FrameTree
	if err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		var e error
		tree, e = page.GetFrameTree().Do(ctx)
		return e
	})); err != nil {
		return nil, "", err
	}
	if tree == nil || tree.Frame == nil {
		return nil, "", fmt.Errorf("storage: could not read the tab's frame tree")
	}
	origin := tree.Frame.SecurityOrigin
	if origin == "" || origin == "://" {
		return nil, "", fmt.Errorf("%w: %s", ErrOpaqueOrigin, tree.Frame.URL)
	}
	return &domstorage.StorageID{SecurityOrigin: origin, IsLocalStorage: isLocal}, origin, nil
}

// StorageList reads every key in one storage area of the tab's top frame
// (RFC-0019), sorted by key, redacted and size-capped per opts.
func (c *CDP) StorageList(ctx context.Context, id, scope string, opts StorageListOpts) (map[string]any, error) {
	sid, origin, err := c.storageID(ctx, id, scope)
	if err != nil {
		return nil, err
	}
	var entries []domstorage.Item
	if err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		var e error
		entries, e = domstorage.GetDOMStorageItems(sid).Do(ctx)
		return e
	})); err != nil {
		return nil, err
	}
	return storageListResult(origin, scope, entries, opts), nil
}

// StorageGet reads one key raw and uncapped: the protocol has no single-item
// read, so it scans getDOMStorageItems' answer for the key. Absent is
// present:false, value:"" — not an error (US-2's acceptance).
func (c *CDP) StorageGet(ctx context.Context, id, scope, key string) (map[string]any, error) {
	sid, origin, err := c.storageID(ctx, id, scope)
	if err != nil {
		return nil, err
	}
	var entries []domstorage.Item
	if err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		var e error
		entries, e = domstorage.GetDOMStorageItems(sid).Do(ctx)
		return e
	})); err != nil {
		return nil, err
	}
	for _, e := range entries {
		if len(e) == 2 && e[0] == key {
			return map[string]any{"scope": scope, "origin": origin, "key": key, "value": e[1], "present": true}, nil
		}
	}
	return map[string]any{"scope": scope, "origin": origin, "key": key, "value": "", "present": false}, nil
}

// StorageSet creates or overwrites one key.
func (c *CDP) StorageSet(ctx context.Context, id, scope, key, value string) (map[string]any, error) {
	sid, origin, err := c.storageID(ctx, id, scope)
	if err != nil {
		return nil, err
	}
	if err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		return domstorage.SetDOMStorageItem(sid, key, value).Do(ctx)
	})); err != nil {
		return nil, err
	}
	return map[string]any{"scope": scope, "origin": origin, "key": key, "set": true}, nil
}

// StorageRemove deletes one key. Chrome's removeDOMStorageItem of an absent
// key succeeds (measured, RFC's Design notes), so this never pre-reads to
// report whether the key existed — the same shape CookieDelete has.
func (c *CDP) StorageRemove(ctx context.Context, id, scope, key string) (map[string]any, error) {
	sid, origin, err := c.storageID(ctx, id, scope)
	if err != nil {
		return nil, err
	}
	if err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		return domstorage.RemoveDOMStorageItem(sid, key).Do(ctx)
	})); err != nil {
		return nil, err
	}
	return map[string]any{"scope": scope, "origin": origin, "key": key, "removed": true}, nil
}

// StorageClear removes every key in one storage area.
func (c *CDP) StorageClear(ctx context.Context, id, scope string) (map[string]any, error) {
	sid, origin, err := c.storageID(ctx, id, scope)
	if err != nil {
		return nil, err
	}
	if err := c.run(ctx, id, chromedp.ActionFunc(func(ctx context.Context) error {
		return domstorage.Clear(sid).Do(ctx)
	})); err != nil {
		return nil, err
	}
	return map[string]any{"scope": scope, "origin": origin, "cleared": true}, nil
}

// storageEntry is one sorted (key, value) pair, the pure intermediate
// storageListResult sorts before rendering.
type storageEntry struct{ key, value string }

// storageListResult renders a listed storage area as the envelope's result
// payload: sort by key, redact, THEN cap — the pure core of StorageList, so
// its ordering is pinned by a test that needs no Chrome.
//
// Redaction before the cap is load-bearing (RFC-0019 Design notes): a value
// cut mid-token no longer has its closing quote, so RedactBody's JSON rule
// would not match and the token's prefix would be reported in clear.
func storageListResult(origin, scope string, entries []domstorage.Item, opts StorageListOpts) map[string]any {
	pairs := make([]storageEntry, 0, len(entries))
	for _, e := range entries {
		if len(e) != 2 {
			continue // malformed entry (not [key, value]): skip rather than panic
		}
		pairs = append(pairs, storageEntry{key: e[0], value: e[1]})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })

	items := make([]map[string]any, 0, len(pairs))
	anyTruncated := false
	for _, p := range pairs {
		v := p.value
		if !opts.NoRedact {
			v = storageRedact(p.key, v)
		}
		cut, truncated := eventbuf.TruncateText(v, opts.MaxValue)
		item := map[string]any{"key": p.key, "value": cut}
		if truncated {
			item["truncated"] = true
			anyTruncated = true
		}
		items = append(items, item)
	}
	return map[string]any{
		"scope": scope, "origin": origin, "items": items,
		"count": len(items), "truncated": anyTruncated,
	}
}

// storageRedact is the pure rule `list` applies to one entry (RFC-0019):
// withhold the value wholesale when the KEY is credential-shaped, otherwise
// redact credential-shaped fields inside it — the same predicates `net`
// applies to headers, URL parameters and bodies (RFC-0003), reused verbatim
// so there is one rule set rather than two that could drift apart.
func storageRedact(key, value string) string {
	if RedactedHeaderName(key) || RedactedParamName(key) {
		return NetRedacted
	}
	return RedactBody(value, false)
}
