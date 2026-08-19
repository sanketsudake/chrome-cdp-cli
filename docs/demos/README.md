# Demo GIFs

Two short recordings, each made against a local `data:` fixture page — not a real logged-in site — using `chrome-cdp record`.
Every frame shows only the fixture's own markup: a heading, a button, and a couple of `role=option` rows.

## `click-by-name.gif`

Clicks a button by its accessible name and waits for a toast to appear.
Fixture: a page with `<button>Show message</button>` and a `role="status"` div that fades in on click.

```sh
chrome-cdp open 'data:text/html;base64,<the fixture above, base64-encoded>'
chrome-cdp activate
chrome-cdp record start --fps 8 --scale 0.5
chrome-cdp click --by name "Show message" --role button
chrome-cdp record stop --format gif -o docs/demos/click-by-name.gif
```

## `select-cascade.gif`

Opens a two-level cascade prompt (`role="textbox"` field → `role="option"` rows) and drills from a category into a leaf.
Fixture: a page with a `Category` field and a JS-rendered popup with `Fruit`/`Vegetable` categories, each expanding to two leaf options.

```sh
chrome-cdp open 'data:text/html;base64,<the fixture above, base64-encoded>'
chrome-cdp activate
chrome-cdp record start --fps 8 --scale 0.5
chrome-cdp click --by name "Category" --role textbox   # open the popup
chrome-cdp click --by name "Fruit" --role option        # drill into the category
chrome-cdp click --by name "Apple" --role option        # select the leaf
chrome-cdp record stop --format gif -o docs/demos/select-cascade.gif
```

`select "Category" "Fruit > Apple" --role textbox` drives the same three clicks in one command.
The GIF was recorded with the individual clicks instead so the drill-down shows up as distinct frames.

`click --wait-text "<substr>"` folds act-and-confirm into the one call, and was left out of the recorded sequence above only because the fixture's text is already in the accessibility tree before the click (its state changes via an opacity fade, not a `display` toggle), so a wait after it is a no-op.
Against a page where the confirming text is genuinely added after the click, add `--wait-text` to the `click` line instead of a separate `wait --text` step.

## Notes

- Both recordings were made with `chrome-cdp record start --fps 8 --scale 0.5`, which keeps the exported GIF well under the 2 MB budget (both are under 60 KB).
- The fixture pages are throwaway `data:` URLs with no real data, so nothing in either GIF needs blurring or cropping.
- `docs/demos/*.gif` is the one exception to the repo-wide `*.gif` ignore rule in `.gitignore`.
