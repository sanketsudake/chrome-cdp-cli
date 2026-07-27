package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
)

const (
	modAlt   = int64(input.ModifierAlt)
	modCtrl  = int64(input.ModifierCtrl)
	modMeta  = int64(input.ModifierMeta)
	modShift = int64(input.ModifierShift)
)

// ParseKeys is the whole contract of the `key` verb's argument: it runs before
// the CLI connects, so every case here is decided without a browser.
func TestParseKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec string
		want []KeyStroke
	}{
		{
			name: "named key",
			spec: "Escape",
			want: []KeyStroke{{Key: "Escape", Code: "Escape", KeyCode: 27}},
		},
		{
			name: "named key is case-insensitive",
			spec: "escape",
			want: []KeyStroke{{Key: "Escape", Code: "Escape", KeyCode: 27}},
		},
		{
			name: "named key alias",
			spec: "esc",
			want: []KeyStroke{{Key: "Escape", Code: "Escape", KeyCode: 27}},
		},
		{
			name: "arrow key",
			spec: "ArrowDown",
			want: []KeyStroke{{Key: "ArrowDown", Code: "ArrowDown", KeyCode: 40}},
		},
		{
			name: "function key",
			spec: "F7",
			want: []KeyStroke{{Key: "F7", Code: "F7", KeyCode: 118}},
		},
		{
			name: "space is a name, and is printable",
			spec: "Space",
			want: []KeyStroke{{Key: " ", Code: "Space", KeyCode: 32, Text: " "}},
		},
		{
			name: "single printable letter carries its text",
			spec: "a",
			want: []KeyStroke{{Key: "a", Code: "KeyA", KeyCode: 65, Text: "a"}},
		},
		{
			name: "single printable punctuation",
			spec: "/",
			want: []KeyStroke{{Key: "/", Code: "Slash", KeyCode: 191, Text: "/"}},
		},
		{
			name: "a shifted character folds Shift into the stroke",
			spec: "A",
			want: []KeyStroke{{Key: "A", Code: "KeyA", KeyCode: 65, Text: "A", Modifiers: modShift}},
		},
		{
			name: "cmd chord maps to Meta, not Ctrl",
			spec: "cmd+a",
			want: []KeyStroke{{Key: "a", Code: "KeyA", KeyCode: 65, Text: "a", Modifiers: modMeta}},
		},
		{
			name: "meta alias",
			spec: "meta+a",
			want: []KeyStroke{{Key: "a", Code: "KeyA", KeyCode: 65, Text: "a", Modifiers: modMeta}},
		},
		{
			name: "super alias",
			spec: "super+a",
			want: []KeyStroke{{Key: "a", Code: "KeyA", KeyCode: 65, Text: "a", Modifiers: modMeta}},
		},
		{
			// Shift is held, so the key the page sees is "K" — which is what a
			// browser reports for a real ctrl+shift+k, and what a handler
			// comparing e.key === "K" is written against.
			name: "multi-modifier chord",
			spec: "ctrl+shift+k",
			want: []KeyStroke{{Key: "K", Code: "KeyK", KeyCode: 75, Text: "K", Modifiers: modCtrl | modShift}},
		},
		{
			name: "modifier order does not matter",
			spec: "shift+ctrl+k",
			want: []KeyStroke{{Key: "K", Code: "KeyK", KeyCode: 75, Text: "K", Modifiers: modCtrl | modShift}},
		},
		{
			name: "modifiers are case-insensitive",
			spec: "CMD+ALT+Escape",
			want: []KeyStroke{{Key: "Escape", Code: "Escape", KeyCode: 27, Modifiers: modMeta | modAlt}},
		},
		{
			name: "chord on a named key",
			spec: "shift+Home",
			want: []KeyStroke{{Key: "Home", Code: "Home", KeyCode: 36, Modifiers: modShift}},
		},
		{
			// Shift does not just set a bit: it changes which character the key
			// produces. A real shift+a is key "A" inserting "A"; dispatching key
			// "a" with text "a" and the Shift bit set types a lowercase letter and
			// never fires a handler written `if (e.key === "A")`.
			name: "shift on a letter resolves the SHIFTED character",
			spec: "shift+a",
			want: []KeyStroke{{Key: "A", Code: "KeyA", KeyCode: 65, Text: "A", Modifiers: modShift}},
		},
		{
			name: "shift on a digit resolves its symbol",
			spec: "shift+1",
			want: []KeyStroke{{Key: "!", Code: "Digit1", KeyCode: 49, Text: "!", Modifiers: modShift}},
		},
		{
			// The physical key is the same one, so the virtual keycode and code
			// stay the digit's — only the produced character shifts.
			name: "shift on a digit keeps the physical key",
			spec: "shift+/",
			want: []KeyStroke{{Key: "?", Code: "Slash", KeyCode: 191, Text: "?", Modifiers: modShift}},
		},
		{
			// A named key has no shifted twin, which is why the live tests never
			// caught the letter case: shift+Home carries no text at all.
			name: "shift on a named key is unaffected",
			spec: "shift+Tab",
			want: []KeyStroke{{Key: "Tab", Code: "Tab", KeyCode: 9, Modifiers: modShift}},
		},
		{
			name: "shift alongside another modifier still shifts the character",
			spec: "cmd+shift+a",
			want: []KeyStroke{{Key: "A", Code: "KeyA", KeyCode: 65, Text: "A", Modifiers: modMeta | modShift}},
		},
		{
			name: "sequence",
			spec: "End shift+Home Backspace",
			want: []KeyStroke{
				{Key: "End", Code: "End", KeyCode: 35},
				{Key: "Home", Code: "Home", KeyCode: 36, Modifiers: modShift},
				{Key: "Backspace", Code: "Backspace", KeyCode: 8},
			},
		},
		{
			name: "surrounding and repeated whitespace is not significant",
			spec: "  Escape   Tab  ",
			want: []KeyStroke{
				{Key: "Escape", Code: "Escape", KeyCode: 27},
				{Key: "Tab", Code: "Tab", KeyCode: 9},
			},
		},
		{
			name: "plus is pressable as a bare key",
			spec: "+",
			want: []KeyStroke{{Key: "+", Code: "Equal", KeyCode: 187, Text: "+", Modifiers: modShift}},
		},
		{
			name: "plus is pressable as a chord key",
			spec: "cmd++",
			want: []KeyStroke{{Key: "+", Code: "Equal", KeyCode: 187, Text: "+", Modifiers: modMeta | modShift}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseKeys(c.spec)
			if err != nil {
				t.Fatalf("ParseKeys(%q) = error %v, want %v", c.spec, err, c.want)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ParseKeys(%q) =\n %+v\nwant\n %+v", c.spec, got, c.want)
			}
		})
	}
}

