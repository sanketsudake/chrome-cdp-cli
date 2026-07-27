package mcp

// The tool surface, and the reason it is shaped the way it is.
//
// An agent pays for every tool in its context window, so the default set is
// BOUNDED (RFC-0004 US-4: ≤ 18) and verbs are grouped behind a discriminator
// argument rather than each getting a tool of its own: `read` takes a `kind`,
// `tabs` and `pointer` take an `action`, `type_text` takes `replace`. Grouping
// costs one enum in the schema and saves eleven tool descriptions.
//
// The descriptions are part of the product. This CLI's advantage on real
// applications is that it addresses elements by ARIA accessible name from the
// page's own accessibility tree, which survives the generated class names that
// break CSS selectors on Workday, Salesforce and the like — and an agent only
// gets that advantage if the schema says so. Hence `addressingHelp` on every
// element-addressing tool.

import "strings"

// Tool names are prefixed so they stay unambiguous in clients that flatten
// every server's tools into one namespace (RFC-0004 open question 3).
const prefix = "chrome_cdp_"

// addressingHelp is repeated on every tool that resolves an element. It is
// deliberately concrete: "prefer accessible names" without saying how reads as
// advice, and gets ignored.
const addressingHelp = "\n\nAddressing: `selector` is a CSS selector unless `by` says otherwise. " +
	"On dynamic applications (Workday, Salesforce, Outlook, most SPAs) prefer `by: \"name\"` and pass the element's " +
	"ARIA accessible name — the label a person reads on screen — because generated CSS class names change between renders. " +
	"`role` narrows a name match to one ARIA role (button|link|textbox|checkbox|…), `nth` picks the Nth visible match, " +
	"`match` selects exact (default)|contains|regex, and `in_row` scopes the match to the table row whose text contains " +
	"that string — the way to press Delete in one row of many. `by: \"ref\"` takes an e<id> handle straight from a " +
	"snapshot, which is the cheapest and most stable way to act on something you have just read, and `by: \"cell\"` " +
	"addresses a grid input by its column header."

// byEnum, waitEnum and matchEnum mirror the CLI's own vocabularies. They are
// declared as schema enums so a wrong value is refused as `usage` before the
// browser is contacted (RFC-0004 VS-5), rather than becoming a selector that
// resolves to nothing thirty seconds later.
var (
	byEnum    = []string{"css", "id", "search", "jspath", "css-all", "name", "ref", "cell", "label"}
	waitEnum  = []string{"visible", "ready", "enabled"}
	matchEnum = []string{"exact", "contains", "regex"}
)

// targetArgs are the two arguments every tool takes: which tab, and how long to
// wait. `target` is omitted when the server was started with --target, which
// pins one tab for the whole session.
func targetArgs() []arg {
	return []arg{
		{name: "target", typ: "string", flag: "target",
			desc: "tab to act on: an id prefix, url:<substring>, title:<substring>, or @N from the tabs list. Defaults to the sticky current tab set by tabs(action=\"use\")."},
		{name: "timeout", typ: "string", flag: "timeout",
			desc: "max time for this call as a Go duration (e.g. \"45s\"); default 30s."},
	}
}

// queryArgs are the shared element-addressing options. Every verb that resolves
// an element takes all of them (QueryOpts in the CLI), so they are declared in
// one place and a verb that invented its own would be a design error.
func queryArgs() []arg {
	return []arg{
		{name: "by", typ: "string", flag: "by", enum: byEnum,
			desc: "selector syntax: css (default) | name (ARIA accessible name — prefer this on dynamic apps) | ref (e<id> from a snapshot) | cell (grid input by [row|]column header) | label (form control by visible label) | id | search | jspath | css-all."},
		{name: "role", typ: "string", flag: "role",
			desc: "with by=\"name\": constrain the match to this ARIA role (button|link|textbox|…). The way to tell a column header from the input under it."},
		{name: "nth", typ: "integer", flag: "nth",
			desc: "with by=\"name\": pick the Nth (1-based) match among visible candidates."},
		{name: "match", typ: "string", flag: "match", enum: matchEnum,
			desc: "with by=\"name\": name match mode — exact (default), contains, or regex."},
		{name: "in_row", typ: "string", flag: "in-row",
			desc: "with by=\"name\": scope the match to the table row whose text contains this string."},
		{name: "wait", typ: "string", flag: "wait", enum: waitEnum,
			desc: "selector wait condition before acting: visible (default) | ready | enabled."},
		{name: "no_wait", typ: "boolean", flag: "no-wait",
			desc: "act immediately and fail fast instead of waiting for the element."},
		{name: "pierce", typ: "boolean", flag: "pierce",
			desc: "reach into shadow DOM and iframes (via DevTools search)."},
	}
}

