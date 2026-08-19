# Widgets

## `select` — prompt / combobox / cascade widgets

Some widgets (Workday's Time Type cascade, portal menus, native `<select>`) can't be opened by a plain `click`/`type` — the popup mounts collapsed and a single synthetic click closes it.
**`select <field> <option>` encapsulates the whole choreography**: resolve the field, open it, walk the cascade, and commit the value — atomically over one connection.

```sh
# Cascade prompt: field by accessible name, option as a `>`-separated path.
chrome-cdp select "Time Type" "Project Plan Tasks > Acme: Widget Platform > Project > Time Entry" --role textbox --json
# A portal menu (button → menu → option) works too:
chrome-cdp select "Actions" "Enter Time by Type" --role button --json
```

- The field is addressed by accessible name by default (`--role textbox` disambiguates an input from a same-named column header; an explicit `--by` overrides).
  Passing a CSS selector without `--by css` fails with a bare `field "<selector>" not found`, which reads like the element is missing rather than like the selector was interpreted as an accessible name.
  A native `<select>` with no accessible name (its label is a separate text node) needs the explicit override, e.g. `chrome-cdp select "[name=QuickAddActivityCategory]" "Direct Revenue" --by css --json`.
- The option is a **`>`-separated cascade path** (`--sep` changes the separator); each segment is matched by **substring** (`--option-match exact|contains|regex`).
  Give every level a real Workday cascade needs — the tree can be several deep, and a segment that resolves to a category rather than a leaf makes `select` **error** (never a false success).
- `--filter "<text>"` types into the prompt to narrow a long list before selecting.
- A native `<select>` is a sub-mode (set by option text).
- Workday's Actions menu anchors inconsistently — `select "Actions" "…"` may return a safe `did not render / settle` (no wrong click); just re-run.

## Coordinates — `--at x,y` (canvas, maps, PDF viewers)

**`--at x,y` on any pointer verb** (`click`/`hover`/`dblclick`/`tripleclick`/`rclick`/`drag`, plus `scroll --wheel --at`) acts at a viewport coordinate with NO element resolution.
This is the only way into a canvas/WebGL surface (drawing tools, maps, charts, PDF viewers): the a11y tree sees one node there, so no selector reaches inside it.
Coordinates are CSS pixels and match a `screenshot --scale 1` capture 1:1, so the loop is: `screenshot --scale 1` → read the pixel → `click --at x,y`.
Outside the viewport is an error (`coordinate_out_of_bounds`, exit 4) with the measured viewport, never a silent clamp — `scroll` first, then act.
The result carries `hit` (what sat under the point), because a coordinate click is deliberately not occlusion-checked.

**`window size <w> <h>` / `window info`** — the REAL Chrome window, unlike `emulate viewport` which only lies to the page.
Set it before a coordinate workflow so pixel coordinates are reproducible across runs.

## `upload` — attach files without the OS dialog

`upload "<sel>" <path> [<path>…]` sets the files directly on an `<input type=file>` (firing `change`) — it never clicks the input, because the native OS file picker is invisible to CDP and can't be dismissed.
Paths are checked before Chrome is contacted (a missing file is exit 2, no connection), `--wait` defaults to `ready` because the real input behind a styled drop zone is usually hidden, `--append` adds to files this session already set, and `--wait-text "Uploaded"` folds in the confirm.

**`upload --drop "<sel>" <path>`** (or `--drop-at x,y`) is for a drop zone with NO `<input type=file>` behind it.
Prefer plain `upload` whenever an input exists (including the hidden one behind most styled drop zones); `--drop` is for the apps that have none.
Check `drop_handled` in the result: `false` means nothing consumed the drop (you addressed the wrong element) even though the command succeeded.
The page is never modified by this — no element is added and no attribute written — and it composes with every addressing mode, including `--by name`.