// Rejections are the load-bearing half: a keyspec that does not parse must be a
// usage error, and in particular an unknown multi-character token must never
// degrade into typing that word out letter by letter.
func TestParseKeysRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"empty":                        "",
		"whitespace only":              "   ",
		"misspelled key name":          "Ecsape",
		"bare multi-char word":         "hello",
		"unknown modifier":             "hyper+a",
		"two non-modifier keys":        "cmd+a+b",
		"trailing plus":                "ctrl+",
		"trailing plus with no key":    "shift+Home ctrl+",
		"modifier with no key":         "cmd",
		"leading plus is an empty mod": "+a",
		"a later token is invalid":     "Escape Ecsape",
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseKeys(spec)
			if err == nil {
				t.Fatalf("ParseKeys(%q) = %+v, want an error", spec, got)
			}
			if got != nil {
				t.Errorf("ParseKeys(%q) returned %+v alongside its error; a failed parse must yield no strokes", spec, got)
			}
		})
	}
}

// A parsed sequence formats back into a keyspec that parses to the identical
// strokes — so the `keys` field of the result envelope is a valid input to the
// verb that produced it, not just a display string.
func TestParseKeysRoundTrip(t *testing.T) {
	t.Parallel()
	specs := []string{
		"Escape",
		"a",
		"A",
		"/",
		"+",
		"Space",
		"cmd+a",
		"ctrl+shift+k",
		"shift+ctrl+k",
		"shift+a",
		"shift+1",
		"shift+Home",
		"CMD+ALT+Escape",
		"escape",
		"End shift+Home Backspace",
		"F7 ArrowDown PageUp",
		"alt+ArrowLeft",
	}
	for _, spec := range specs {
		t.Run(spec, func(t *testing.T) {
			t.Parallel()
			first, err := ParseKeys(spec)
			if err != nil {
				t.Fatalf("ParseKeys(%q): %v", spec, err)
			}
			formatted := FormatKeys(first)
			second, err := ParseKeys(formatted)
			if err != nil {
				t.Fatalf("ParseKeys(FormatKeys(%q)) = ParseKeys(%q): %v", spec, formatted, err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Errorf("round-trip changed the strokes: %q -> %q\n first: %+v\nsecond: %+v", spec, formatted, first, second)
			}
			if again := FormatKeys(second); again != formatted {
				t.Errorf("FormatKeys is not idempotent: %q -> %q -> %q", spec, formatted, again)
			}
		})
	}
}

// `shift+a` and `A` are two spellings of the same physical press, so they must
// parse to the IDENTICAL stroke — key, code, virtual keycode, inserted text and
// modifier mask alike. Anything less means one of the two spellings dispatches a
// press no keyboard can produce.
func TestShiftChordEqualsTheShiftedCharacter(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ chord, shifted string }{
		{"shift+a", "A"},
		{"shift+1", "!"},
		{"shift+/", "?"},
		{"shift+=", "+"},
	} {
		t.Run(c.chord, func(t *testing.T) {
			t.Parallel()
			chord, err := ParseKeys(c.chord)
			if err != nil {
				t.Fatalf("ParseKeys(%q): %v", c.chord, err)
			}
			shifted, err := ParseKeys(c.shifted)
			if err != nil {
				t.Fatalf("ParseKeys(%q): %v", c.shifted, err)
			}
			if !reflect.DeepEqual(chord, shifted) {
				t.Errorf("ParseKeys(%q) = %+v, want it identical to ParseKeys(%q) = %+v",
					c.chord, chord, c.shifted, shifted)
			}
		})
	}
}