// actArgs are the two options every mutating verb shares: confirm the write
// landed, and handle a native dialog the action opens.
func actArgs() []arg {
	return []arg{
		{name: "wait_text", typ: "string", flag: "wait-text",
			desc: "after the action succeeds, wait until the page contains this text (e.g. a \"Saved\" toast). Folds act-then-confirm into one call."},
		{name: "on_dialog", typ: "string", flag: "on-dialog", enum: []string{"accept", "dismiss"},
			desc: "auto-handle a native alert/confirm/prompt opened by the action."},
	}
}

func concat(sets ...[]arg) []arg {
	var out []arg
	for _, s := range sets {
		out = append(out, s...)
	}
	return out
}

// registry builds the whole tool surface, in listing order.
func registry() []*tool {
	return []*tool{
		tabsTool(),
		navigateTool(),
		snapshotTool(),
		readTool(),
		clickTool(),
		typeTextTool(),
		keyTool(),
		pointerTool(),
		selectOptionTool(),
		scrollTool(),
		uploadTool(),
		waitForTool(),
		screenshotTool(),
		consoleTool(),
		networkTool(),
		evaluateTool(),
		batchTool(),
		rawCDPTool(),
	}
}

func tabsTool() *tool {
	return &tool{
		name:  prefix + "tabs",
		title: "Tabs",
		desc: "Tab lifecycle in the user's real Chrome: list open tabs, open a new one, set the sticky current tab, close tabs, or foreground one.\n\n" +
			"Start here. `action: \"list\"` gives every tab with an `idx` (@N), id, title and URL; `action: \"use\"` then pins one as the current tab so later calls need no `target`. " +
			"`action: \"activate\"` foregrounds a tab, which is the fix when a read reports `tab_hidden`: Chrome throttles the accessibility tree on a backgrounded tab, so name/ref/cell addressing stalls there.",
		disc: "action",
		actions: map[string]string{
			"list": "list", "open": "open", "use": "use", "close": "close", "activate": "activate",
		},
		verbs: []string{"list", "open", "use", "close", "activate"},
		args: concat([]arg{
			{name: "action", typ: "string", required: true, enum: []string{"list", "open", "use", "close", "activate"},
				desc: "list: every open tab. open: open `url` in a new tab and make it current. use: make `target` the sticky current tab. close: close `target` (or every tab matching `url`/`title` with `all`). activate: foreground `target`."},
			{name: "url", typ: "string", flag: "url",
				desc: "with action=\"open\": the URL to open. With action=\"list\"/\"close\": only tabs whose URL contains this substring."},
			{name: "title", typ: "string", flag: "title",
				desc: "with action=\"list\"/\"close\": only tabs whose title contains this substring."},
			{name: "all", typ: "boolean", flag: "all",
				desc: "with action=\"close\": close every matching tab (required when more than one matches)."},
		}, targetArgs()),
		build: func(c *call) (string, []string, error) {
			action := c.str("action")
			if err := c.only(action, map[string][]string{
				"all": {"close"}, "title": {"list", "close"},
			}); err != nil {
				return "", nil, err
			}
			switch action {
			case "list":
				return "list", append([]string{"list"}, c.flags("action", "target")...), nil
			case "open":
				if !c.has("url") {
					return "", nil, usagef("tabs action=\"open\" needs `url`")
				}
				return "open", append([]string{"open", c.str("url")}, c.flags("action", "url", "target", "title", "all")...), nil
			case "use":
				if !c.has("target") {
					return "", nil, usagef("tabs action=\"use\" needs `target` (an id prefix, url:<s>, title:<s>, or @N)")
				}
				return "use", append([]string{"use", c.str("target")}, c.flags("action", "target", "url", "title", "all")...), nil
			case "close", "activate":
				argv := []string{action}
				if c.has("target") {
					argv = append(argv, c.str("target"))
				}
				return action, append(argv, c.flags("action", "target")...), nil
			}
			return "", nil, usagef("unknown tabs action %q", action)
		},
	}
}

