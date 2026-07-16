# Automating a logged-in web app

The reason `chrome-cdp` exists: drive a web app you're **already signed into** — your real session, cookies, and SSO — without a headless browser, a second login, or a stored credential.
Because it attaches to your own Chrome, the app loads authenticated and you never type a password.

This guide shows the loop that every such task follows.

## The loop

```
list ─▶ use ─▶ snap ─▶ act ─▶ verify
```

Find the tab, make it current, read what's actionable, do one thing, confirm it landed — then repeat.
Read before you act, and confirm after; the two reads are what make the run reliable on a page you didn't hard-code selectors for.

## 1. Find the tab and make it current

```sh
chrome-cdp list --url dashboard        # the --url filter beats grepping the whole list
chrome-cdp use url:dashboard           # sticky: later commands need no --target
```

No tab open yet?
`chrome-cdp open https://app.example.com/dashboard` creates one, navigates, and makes it current.

## 2. Confirm you're actually signed in — by content, not URL

A single-page app can render a **login view at the app's own URL**, so a URL that looks right isn't proof.
Check for a login control instead:

```sh
chrome-cdp wait --idle                          # let the SPA settle (network, not a fixed sleep)
chrome-cdp snap --grep "Log ?in|Sign ?in"       # a login control present ⇒ not signed in
```

If a login control shows, click your app's SSO entry, wait, and re-check.
If the click lands on `login.microsoftonline.com` or a passkey screen, **stop and let the user finish that step by hand** — never drive a passkey.

## 3. Read what's actionable

`snap` is the accessibility-tree view: every actionable control by role and accessible name, crossing shadow DOM and iframes, with toasts under `alerts` and widget state per node.
Filter it server-side so you read ten nodes, not a thousand:

```sh
chrome-cdp snap --role button                   # just the buttons
chrome-cdp snap --region "Approvals"            # just one container's subtree
```

Orient here, then address controls by their **accessible name** — robust where CSS ids churn per session:

```sh
chrome-cdp click --by name "Review" --match contains --role button
```

`--match contains` clicks by a fragment (`Review`) without copying a verbose full name (`Review Approval: Awaiting Action by …`).

## 4. Act, and confirm in the same call

For a write, fold the confirmation into the action with `--wait-text` — it blocks until the toast appears, so success is proven, not assumed:

```sh
chrome-cdp click --by name "Approve" --role button --wait-text "Success"
```

## 5. Verify

Re-read to confirm the effect before the next step — a fresh `snap`, a `wait --text`, or reading `snap.alerts`.
Confirm a write by its toast or a re-read, **not** by a screenshot.

## Worked example: approve a pending item

```sh
chrome-cdp use url:app
chrome-cdp snap --role button                                   # find the exact name
chrome-cdp click --by name "Review" --match contains --role button
chrome-cdp wait --stable                                        # let the panel open
chrome-cdp click --by name "Approve" --role button --wait-text "Success"
```

Five commands, no CSS selectors, no credentials — and every step either reads state or confirms it.

## When a control won't open

Some widgets — portal menus, cascade prompts, native `<select>`s — mount collapsed and a plain `click` closes them.
Use the [`select`](driving-widgets-with-select.md) verb for those.
For a control repeated across table rows (a per-row Delete/Edit), scope the name match with `--in-row`.
For a form of labelled fields, see [Forms and grids](forms-and-grids.md).

## Etiquette

You're driving the user's real browser.
Go one command at a time on a bounded `--timeout`, avoid actions that raise a native dialog (use `--on-dialog accept|dismiss` when one is unavoidable), and never submit or approve anything the user hasn't confirmed.