func TestParseModifiers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		spec string
		want int64
	}{
		{"", 0},
		{"ctrl", modCtrl},
		{"shift", modShift},
		{"alt", modAlt},
		{"cmd", modMeta},
		{"meta", modMeta},
		{"super", modMeta},
		{"CMD", modMeta},
		{"cmd+shift", modMeta | modShift},
		{"ctrl+alt+shift+cmd", modCtrl | modAlt | modShift | modMeta},
		{"cmd+cmd", modMeta},
		{" cmd + shift ", modMeta | modShift},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%q", c.spec), func(t *testing.T) {
			t.Parallel()
			got, err := ParseModifiers(c.spec)
			if err != nil {
				t.Fatalf("ParseModifiers(%q): %v", c.spec, err)
			}
			if got != c.want {
				t.Errorf("ParseModifiers(%q) = %d, want %d", c.spec, got, c.want)
			}
		})
	}
}

func TestParseModifiersRejects(t *testing.T) {
	t.Parallel()
	for _, spec := range []string{"hyper", "ctrl+hyper", "a", "cmd+", "+"} {
		t.Run(spec, func(t *testing.T) {
			t.Parallel()
			if got, err := ParseModifiers(spec); err == nil {
				t.Errorf("ParseModifiers(%q) = %d, want an error", spec, got)
			}
		})
	}
}

// keyLogPage records every keydown into window.__log so a live test can assert
// on what the renderer actually saw — key, code, keyCode, and each modifier flag.
const keyLogPage = `<!doctype html><title>Keys</title><body>
<input id="q" aria-label="Field">
<script>
window.__log = [];
document.addEventListener('keydown', function (e) {
  window.__log.push({key: e.key, code: e.code, keyCode: e.keyCode,
    ctrl: e.ctrlKey, alt: e.altKey, shift: e.shiftKey, meta: e.metaKey});
});
</script>
</body>`