func navigateTool() *tool {
	return &tool{
		name:  prefix + "navigate",
		title: "Navigate",
		desc: "Navigate the target tab to a URL and wait for load, or move through its history.\n\n" +
			"Exactly one of `url`, `back`, `forward` or `reload`. The result reports the settled URL and HTTP status, so a redirect to a login page is visible in the result rather than a surprise on the next call.",
		verbs: []string{"nav"},
		args: concat([]arg{
			{name: "url", typ: "string", pos: true, desc: "destination URL. Under a policy allow-list the DESTINATION is checked before Chrome is contacted."},
			{name: "back", typ: "boolean", flag: "back", desc: "go back one history entry instead of navigating."},
			{name: "forward", typ: "boolean", flag: "forward", desc: "go forward one history entry."},
			{name: "reload", typ: "boolean", flag: "reload", desc: "reload the current page."},
			{name: "hard", typ: "boolean", flag: "hard", desc: "with reload: bypass the cache."},
		}, actArgs()[:1], targetArgs()),
		build: func(c *call) (string, []string, error) {
			argv := []string{"nav"}
			if c.has("url") {
				argv = append(argv, c.str("url"))
			}
			return "nav", append(argv, c.flags("url")...), nil
		},
	}
}

func snapshotTool() *tool {
	return &tool{
		name:  prefix + "snapshot",
		title: "Snapshot the page",
		desc: "Read the page as an accessibility-tree snapshot: the roles, accessible names, values and states of everything interactive, each with an `e<id>` ref.\n\n" +
			"This is the primary read and usually the right first call on a page you have not seen. It returns what a screen reader would see, not raw HTML, so it is far smaller than the DOM and its names are exactly what `by: \"name\"` matches. " +
			"Act on what you read with `by: \"ref\"` and the `e<id>` from the snapshot. Filter a large app with `role` (only buttons, say), `grep` (a regex over names), or `region` (only inside the container whose name contains this); `dedupe` collapses identical role+name pairs in a virtualized grid.",
		verbs: []string{"snap"},
		args: concat([]arg{
			{name: "role", typ: "string", flag: "role", desc: "only nodes with this ARIA role (button|link|textbox|…)."},
			{name: "grep", typ: "string", flag: "grep", desc: "only nodes whose accessible name matches this regex."},
			{name: "region", typ: "string", flag: "region", desc: "only nodes inside a container whose accessible name contains this."},
			{name: "dedupe", typ: "boolean", flag: "dedupe", desc: "collapse identical role+name (for virtualized grids)."},
		}, targetArgs()),
		build: func(c *call) (string, []string, error) {
			return "snap", append([]string{"snap"}, c.flags()...), nil
		},
	}
}

func readTool() *tool {
	return &tool{
		name:  prefix + "read",
		title: "Read page content",
		desc: "Read content out of the page: `kind: \"text\"` (visible text, or the main article with `article`), \"html\" (markup), \"value\" (a form field's current value), or \"grid\" (a table or grid as structured {headers, rows}).\n\n" +
			"Prefer \"text\" with `article: true` for reading a page, \"grid\" for anything tabular, and \"value\" to confirm a field holds what you typed. Use snapshot instead when you want to know what is clickable." + addressingHelp,
		disc:    "kind",
		actions: map[string]string{"text": "text", "html": "html", "value": "value", "grid": "grid"},
		verbs:   []string{"text", "html", "value", "grid"},
		args: concat([]arg{
			{name: "kind", typ: "string", required: true, enum: []string{"text", "html", "value", "grid"},
				desc: "what to read: text | html | value | grid."},
			{name: "selector", typ: "string", pos: true,
				desc: "element to read; omit for the whole page (required for kind=\"value\")."},
			{name: "article", typ: "boolean", flag: "article", desc: "kind=\"text\": extract the main readable content, dropping navigation and boilerplate."},
			{name: "markdown", typ: "boolean", flag: "markdown", desc: "kind=\"text\" with article: keep headings, lists, links and code blocks as markdown."},
			{name: "min_chars", typ: "integer", flag: "min-chars", desc: "kind=\"text\" with article: below this many extracted characters, report extracted:false and return the full text."},
			{name: "inner", typ: "boolean", flag: "inner", desc: "kind=\"html\": inner HTML instead of outer."},
			{name: "all", typ: "boolean", flag: "all", desc: "kind=\"value\": the value of every match, as a list."},
		}, queryArgs(), targetArgs()),
		build: func(c *call) (string, []string, error) {
			kind := c.str("kind")
			if err := c.only(kind, map[string][]string{
				"article": {"text"}, "markdown": {"text"}, "min_chars": {"text"},
				"inner": {"html"}, "all": {"value"},
			}); err != nil {
				return "", nil, err
			}
			if kind == "value" && !c.has("selector") {
				return "", nil, usagef("read kind=\"value\" needs `selector`")
			}
			argv := []string{kind}
			if c.has("selector") {
				argv = append(argv, c.str("selector"))
			}
			return kind, append(argv, c.flags("kind", "selector")...), nil
		},
	}
}

