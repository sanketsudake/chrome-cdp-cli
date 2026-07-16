# Forms and grids

Enterprise forms are where CSS selectors go to die: ids like `#56$1-input` change every session, labels aren't wired to their controls, and a week of hours lives in a grid addressed only by column.
`chrome-cdp` gives you three addressing modes that survive this, plus a batch mode that fills a whole grid over one connection.

## Address a control by its visible label

When a field's label is text a human reads but nothing the DOM links to the control (no `aria-label`, no `<label for>`), `--by label` finds the control by that visible text — no `eval` hunt for a selector.

```sh
chrome-cdp fill --by label "Notes" "Q3 planning"
chrome-cdp select --by label "Category" "Direct Revenue"   # even a bare native <select>
```

`fill` **replaces** the field's content (it clears, then types), so a field pre-filled with `0` becomes `8`, not `80`.
Use `type` only when you mean to append.

## Address a grid cell by its column

`--by cell "<column header>"` resolves the editable input under a column, so you fill by the header a human sees instead of mapping inputs to x-coordinates:

```sh
chrome-cdp fill --by cell "Mon, 7/13" "8"
chrome-cdp fill --by cell "Regular|Mon, 7/13" "8"   # row|column, for a multi-row grid
```

## Fill a whole grid over one connection

Spawning a process per cell is slow and drops the held connection between calls.
`session` reads NDJSON commands on stdin and runs them all over one connection, so the fills, the read-back, and the save happen in a single attach:

```sh
printf '%s\n' \
  '["fill","--by","cell","Mon, 7/13","8"]' \
  '["fill","--by","cell","Tue, 7/14","8"]' \
  '["fill","--by","cell","Wed, 7/15","8"]' \
  '["value","--all","input[data-automation-id=numericInput]"]' \
  '["click","--by","name","Save and Close","--role","button","--wait-text","saved"]' \
  | chrome-cdp session
```

The `value --all` line reads every hour cell back in one call so you can verify the row before the save commits, and `--wait-text "saved"` proves the save landed.

## Read the grid back as a table

To check state without parsing `snap`, read the grid as structured data:

```sh
chrome-cdp grid          # → { headers: [...], rows: [[...]], count: N }
```

## Putting it together

A review-first fill is: read the current grid, propose the change, fill on confirmation, read back, save.

```sh
chrome-cdp grid                                     # 1. read current state
# 2. propose to the user, get confirmation, then:
chrome-cdp fill --by cell "Mon, 7/13" "8"           # 3. fill
chrome-cdp value --all "input.hours"                # 4. read back
chrome-cdp click --by name "Save" --role button --wait-text "saved"   # 5. save + confirm
```

Every value is set by a header or label a human can see, so the same script survives the app renaming an internal id overnight.

## A note on dates and native widgets

Date pickers and native `<select>`s have their own quirks — a picker click that only highlights, a `<select>` with no accessible name, a stored date that shifts a day across a timezone boundary.
Set values by visible label, **verify the committed value after**, and re-issue the set if a picker didn't take.
See also [Driving widgets with `select`](driving-widgets-with-select.md).
