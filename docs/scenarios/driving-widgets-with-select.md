# Driving widgets with `select`

Some controls can't be driven by a plain `click` or `type`.
A portal menu, a cascade prompt, or a rich combobox mounts **collapsed** — a zero-size box that animates open on a real pointer sequence — so a single synthetic click lands mid-animation on nothing and registers as an outside-click that closes the popup.
The `select` verb is built for exactly these.

## What `select` does differently

It runs the whole open-and-choose choreography over one held connection: it brings the tab to the front, coordinate-clicks the field at its live occlusion-verified centre, re-reads the geometry between the open and the option click, and drills a cascade level by level — committing a real selection rather than a click that bounces off.
It **errors** rather than reporting a false success if the path is incomplete (e.g. the final segment resolves to a category, not a selectable leaf).

## The three shapes it handles

**A native `<select>`** — addressed by its visible label when it has no accessible name:

```sh
chrome-cdp select --by label "Activity Category" "Direct Revenue"
```

**A portal menu** — a button that opens a floating menu whose item a plain click can't reach:

```sh
chrome-cdp select "Actions" "Enter Time by Type" --role button
```

**A cascade prompt** — a tree several levels deep, given as a `>`-separated path:

```sh
chrome-cdp select "Time Type" "Projects > Acme: Platform > Project > Time Entry" --role textbox
```

`--role textbox` disambiguates the input from a same-named column header.
Option segments match by case-insensitive **substring** by default, so a short config value (`Projects`) matches a verbose rendered label (`Projects and Tasks`); use `--option-match exact` when a substring would collide (`40 Hour Week` vs `Over 40 Hour Week`).

## Tuning the match

| Flag | When |
|------|------|
| `--option-match exact` | a substring match would hit the wrong option |
| `--option-match regex` | you need a pattern |
| `--filter "<text>"` | a long option list — type to narrow it before selecting |
| `--sep "/"` | the path naturally contains `>` |

## When it doesn't open on the first try

Some menus anchor inconsistently and render mis-positioned on a given attempt.
`select` returns a safe `did not render / settle` — a no-op, never a wrong click — so just re-run it; it opens on the next try.

## Confirming the choice

A cascade commits a selected-item "pill"; read it back to confirm before moving on:

```sh
chrome-cdp snap --region "Time Type"     # the committed selection shows here
```

Or fold the confirmation into the next write with `--wait-text`.