func clickTool() *tool {
	return &tool{
		name:  prefix + "click",
		title: "Click",
		desc: "Click an element with a real mouse press at its resolved centre, after waiting for it to be visible and unoccluded.\n\n" +
			"A click that never lands because an overlay covers the target is reported as `occluded` rather than a bare timeout, so dismiss the overlay instead of re-checking the selector. `modifiers` holds keys during the click (\"cmd\" to multi-select, \"shift\" to extend a range)." + addressingHelp,
		verbs: []string{"click"},
		args: concat([]arg{
			{name: "selector", typ: "string", pos: true, required: true, desc: "the element to click (see Addressing)."},
			{name: "modifiers", typ: "string", flag: "modifiers", desc: "modifier keys held during the click, +-joined: ctrl+shift+alt+cmd."},
		}, actArgs(), queryArgs(), targetArgs()),
		build: func(c *call) (string, []string, error) {
			return "click", append([]string{"click", c.str("selector")}, c.flags("selector")...), nil
		},
	}
}

func typeTextTool() *tool {
	return &tool{
		name:  prefix + "type_text",
		title: "Type text",
		desc: "Type text into a field with real keystrokes, so the app's own input handlers, validation and autocomplete all fire.\n\n" +
			"`replace: true` clears the field first (the CLI's `fill`) and is what you want for a field that already holds a value; the default appends. For keys that are not literal text — Escape, Tab, cmd+a — use the `key` tool, and for a prompt or combobox that needs an option chosen, use `select_option`." + addressingHelp,
		verbs: []string{"type", "fill"},
		args: concat([]arg{
			{name: "selector", typ: "string", pos: true, required: true, desc: "the field to type into (see Addressing)."},
			{name: "text", typ: "string", pos: true, required: true, desc: "the literal text to type."},
			{name: "replace", typ: "boolean", desc: "clear the field and set it to `text` (fill) instead of appending."},
		}, actArgs(), queryArgs(), targetArgs()),
		build: func(c *call) (string, []string, error) {
			verb := "type"
			if c.bool("replace") {
				verb = "fill"
			}
			return verb, append([]string{verb, c.str("selector"), c.str("text")}, c.flags("selector", "text", "replace")...), nil
		},
	}
}

func keyTool() *tool {
	return &tool{
		name:  prefix + "key",
		title: "Press keys",
		desc: "Press named keys and modifier chords: Escape, Enter, Tab, cmd+a, \"End shift+Home Backspace\".\n\n" +
			"With no `selector` the keys go to whatever the page has focused, which is how `keys: \"Escape\"` closes a modal that has nothing addressable to aim at. Use `type_text` for literal text — an unknown multi-character key name is an error here, never typed out letter by letter." + addressingHelp,
		verbs: []string{"key"},
		args: concat([]arg{
			{name: "keys", typ: "string", pos: true, required: true,
				desc: "the key sequence: a named key (Escape, Enter, Tab, Space, Home, PageDown, ArrowUp, F1…F24), a single printable character, or a chord (ctrl/shift/alt/cmd, cmd is Meta everywhere). Space-separate a sequence."},
			{name: "selector", typ: "string", pos: true, desc: "focus this element first; omit to send to the focused element."},
			{name: "repeat", typ: "integer", flag: "repeat", desc: "press the sequence this many times (1..100)."},
			{name: "delay", typ: "string", flag: "delay", desc: "pause between repeats as a Go duration (e.g. \"100ms\"), for apps that debounce input."},
		}, actArgs()[:1], queryArgs(), targetArgs()),
		build: func(c *call) (string, []string, error) {
			// The keyspec is always the LAST argument, with the optional
			// selector before it — the CLI's own contract.
			argv := []string{"key"}
			if c.has("selector") {
				argv = append(argv, c.str("selector"))
			}
			argv = append(argv, c.str("keys"))
			return "key", append(argv, c.flags("keys", "selector")...), nil
		},
	}
}

