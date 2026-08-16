package chrome

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/chromedp/cdproto/accessibility"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// focusedReadTimeout bounds the best-effort accessibility read that annotates a
// key press with what ended up focused. It is short on purpose: Chrome throttles
// the accessibility tree on a backgrounded tab, and the press has already landed
// by the time we ask, so waiting out the caller's full --timeout for a cosmetic
// field would turn a successful press into a timeout.
const focusedReadTimeout = 2 * time.Second

// Key dispatches keyboard events that are not literal text — named keys, modifier
// chords, and repeats.
//
// With a selector it resolves and focuses the element first, via the same
// occlusion-verified coordinate click Type uses, so it lands on a background tab
// where a box-model node click would poll until timeout. With an EMPTY selector
// it does no element resolution at all and dispatches to whatever the page
// currently has focused — which is the whole point of the verb for `key Escape`
// on a modal, or an arrow-key walk through a listbox, where the thing that must
// receive the key has no addressable selector.
func (c *CDP) Key(ctx context.Context, id, selector string, keys []KeyStroke, opts KeyOpts) (map[string]any, error) {
	if len(keys) == 0 {
		return nil, errors.New("no keys to press")
	}
	repeat := max(opts.Repeat, 1)

	core := chromedp.ActionFunc(func(actx context.Context) error {
		if selector != "" {
			if err := coordClickSelector(actx, selector, opts.Query, 1); err != nil {
				return err
			}
		}
		for i := 0; i < repeat; i++ {
			if i > 0 && opts.Delay > 0 {
				select {
				case <-actx.Done():
					return actx.Err()
				case <-time.After(opts.Delay):
				}
			}
			for _, k := range keys {
				if err := dispatchKeyStroke(actx, k); err != nil {
					return err
				}
			}
		}
		return nil
	})

	action, sink := withOptionalDialog(opts.Query, core)
	if err := c.run(ctx, id, bringToFront(), action); err != nil {
		return nil, err
	}

	res := map[string]any{"keys": KeyNames(keys), "repeat": repeat}
	if f := c.focusedDesc(ctx, id); f != "" {
		res["focused"] = f
	}
	return withDialogResult(res, sink), nil
}

// dispatchKeyStroke sends one stroke as the full physical sequence a real press
// produces: keyDown for each held modifier, the key itself, its keyUp, then the
// modifiers released in reverse.
//
// Every event carries the complete modifier bitmask, including the modifiers'
// own events. Pages branch on event.metaKey/ctrlKey rather than tracking presses,
// so a chord whose mask is set on only the key event reads as an unmodified press
// and the shortcut silently does the wrong thing.
func dispatchKeyStroke(ctx context.Context, k KeyStroke) error {
	mods := input.Modifier(k.Modifiers)

	for _, m := range modifierTable {
		if k.Modifiers&m.bit == 0 {
			continue
		}
		ev := keyEvent(input.KeyRawDown, m.key, m.code, m.keyCode, mods)
		if err := ev.Do(ctx); err != nil {
			return err
		}
	}

	down := keyEvent(input.KeyRawDown, k.Key, k.Code, k.KeyCode, mods)
	// Text is what actually inserts the character, so it is set for a printable
	// key and left empty for a named one. It is also suppressed when a non-Shift
	// modifier is held: cmd+a is select-all, and a keyDown carrying text would
	// ALSO type an "a" into the field it just selected.
	if k.Text != "" && k.Modifiers&^int64(input.ModifierShift) == 0 {
		down.Type = input.KeyDown
		down.Text = k.Text
		down.UnmodifiedText = unmodifiedText(k)
	}
	if err := down.Do(ctx); err != nil {
		return err
	}
	if err := keyEvent(input.KeyUp, k.Key, k.Code, k.KeyCode, mods).Do(ctx); err != nil {
		return err
	}

	for _, m := range slices.Backward(modifierTable) {
		if k.Modifiers&m.bit == 0 {
			continue
		}
		if err := keyEvent(input.KeyUp, m.key, m.code, m.keyCode, mods).Do(ctx); err != nil {
			return err
		}
	}
	return nil
}

func keyEvent(t input.KeyType, key, code string, keyCode int64, mods input.Modifier) *input.DispatchKeyEventParams {
	return &input.DispatchKeyEventParams{
		Type:                  t,
		Key:                   key,
		Code:                  code,
		WindowsVirtualKeyCode: keyCode,
		NativeVirtualKeyCode:  nativeKeyCode(keyCode),
		Modifiers:             mods,
	}
}

// nativeKeyCode is the native scan code to send alongside the Windows virtual
// key code. macOS scan codes are a different numbering entirely, so sending the
// Windows value there is worse than sending nothing — chromedp's kb.Encode zeroes
// it on darwin for the same reason, and this verb matches the behaviour `type`
// already has.
func nativeKeyCode(keyCode int64) int64 {
	if runtime.GOOS == "darwin" {
		return 0
	}
	return keyCode
}

// unmodifiedText is the character the key would insert with no Shift held ("a"
// for "A"). Chrome uses it to resolve accelerators, and getting it wrong makes a
// shifted key look like a different physical key.
func unmodifiedText(k KeyStroke) string {
	runes := []rune(k.Key)
	if len(runes) == 1 {
		if v, ok := kb.Keys[runes[0]]; ok && v.Unmodified != "" {
			return v.Unmodified
		}
	}
	return k.Text
}

// focusedDesc renders the currently-focused element as `role "name"`.
//
// It is strictly best-effort. The accessibility tree is throttled on a
// backgrounded tab, so this read is the one part of the verb that can stall for
// reasons that have nothing to do with whether the keys landed. A failure, a
// timeout, or a page with nothing focused all return "" and the field is simply
// omitted — never an error, because the press already succeeded.
//
// It reads the tree directly rather than going through Snapshot: Snapshot reports
// the FIRST node carrying the focused state, and the document root carries it
// whenever the window has focus — which bringToFront has just arranged. The
// useful answer is the element inside the document, which appears later.
func (c *CDP) focusedDesc(ctx context.Context, id string) string {
	fctx, cancel := context.WithTimeout(ctx, focusedReadTimeout)
	defer cancel()
	var nodes []*accessibility.Node
	err := c.run(fctx, id, chromedp.ActionFunc(func(actx context.Context) error {
		var e error
		nodes, e = accessibility.GetFullAXTree().Do(actx)
		return e
	}))
	if err != nil {
		return ""
	}
	for _, n := range nodes {
		if !axHasState(n, "focused") {
			continue
		}
		role, name := axString(n.Role), axString(n.Name)
		if role == "" || role == "RootWebArea" {
			continue
		}
		return strings.TrimSpace(fmt.Sprintf("%s %q", role, name))
	}
	return ""
}
