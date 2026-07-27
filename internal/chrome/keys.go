package chrome

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp/kb"
)

// This file is the `key` verb's grammar (see ParseKeys): a pure parser with no
// context, no browser, and no I/O. The CLI runs it BEFORE resolving a target, so a
// malformed keyspec is a usage error (exit 2) that never launches or touches
// Chrome — the contract agents rely on to know "your call was wrong, don't retry".
//
// A multi-character token that is not a known name is REJECTED rather than
// silently typed as literal text: `key Ecsape` is a typo, and pressing E-c-s-a-p-e
// into a form is a far worse outcome than an error. Use `type` for literal text.

// modifierBits maps every accepted modifier spelling to its CDP modifier bit.
//
// `cmd` is Meta on every platform — deliberately NOT rewritten to Ctrl on
// Linux/Windows. The verb dispatches what the caller asked for; a page that
// wants a platform-appropriate accelerator is the caller's decision to make,
// and silently swapping the modifier would make `cmd+a` mean two different
// things depending on where the CLI happens to run.
var modifierBits = map[string]int64{
	"ctrl":  int64(input.ModifierCtrl),
	"shift": int64(input.ModifierShift),
	"alt":   int64(input.ModifierAlt),
	"cmd":   int64(input.ModifierMeta),
	"meta":  int64(input.ModifierMeta),
	"super": int64(input.ModifierMeta),
}

// modifierTable is the one row per modifier bit: its canonical keyspec spelling
// (and the print order a formatted stroke uses, so formatting is stable whatever
// order the caller wrote the modifiers in), plus the DOM key/code/virtual-keycode
// tuple dispatchKeyStroke presses the physical modifier with.
//
// The virtual key codes are the ones browsers report for a real press
// (Shift 16, Control 17, Alt 18, Meta 91). kb's table carries the *Left variants
// (160/162/164) because it describes scan codes, not the event.keyCode a page
// reads — a handler comparing e.keyCode === 17 must still see the Ctrl press.
var modifierTable = []struct {
	bit     int64
	name    string // keyspec spelling
	key     string // DOM KeyboardEvent.key for the physical modifier
	code    string
	keyCode int64
}{
	{int64(input.ModifierCtrl), "ctrl", "Control", "ControlLeft", 17},
	{int64(input.ModifierAlt), "alt", "Alt", "AltLeft", 18},
	{int64(input.ModifierShift), "shift", "Shift", "ShiftLeft", 16},
	{int64(input.ModifierMeta), "cmd", "Meta", "MetaLeft", 91},
}

// namedKeys maps every accepted spelling of a named key (lower-cased) to the
// rune chromedp's kb.Keys table is keyed by.
//
// The table stores only the NAME→rune edge; the DOM key/code/virtual-keycode
// tuple comes from kb, which is generated from Chrome's own key data. Hand-rolling
// those triples is how a verb ends up dispatching `code: ""` and being ignored by
// every framework that reads event.code.
//
// Bare modifier names (Shift, Control, Meta) are intentionally absent: they are
// modifiers in this grammar, and accepting them as keys too would make `shift`
// ambiguous.
var namedKeys = buildNamedKeys()

func buildNamedKeys() map[string]rune {
	m := make(map[string]rune, 96)
	add := func(kbConst string, names ...string) {
		r := []rune(kbConst)
		if len(r) != 1 {
			panic("chrome: named key constant is not a single rune: " + kbConst)
		}
		for _, n := range names {
			m[strings.ToLower(n)] = r[0]
		}
	}

	add(kb.Escape, "Escape", "Esc")
	add(kb.Enter, "Enter", "Return")
	add(kb.Tab, "Tab")
	add(kb.Backspace, "Backspace")
	add(kb.Delete, "Delete", "Del")
	add(" ", "Space", "Spacebar")
	add(kb.Insert, "Insert", "Ins")
	add(kb.Home, "Home")
	add(kb.End, "End")
	add(kb.PageUp, "PageUp", "PgUp")
	add(kb.PageDown, "PageDown", "PgDn")
	add(kb.ArrowUp, "ArrowUp", "Up")
	add(kb.ArrowDown, "ArrowDown", "Down")
	add(kb.ArrowLeft, "ArrowLeft", "Left")
	add(kb.ArrowRight, "ArrowRight", "Right")
	add(kb.ContextMenu, "ContextMenu", "Menu")
	add(kb.CapsLock, "CapsLock")
	add(kb.NumLock, "NumLock")
	add(kb.ScrollLock, "ScrollLock")
	add(kb.PrintScreen, "PrintScreen")
	add(kb.Pause, "Pause")
	add(kb.Clear, "Clear")
	add(kb.Help, "Help")
	add(kb.Copy, "Copy")
	add(kb.Cut, "Cut")
	add(kb.Paste, "Paste")
	add(kb.Undo, "Undo")
	add(kb.Redo, "Redo")

	for i, f := range []string{
		kb.F1, kb.F2, kb.F3, kb.F4, kb.F5, kb.F6, kb.F7, kb.F8,
		kb.F9, kb.F10, kb.F11, kb.F12, kb.F13, kb.F14, kb.F15, kb.F16,
		kb.F17, kb.F18, kb.F19, kb.F20, kb.F21, kb.F22, kb.F23, kb.F24,
	} {
		add(f, fmt.Sprintf("F%d", i+1))
	}
	return m
}