func pointerTool() *tool {
	return &tool{
		name:  prefix + "pointer",
		title: "Pointer gestures",
		desc: "Pointer gestures other than a plain click: hover (reveal a menu or tooltip), dblclick, rclick (open a context menu), and drag.\n\n" +
			"A drag needs a destination: either `to` (a drop-target selector) or `dx`/`dy` (a pixel offset from the source centre). `hold` waits after the press before moving, which is what long-press-to-drag UIs require." + addressingHelp,
		disc:    "action",
		actions: map[string]string{"hover": "hover", "dblclick": "dblclick", "rclick": "rclick", "drag": "drag"},
		verbs:   []string{"hover", "dblclick", "rclick", "drag"},
		args: concat([]arg{
			{name: "action", typ: "string", required: true, enum: []string{"hover", "dblclick", "rclick", "drag"},
				desc: "which gesture to dispatch."},
			{name: "selector", typ: "string", pos: true, required: true, desc: "the element to act on (the drag SOURCE for action=\"drag\")."},
			{name: "modifiers", typ: "string", flag: "modifiers", desc: "modifier keys held during the gesture, +-joined: ctrl+shift+alt+cmd."},
			{name: "hold", typ: "string", flag: "hold", desc: "hover: keep the pointer in place this long (slow tooltips). drag: pause after the press before moving. Go duration, e.g. \"500ms\"."},
			{name: "to", typ: "string", flag: "to", desc: "action=\"drag\": drop-target selector (mutually exclusive with dx/dy)."},
			{name: "to_by", typ: "string", flag: "to-by", enum: byEnum, desc: "action=\"drag\": `by` mode for the drop target (defaults to `by`)."},
			{name: "dx", typ: "number", flag: "dx", desc: "action=\"drag\": horizontal distance in pixels from the source centre."},
			{name: "dy", typ: "number", flag: "dy", desc: "action=\"drag\": vertical distance in pixels from the source centre."},
			{name: "steps", typ: "integer", flag: "steps", desc: "action=\"drag\": interpolated move events between press and release."},
		}, actArgs(), queryArgs(), targetArgs()),
		build: func(c *call) (string, []string, error) {
			action := c.str("action")
			if err := c.only(action, map[string][]string{
				"hold": {"hover", "drag"},
				"to":   {"drag"}, "to_by": {"drag"}, "dx": {"drag"}, "dy": {"drag"}, "steps": {"drag"},
			}); err != nil {
				return "", nil, err
			}
			return action, append([]string{action, c.str("selector")}, c.flags("action", "selector")...), nil
		},
	}
}

func selectOptionTool() *tool {
	return &tool{
		name:  prefix + "select_option",
		title: "Select an option",
		desc: "Choose an option in a prompt, combobox, cascade widget or native <select> — the control that a plain click-then-click cannot drive reliably.\n\n" +
			"The field is addressed by ARIA accessible name by default (add `role: \"textbox\"` to tell an input from a same-named column header). The option is matched by substring unless `option_match` says otherwise, and a cascade path is written with \" > \" between levels, e.g. option: \"Project Plan Tasks > Migration\". `filter` types into the prompt first to narrow a long list." + addressingHelp,
		verbs: []string{"select"},
		args: concat([]arg{
			{name: "field", typ: "string", pos: true, required: true, desc: "the prompt/combobox/select to drive, by accessible name."},
			{name: "option", typ: "string", pos: true, required: true, desc: "the option to choose; use \">\" between levels for a cascade."},
			{name: "filter", typ: "string", flag: "filter", desc: "type this into the prompt to narrow the options before selecting."},
			{name: "option_match", typ: "string", flag: "option-match", enum: []string{"contains", "exact", "regex"}, desc: "option match mode; contains by default."},
			{name: "sep", typ: "string", flag: "sep", desc: "cascade path separator between option levels (default \">\")."},
		}, actArgs(), queryArgs(), targetArgs()),
		build: func(c *call) (string, []string, error) {
			return "select", append([]string{"select", c.str("field"), c.str("option")}, c.flags("field", "option")...), nil
		},
	}
}

func scrollTool() *tool {
	return &tool{
		name:  prefix + "scroll",
		title: "Scroll",
		desc: "Scroll the window, an element's own scroll box, or a selector into view.\n\n" +
			"`to: true` scrolls the selector into view; otherwise `dx`/`dy` scroll by a pixel delta (positive `dy` scrolls down). `wheel: true` dispatches a real mouse wheel, which is what virtualized grids that render on wheel — rather than on scroll — need to load more rows." + addressingHelp,
		verbs: []string{"scroll"},
		args: concat([]arg{
			{name: "selector", typ: "string", pos: true, desc: "the scrollable element; omit for the window."},
			{name: "dx", typ: "number", flag: "dx", desc: "horizontal scroll delta in pixels."},
			{name: "dy", typ: "number", flag: "dy", desc: "vertical scroll delta in pixels (positive scrolls down)."},
			{name: "to", typ: "boolean", flag: "to", desc: "scroll the selector into view instead of by a delta."},
			{name: "wheel", typ: "boolean", flag: "wheel", desc: "dispatch a real mouse wheel (for grids that render on wheel, not scroll)."},
		}, queryArgs(), targetArgs()),
		build: func(c *call) (string, []string, error) {
			argv := []string{"scroll"}
			if c.has("selector") {
				argv = append(argv, c.str("selector"))
			}
			return "scroll", append(argv, c.flags("selector")...), nil
		},
	}
}