type keyEventLog struct {
	Key     string `json:"key"`
	Code    string `json:"code"`
	KeyCode int    `json:"keyCode"`
	Ctrl    bool   `json:"ctrl"`
	Alt     bool   `json:"alt"`
	Shift   bool   `json:"shift"`
	Meta    bool   `json:"meta"`
}

// TestKeyLive drives a real renderer, because the only question that matters for
// this verb — did the page receive the press, with the modifiers set — cannot be
// answered by a stub. It is one sequential test: the steps share a focused field
// on purpose, since "the keys go wherever focus already is" is itself a
// requirement.
func TestKeyLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, keyLogPage)
	}))
	t.Cleanup(srv.Close)

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	press := func(t *testing.T, selector, spec string, opts KeyOpts) map[string]any {
		t.Helper()
		keys, err := ParseKeys(spec)
		if err != nil {
			t.Fatalf("ParseKeys(%q): %v", spec, err)
		}
		res, err := b.Key(ctx, id, selector, keys, opts)
		if err != nil {
			t.Fatalf("Key(%q, %q): %v", selector, spec, err)
		}
		return res
	}
	fieldValue := func(t *testing.T) string {
		t.Helper()
		got, err := b.Eval(ctx, id, "document.getElementById('q').value")
		if err != nil {
			t.Fatalf("Eval value: %v", err)
		}
		v, _ := got.(map[string]any)["value"].(string)
		return v
	}
	resetLog := func(t *testing.T) {
		t.Helper()
		if _, err := b.Eval(ctx, id, "window.__log = []"); err != nil {
			t.Fatalf("reset log: %v", err)
		}
	}
	readLog := func(t *testing.T) []keyEventLog {
		t.Helper()
		got, err := b.Eval(ctx, id, "JSON.stringify(window.__log)")
		if err != nil {
			t.Fatalf("Eval log: %v", err)
		}
		raw, _ := got.(map[string]any)["value"].(string)
		var log []keyEventLog
		if err := json.Unmarshal([]byte(raw), &log); err != nil {
			t.Fatalf("log is not JSON (%q): %v", raw, err)
		}
		return log
	}

	// A selector focuses the field first, and a printable key inserts its text.
	t.Run("printable key with a selector", func(t *testing.T) {
		if _, err := b.Eval(ctx, id, "document.getElementById('q').value = ''"); err != nil {
			t.Fatalf("clear field: %v", err)
		}
		resetLog(t)
		res := press(t, "#q", "a", KeyOpts{})
		if got := fieldValue(t); got != "a" {
			t.Errorf("field = %q, want %q — a printable key must set Text so the character is inserted", got, "a")
		}
		if keys, _ := res["keys"].([]string); len(keys) != 1 || keys[0] != "a" {
			t.Errorf("result keys = %v, want [a]", res["keys"])
		}
		if res["repeat"] != 1 {
			t.Errorf("result repeat = %v, want 1", res["repeat"])
		}
		// focused is best-effort, but on a foreground tab with a just-focused
		// field it should be there and name the textbox.
		f, ok := res["focused"].(string)
		if !ok {
			t.Errorf("result has no focused field: %v", res)
		} else if !strings.Contains(f, "textbox") || !strings.Contains(f, "Field") {
			t.Errorf("focused = %q, want it to describe the focused textbox", f)
		}
		if log := readLog(t); len(log) != 1 || log[0].Key != "a" || log[0].Code != "KeyA" {
			t.Errorf("keydown log = %+v, want one {key:a, code:KeyA} — a page reading event.code must still see the press", log)
		}
	})

	// An empty selector does no element resolution and goes to whatever is
	// already focused — the field from the previous step.
	t.Run("empty selector goes to current focus", func(t *testing.T) {
		resetLog(t)
		press(t, "", "b c", KeyOpts{})
		if got := fieldValue(t); got != "abc" {
			t.Errorf("field = %q, want %q — a sequence must press left to right into the focused element", got, "abc")
		}
		log := readLog(t)
		if len(log) != 2 || log[0].Key != "b" || log[1].Key != "c" {
			t.Errorf("keydown log = %+v, want b then c", log)
		}
	})

	// A chord sets the bitmask on the press and inserts nothing: cmd+a is
	// select-all, not "type an a".
	t.Run("chord sets the modifier flags and suppresses text", func(t *testing.T) {
		resetLog(t)
		press(t, "", "cmd+a", KeyOpts{})
		if got := fieldValue(t); got != "abc" {
			t.Errorf("field = %q, want it unchanged at %q — a modified key must not also insert its character", got, "abc")
		}
		log := readLog(t)
		var chord *keyEventLog
		for i := range log {
			if log[i].Key == "a" {
				chord = &log[i]
			}
		}
		if chord == nil {
			t.Fatalf("keydown log = %+v, want a keydown for the chord's key", log)
		}
		if !chord.Meta {
			t.Errorf("chord keydown = %+v, want metaKey true — pages branch on event.metaKey", *chord)
		}
		if chord.Ctrl {
			t.Errorf("chord keydown = %+v, want ctrlKey false — cmd is Meta on every platform", *chord)
		}
		// The Meta press itself must also carry the mask.
		for _, e := range log {
			if e.Key == "Meta" && !e.Meta {
				t.Errorf("Meta keydown = %+v, want metaKey true on the modifier's own event", e)
			}
		}
	})

	// Named keys drive real editing: End, then shift+Home to select, then
	// Backspace to delete the selection.
	t.Run("named keys and a shift chord edit the field", func(t *testing.T) {
		resetLog(t)
		press(t, "", "End shift+Home Backspace", KeyOpts{})
		if got := fieldValue(t); got != "" {
			t.Errorf("field = %q, want it emptied by End shift+Home Backspace", got)
		}
		log := readLog(t)
		// The Shift press is itself a keydown the page sees, which is the point:
		// filter the modifier out to check the sequence, then check it was there.
		var keys []keyEventLog
		sawShiftPress := false
		for _, e := range log {
			if e.Key == "Shift" {
				sawShiftPress = true
				continue
			}
			keys = append(keys, e)
		}
		if len(keys) != 3 {
			t.Fatalf("keydown log = %+v, want 3 non-modifier presses", log)
		}
		if keys[0].Key != "End" || keys[1].Key != "Home" || keys[2].Key != "Backspace" {
			t.Errorf("keydown log = %+v, want End, Home, Backspace", keys)
		}
		if !keys[1].Shift {
			t.Errorf("Home keydown = %+v, want shiftKey true", keys[1])
		}
		if keys[0].Shift || keys[2].Shift {
			t.Errorf("keydown log = %+v, want Shift held only for the chord", keys)
		}
		if !sawShiftPress {
			t.Error("no keydown for Shift itself; a chord must press the modifier, not just set the bitmask")
		}
	})

	// --repeat presses the whole sequence n times.
	t.Run("repeat", func(t *testing.T) {
		resetLog(t)
		res := press(t, "", "ArrowDown", KeyOpts{Repeat: 3, Delay: 10 * time.Millisecond})
		if res["repeat"] != 3 {
			t.Errorf("result repeat = %v, want 3", res["repeat"])
		}
		n := 0
		for _, e := range readLog(t) {
			if e.Key == "ArrowDown" {
				n++
			}
		}
		if n != 3 {
			t.Errorf("ArrowDown presses = %d, want 3", n)
		}
	})

	// An unresolvable selector is an error, not a silent press into the void.
	t.Run("unresolvable selector fails", func(t *testing.T) {
		keys, err := ParseKeys("Escape")
		if err != nil {
			t.Fatalf("ParseKeys: %v", err)
		}
		bctx, bcancel := context.WithTimeout(ctx, 3*time.Second)
		defer bcancel()
		if _, err := b.Key(bctx, id, "#nope", keys, KeyOpts{}); err == nil {
			t.Error("Key on a selector that matches nothing returned nil error")
		}
	})
}