// ParseKeys parses a keyspec into the strokes to dispatch. It is pure: no
// context, no browser, no clock — so the CLI can validate the argument before it
// connects to Chrome, and so the grammar is testable without a renderer.
//
// Accepted forms, which may be combined into a space-separated sequence:
//
//	Escape        a named key (matched case-insensitively)
//	a  /  7       one printable character
//	cmd+a         a chord: modifiers, then exactly one key
//	"End shift+Home Backspace"   a sequence, pressed left to right
func ParseKeys(spec string) ([]KeyStroke, error) {
	tokens := strings.Fields(spec)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("empty keyspec: want a key name (Escape), a character (a), a chord (cmd+a), or a space-separated sequence (%q)", "End shift+Home Backspace")
	}
	out := make([]KeyStroke, 0, len(tokens))
	for _, tok := range tokens {
		k, err := parseKeyToken(tok)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

// ParseModifiers parses a `+`-joined modifier list ("cmd+shift") into the CDP
// modifier bitmask. An empty string is no modifiers, not an error.
//
// It is deliberately independent of ParseKeys: the pointer verbs take a
// modifier list with no key attached (`hover --modifiers alt`), and sharing the
// parser is what keeps `cmd` meaning Meta in exactly one place.
func ParseModifiers(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	var mods int64
	for part := range strings.SplitSeq(s, "+") {
		bit, err := modifierBit(strings.TrimSpace(part))
		if err != nil {
			return 0, err
		}
		mods |= bit
	}
	return mods, nil
}

// modifierNames renders a CDP modifier bitmask back into the canonical names, in
// modifierTable's order, so a mask never round-trips into a spelling
// ParseModifiers would reject. It returns an empty (never nil) slice, so the JSON
// is `[]` rather than `null`.
func modifierNames(mask int64) []string {
	names := make([]string, 0, len(modifierTable))
	for _, m := range modifierTable {
		if mask&m.bit != 0 {
			names = append(names, m.name)
		}
	}
	return names
}

func modifierBit(name string) (int64, error) {
	bit, ok := modifierBits[strings.ToLower(name)]
	if !ok {
		return 0, fmt.Errorf("unknown modifier %q: want ctrl, shift, alt, or cmd (aliases meta, super)", name)
	}
	return bit, nil
}

// parseKeyToken parses one whitespace-free token — modifiers plus exactly one key.
func parseKeyToken(tok string) (KeyStroke, error) {
	parts := strings.Split(tok, "+")
	// A literal "+" as the key splits into a pair of empty parts ("+" → ["", ""],
	// "cmd++" → ["cmd", "", ""]); fold those back into a single "+" key so the
	// plus character stays pressable without a separate escaping rule.
	if n := len(parts); n >= 2 && parts[n-1] == "" && parts[n-2] == "" {
		parts = append(parts[:n-2:n-2], "+")
	}
	key := parts[len(parts)-1]
	if key == "" {
		return KeyStroke{}, fmt.Errorf("key %q ends with '+': a chord is modifiers then exactly one key, e.g. ctrl+shift+k", tok)
	}

	var mods int64
	for _, m := range parts[:len(parts)-1] {
		bit, err := modifierBit(m)
		if err != nil {
			// The common shape of this error is a second non-modifier key
			// ("cmd+a+b"), so say what the grammar actually allows.
			return KeyStroke{}, fmt.Errorf("%w (in %q: a chord is modifiers then exactly one key)", err, tok)
		}
		mods |= bit
	}

	k, err := keyStrokeFor(key, mods)
	if err != nil {
		return KeyStroke{}, err
	}
	k.Modifiers |= mods
	return k, nil
}

// keyStrokeFor resolves a single key part — a named key or one printable
// character — to its DOM key/code/keycode tuple, for a press held with mods.
//
// The modifiers are not decoration here. Shift does not merely set a bit: it
// changes which character the physical key produces, so `shift+a` must resolve
// to "A" (inserting "A") exactly as the bare `A` spelling does. Resolving the
// base rune and OR-ing Shift on afterwards dispatches key "a" with text "a" and
// the Shift bit set — a press no keyboard can make, which types a lowercase
// letter and never fires a handler written `if (e.key === "A")`.
func keyStrokeFor(key string, mods int64) (KeyStroke, error) {
	r, err := keyRune(key)
	if err != nil {
		return KeyStroke{}, err
	}
	if mods&int64(input.ModifierShift) != 0 {
		if shifted, ok := shiftTwin[r]; ok {
			r = shifted
		}
	}
	return strokeForRune(r), nil
}

// keyRune resolves a key part to the rune kb's table is keyed by — a named key's
// sentinel rune, or the single character itself.
func keyRune(key string) (rune, error) {
	if r, ok := namedKeys[strings.ToLower(key)]; ok {
		return r, nil
	}
	runes := []rune(key)
	if len(runes) != 1 {
		return 0, fmt.Errorf("unknown key name %q: a multi-character key must be a known name (Escape, Enter, Tab, Space, Home, PageDown, ArrowUp, F1…F24, …); use `type` to send literal text", key)
	}
	if _, known := kb.Keys[runes[0]]; !known && !unicode.IsPrint(runes[0]) {
		return 0, fmt.Errorf("key %q is neither a known key name nor a printable character", key)
	}
	return runes[0], nil
}

// shiftTwin maps a character to the one the SAME physical key produces with
// Shift held — 'a'→'A', '1'→'!', '/'→'?'. It is derived from kb's own table
// (two entries sharing a Code, one with Shift set) rather than hand-written, so
// it stays correct for every key Chrome knows about and needs no upkeep.
//
// Named keys are absent by construction: Home, Tab and the rest have no shifted
// character, which is why `shift+Home` is unaffected — and why the live tests,
// which only ever pressed shift with named keys, never caught this.
var shiftTwin = buildShiftTwin()

func buildShiftTwin() map[rune]rune {
	base := map[string]rune{}
	shifted := map[string]rune{}
	for r, v := range kb.Keys {
		if v.Code == "" {
			continue
		}
		if v.Shift {
			shifted[v.Code] = r
		} else {
			base[v.Code] = r
		}
	}
	m := make(map[rune]rune, len(shifted))
	for code, s := range shifted {
		if b, ok := base[code]; ok {
			m[b] = s
		}
	}
	return m
}

// strokeForRune builds the stroke for a rune, taking the key/code/keycode tuple
// from kb where it has one. Shift is folded into the stroke's modifiers for a
// character that requires it ("A", "?"), because that is physically what the
// press is — and CDP will not produce the shifted character otherwise.
func strokeForRune(r rune) KeyStroke {
	v, ok := kb.Keys[r]
	if !ok {
		// Outside kb's table (an accented or non-Latin character): dispatch it as
		// text with no code/keycode. It still inserts; it just has no physical key.
		s := string(r)
		return KeyStroke{Key: s, Text: s}
	}
	k := KeyStroke{Key: v.Key, Code: v.Code, KeyCode: v.Windows}
	if v.Print {
		k.Text = v.Text
	}
	if v.Shift {
		k.Modifiers |= int64(input.ModifierShift)
	}
	return k
}

// String formats a stroke back into the keyspec syntax that parses to it, so the
// result envelope can echo what was pressed in the same language the caller used.
func (k KeyStroke) String() string {
	mods := k.Modifiers
	if impliesShift(k) {
		// "A" already means shift+a; spelling the modifier out would be noise and
		// would make the round-trip through String→ParseKeys non-idempotent.
		mods &^= int64(input.ModifierShift)
	}
	var b strings.Builder
	for _, name := range modifierNames(mods) {
		b.WriteString(name)
		b.WriteByte('+')
	}
	b.WriteString(keyName(k))
	return b.String()
}

// FormatKeys renders a sequence of strokes as a single keyspec.
func FormatKeys(keys []KeyStroke) string {
	return strings.Join(KeyNames(keys), " ")
}

// KeyNames returns the canonical name for every stroke, in order — the `keys`
// field of the `key` verb's result envelope.
func KeyNames(keys []KeyStroke) []string {
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k.String()
	}
	return names
}

// keyName is the spelling of the key itself, without modifiers. The DOM key
// value is the canonical name for everything except Space, whose key value is a
// literal space and would vanish inside a space-separated sequence.
func keyName(k KeyStroke) string {
	switch {
	case k.Key == " ":
		return "Space"
	case k.Key == "":
		return k.Code
	default:
		return k.Key
	}
}

// impliesShift reports whether the key character already carries Shift.
func impliesShift(k KeyStroke) bool {
	runes := []rune(k.Key)
	if len(runes) != 1 {
		return false
	}
	v, ok := kb.Keys[runes[0]]
	return ok && v.Shift && v.Key == k.Key
}