func uploadTool() *tool {
	return &tool{
		name:  prefix + "upload",
		title: "Upload files",
		desc: "Attach local files to a file input, without a native file dialog.\n\n" +
			"Paths must be absolute, existing and readable, and are checked before Chrome is contacted; under a configured policy they must also sit under a configured `upload_roots` directory. The result reports the files read back off the input, so a silent no-op is visible." + addressingHelp,
		verbs: []string{"upload"},
		args: concat([]arg{
			{name: "selector", typ: "string", pos: true, required: true, desc: "the file input (see Addressing)."},
			{name: "paths", typ: "array", items: "string", pos: true, required: true, desc: "absolute paths of the files to attach."},
			{name: "append", typ: "boolean", flag: "append", desc: "add to the files this session set on the input instead of replacing them."},
		}, actArgs()[:1], queryArgs(), targetArgs()),
		build: func(c *call) (string, []string, error) {
			paths, err := c.stringList("paths")
			if err != nil {
				return "", nil, err
			}
			if len(paths) == 0 {
				return "", nil, usagef("upload needs at least one path in `paths`")
			}
			argv := append([]string{"upload", c.str("selector")}, paths...)
			return "upload", append(argv, c.flags("selector", "paths")...), nil
		},
	}
}

func waitForTool() *tool {
	return &tool{
		name:  prefix + "wait_for",
		title: "Wait for a condition",
		desc: "Block until the page reaches a condition: a URL substring, a selector visible or gone, text present, the accessibility tree settled (`stable`), the network idle, or one HTTP request completing (`request`).\n\n" +
			"Prefer a condition over a fixed `for` sleep — a sleep is either flaky or slow. `text` is the usual way to confirm a save landed, `idle` the usual way to wait out an SPA load, and `request` (with `method`/`status`/`failed`) the way to wait for one specific call the page makes." + addressingHelp,
		verbs: []string{"wait"},
		args: concat([]arg{
			{name: "url", typ: "string", flag: "url", desc: "wait until the tab's URL contains this substring."},
			{name: "visible", typ: "string", flag: "visible", desc: "wait until this selector is visible."},
			{name: "gone", typ: "string", flag: "gone", desc: "wait until this selector is gone (a spinner disappearing)."},
			{name: "text", typ: "string", flag: "text", desc: "wait until the page contains this text (e.g. a \"Success\" toast)."},
			{name: "stable", typ: "boolean", flag: "stable", desc: "wait until the accessibility tree stops changing (the page settled)."},
			{name: "idle", typ: "boolean", flag: "idle", desc: "wait until network activity settles (no in-flight requests)."},
			{name: "for", typ: "string", flag: "for", desc: "wait a fixed Go duration (e.g. \"3s\") — a fallback; prefer a condition."},
			{name: "request", typ: "string", flag: "request", desc: "wait until an HTTP request whose URL contains this substring completes (re:<pattern> for a regex)."},
			{name: "method", typ: "array", items: "string", flag: "method", desc: "with request: only these HTTP methods."},
			{name: "status", typ: "string", flag: "status", desc: "with request: only these statuses (200 | 2xx | >=400 | !2xx)."},
			{name: "failed", typ: "boolean", flag: "failed", desc: "with request: only non-2xx responses and network-level failures."},
			{name: "xhr", typ: "boolean", flag: "xhr", desc: "with request: shorthand for type xhr + fetch."},
			{name: "type", typ: "array", items: "string", flag: "type", desc: "with request: only these resource types (document|xhr|fetch|script|stylesheet|image|font|websocket|other)."},
		}, queryArgs(), targetArgs()),
		build: func(c *call) (string, []string, error) {
			return "wait", append([]string{"wait"}, c.flags()...), nil
		},
	}
}

func screenshotTool() *tool {
	return &tool{
		name:  prefix + "screenshot",
		title: "Screenshot",
		desc: "Capture the tab as an image and return it inline: the viewport by default, or one element (`selector`), an explicit `region`, or the whole scrollable page (`full_page`).\n\n" +
			"Reach for snapshot or read first — they are smaller, cheaper and directly actionable. A screenshot is for when layout or a rendered artefact is the question. Pass `output` to also write the file to disk; the image comes back either way." + addressingHelp,
		verbs: []string{"screenshot"},
		image: true,
		args: concat([]arg{
			{name: "selector", typ: "string", flag: "selector", desc: "capture this element's box (see Addressing)."},
			{name: "full_page", typ: "boolean", flag: "full-page", desc: "capture the whole scrollable page, beyond the fold."},
			{name: "region", typ: "string", flag: "region", desc: "capture an explicit page-coordinate rectangle: x,y,w,h."},
			{name: "format", typ: "string", flag: "format", enum: []string{"png", "jpeg", "webp"}, desc: "image format; png by default."},
			{name: "quality", typ: "integer", flag: "quality", desc: "compression quality 0-100 (jpeg/webp only; an error with png)."},
			{name: "scale", typ: "number", flag: "scale", desc: "output scale factor, 0.1-3."},
			{name: "padding", typ: "number", flag: "padding", desc: "expand an element capture by this many pixels."},
			{name: "output", typ: "string", flag: "output", desc: "also write the image to this path; omit to only return it inline."},
		}, queryArgs(), targetArgs()),
		build: func(c *call) (string, []string, error) {
			// `output` is handled by the server: with none given it captures to a
			// temporary file, reads the bytes back for the image block, and
			// removes it, so a client that only wants the picture gets no litter.
			return "screenshot", append([]string{"screenshot"}, c.flags()...), nil
		},
	}
}

func consoleTool() *tool {
	return &tool{
		name:  prefix + "console",
		title: "Console messages",
		desc: "Read the console messages and uncaught exceptions the tab has produced since the connection attached.\n\n" +
			"The first thing to check when an action appeared to work and the page did not change. `only_errors` narrows to errors, `grep` to a regex over the text, `since` to a recent window (e.g. \"30s\"), and `clear` drops the buffer so the next read only shows what a following action caused.",
		verbs: []string{"console"},
		args: concat([]arg{
			{name: "level", typ: "array", items: "string", flag: "level", desc: "only these levels: debug|log|info|warn|error."},
			{name: "only_errors", typ: "boolean", flag: "only-errors", desc: "shorthand for level=[\"error\"] (uncaught exceptions report at error level)."},
			{name: "grep", typ: "string", flag: "grep", desc: "only messages whose text matches this regex."},
			{name: "since", typ: "string", flag: "since", desc: "only messages newer than this Go duration (e.g. \"30s\")."},
			{name: "limit", typ: "integer", flag: "limit", desc: "most recent N matching messages (default 100)."},
			{name: "clear", typ: "boolean", flag: "clear", desc: "drop the buffered messages after reading."},
			{name: "fail_on_match", typ: "boolean", flag: "fail-on-match", desc: "report assertion_failed when at least one message matches (the messages are still returned)."},
		}, targetArgs()),
		build: func(c *call) (string, []string, error) {
			return "console", append([]string{"console"}, c.flags()...), nil
		},
	}
}

func networkTool() *tool {
	return &tool{
		name:  prefix + "network",
		title: "Network requests",
		desc: "Read the HTTP requests the tab has made since the connection attached, with status, timing and optionally headers and bodies.\n\n" +
			"How to tell \"the click did nothing\" from \"the call was made and returned 403\". Credential-shaped headers and URL parameters are redacted unless `no_redact` is set. Use wait_for with `request` to block on a call that has not happened yet.",
		verbs: []string{"net"},
		args: concat([]arg{
			{name: "url", typ: "string", flag: "url", desc: "only requests whose URL contains this substring (re:<pattern> for a regex)."},
			{name: "method", typ: "array", items: "string", flag: "method", desc: "only these HTTP methods."},
			{name: "status", typ: "string", flag: "status", desc: "only these statuses: 200 | 2xx | >=400 | !2xx."},
			{name: "failed", typ: "boolean", flag: "failed", desc: "only non-2xx responses and network-level failures."},
			{name: "xhr", typ: "boolean", flag: "xhr", desc: "shorthand for type xhr + fetch."},
			{name: "type", typ: "array", items: "string", flag: "type", desc: "only these resource types: document|xhr|fetch|script|stylesheet|image|font|websocket|other."},
			{name: "headers", typ: "boolean", flag: "headers", desc: "include request and response headers (sensitive values redacted)."},
			{name: "body", typ: "boolean", flag: "body", desc: "include request and response bodies (size-capped)."},
			{name: "no_redact", typ: "boolean", flag: "no-redact", desc: "do NOT redact credential-shaped headers and URL parameters."},
			{name: "since", typ: "string", flag: "since", desc: "only requests newer than this Go duration."},
			{name: "limit", typ: "integer", flag: "limit", desc: "most recent N matching requests (default 100)."},
			{name: "clear", typ: "boolean", flag: "clear", desc: "drop the buffered requests after reading."},
			{name: "fail_on_match", typ: "boolean", flag: "fail-on-match", desc: "report assertion_failed when at least one request matches."},
		}, targetArgs()),
		build: func(c *call) (string, []string, error) {
			return "net", append([]string{"net"}, c.flags()...), nil
		},
	}
}

func evaluateTool() *tool {
	return &tool{
		name:  prefix + "evaluate",
		title: "Evaluate JavaScript",
		desc: "Evaluate JavaScript in the target tab and return its value.\n\n" +
			"POWERFUL AND UNCONSTRAINED: this runs arbitrary script in a page the user is signed into, so it can read anything on that page, submit forms, and navigate the tab somewhere else entirely — which is why a configured policy commonly denies it. Prefer snapshot, read, click and type_text, which are checked, observable and far easier for a user to audit; reach for this when there is genuinely no verb for what you need. " +
			"`await: true` switches to DevTools console semantics: top-level await resolves and the last expression's value is returned.",
		verbs: []string{"eval"},
		args: concat([]arg{
			{name: "expression", typ: "string", pos: true, required: true, desc: "the JavaScript to evaluate."},
			{name: "await", typ: "boolean", flag: "await", desc: "REPL semantics: top-level await resolves and the last expression's value is returned."},
		}, targetArgs()),
		build: func(c *call) (string, []string, error) {
			return "eval", append([]string{"eval", c.str("expression")}, c.flags("expression")...), nil
		},
	}
}

func batchTool() *tool {
	return &tool{
		name:  prefix + "batch",
		title: "Batch",
		desc: "Run several of these tools in order over one held connection, in a single round trip.\n\n" +
			"Use it for any interaction you already know the shape of — click, wait, fill, click, read — instead of paying a round trip per step. Each step is {\"tool\": \"<name>\", \"arguments\": {…}}, results come back in order, and a step that fails STOPS the batch: the failing index and its typed error are reported, and the remaining steps do not run. Every step is validated and policy-checked exactly as it would be on its own.",
		verbs: []string{"session"},
		args: []arg{
			{name: "steps", typ: "array", items: "object", required: true,
				desc: "the steps to run, in order: [{\"tool\": \"chrome_cdp_click\", \"arguments\": {\"selector\": \"Save\", \"by\": \"name\"}}, …]. A step may not be another batch."},
		},
		build: func(*call) (string, []string, error) {
			// batch never becomes one argv; the server runs its steps through
			// this same registry. Declared for completeness so every tool has a
			// verb for classification.
			return "session", nil, nil
		},
	}
}

func rawCDPTool() *tool {
	return &tool{
		name:  prefix + "raw_cdp",
		title: "Raw CDP call",
		desc: "Call any Chrome DevTools Protocol method directly.\n\n" +
			"The escape hatch, exposed only under --tools full: it is unconstrained, browser-level calls reach every tab at once, and a policy cannot name an origin for one. Use `list: true` to enumerate the connected Chrome's domains.",
		verbs: []string{"raw"},
		full:  true,
		args: concat([]arg{
			{name: "method", typ: "string", pos: true, desc: "the CDP method, e.g. \"Page.getNavigationHistory\"."},
			{name: "params", typ: "object", pos: true, desc: "the method's parameters, as an object."},
			{name: "browser", typ: "boolean", flag: "browser", desc: "run at the browser level (Browser.* / Target.* methods, no tab needed)."},
			{name: "list", typ: "boolean", flag: "list", desc: "list the connected Chrome's CDP domains (Schema.getDomains)."},
		}, targetArgs()),
		build: func(c *call) (string, []string, error) {
			argv := []string{"raw"}
			if c.has("method") {
				argv = append(argv, c.str("method"))
				if c.has("params") {
					b, err := jsonBytes(c.args["params"])
					if err != nil {
						return "", nil, usagef("`params` must be a JSON object: %v", err)
					}
					argv = append(argv, string(b))
				}
			} else if !c.bool("list") {
				return "", nil, usagef("raw_cdp needs `method` (or list: true)")
			}
			return "raw", append(argv, c.flags("method", "params")...), nil
		},
	}
}

// only rejects an argument that belongs to a different value of the tool's
// discriminator. Catching it here names the argument and the action it belongs
// to; letting it through would surface as cobra's "unknown flag" for a verb the
// caller never typed.
func (c *call) only(selected string, belongs map[string][]string) error {
	for name, owners := range belongs {
		if !c.has(name) {
			continue
		}
		if !contains(owners, selected) {
			return usagef("`%s` applies to %s %s, not %q", name, c.tool.disc, quoteList(owners), selected)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func quoteList(list []string) string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, `"`+s+`"`)
	}
	return strings.Join(out, " / ")
}
