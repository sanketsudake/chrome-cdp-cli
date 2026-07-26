# Saving an automation as a recipe

You worked out the eleven commands that submit your timesheet.
Right now that knowledge is in your shell history, and next week it will be gone.

A **recipe** is where it goes instead: a YAML file whose steps are the same argv arrays `session` already reads, with declared inputs so one file covers every week.
Name it, re-run it, commit it, hand it to a colleague.

The worked example this guide builds is [`recipes/submit-timesheet.yaml`](recipes/submit-timesheet.yaml).

## Start from something that works

Get the sequence right on the command line first, the way [Automating a logged-in web app](automating-a-logged-in-web-app.md) describes: `list → use → snap → act → verify`.
A recipe is a record of a working sequence, not a way to discover one.
Nothing in the format helps you find a selector.

Once the batch runs cleanly under `session`, you already have the recipe's `steps` — they are the same lines.

## Scaffold the file

```sh
chrome-cdp recipe new submit-timesheet
# → .chrome-cdp/recipes/submit-timesheet.yaml
```

The template is a working recipe, not a sketch: it loads, validates, and dry-runs as written, so you can edit it one step at a time and check yourself as you go.

## The format, and why it is this small

```yaml
name: submit-timesheet
description: Fill a week of hours in the timesheet grid and save it.
inputs:
  week:  { required: true, description: "Monday of the week, as the grid labels it" }
  hours: { default: "8",   description: "Hours per weekday" }
target: url:workday
steps:
  - label: open the timesheet
    run: ["nav", "https://workday.internal/time"]
  - run: ["wait", "--idle"]
  - label: save and confirm
    run: ["click", "--by", "name", "Save and Close", "--role", "button", "--wait-text", "saved"]
    on_error: abort
```

`run` is an argv array — identical to a `session` stdin line.
Anything valid in `session` is valid here and vice versa, which is the whole design: there is one execution path, and a recipe is a file that feeds it.
Flags a verb already takes go in the array, so `--timeout 60s` on the one slow step is just another element.

`{{name}}` substitutes an input **into one argv element**.
There is no shell in this design and there is no `shell:` step type, so `--set note="; rm -rf /"` reaches the browser as one literal string and nothing in it is interpreted.
That is not a hardening measure bolted on afterwards; it is why the format has argv arrays instead of command lines.

`on_error` is `abort` (the default) or `continue`.
There are no retries, no conditionals, and no loops.
If your automation needs them, write a program that calls `session` — that is the supported answer, and a bigger recipe format is not.

## Run it

```sh
chrome-cdp recipe run submit-timesheet --set week="Mon, 7/20"
```

You get one envelope per step, each carrying `step` and `label`, then a summary:

```json
{"ok":true,"command":"nav","result":{"url":"https://workday.internal/time"},"step":1,"label":"open the timesheet"}
...
{"ok":true,"command":"recipe","result":{"recipe":"submit-timesheet","steps":7,"completed":7,"failed":null}}
```

If a step fails, the run stops there, the summary says which step and why —
`failed: {"index":6,"label":"save and confirm","code":"target_timeout"}` — and **the process exits with that step's code**.
So a shell caller branches exactly as it would on a single command:

```sh
chrome-cdp recipe run submit-timesheet --set week="Mon, 7/20" || echo "failed with $?"
```

Under `--quiet` you get the summary alone, which is what you want from cron.

## Read one before you run it

A recipe drives the browser you are already signed into.
Treat one you were sent the way you would treat a shell script someone sent you:

```sh
chrome-cdp recipe show their-recipe            # the source, comments and all
chrome-cdp recipe run their-recipe --dry-run   # the resolved argv, one array per line
```

The dry run prints the exact bytes `session` consumes, so you can also just run them:

```sh
chrome-cdp recipe run their-recipe --dry-run | chrome-cdp session
```

That pipeline is worth knowing for debugging, and it is the honest demonstration that recipes add no hidden magic — the two paths drive the browser identically, and a test in the suite asserts it.

## Share it with your team

Recipes resolve project-local first, then from your own config directory:

1. `./.chrome-cdp/recipes/`
2. `$XDG_CONFIG_HOME/chrome-cdp/recipes/`
3. `--dir <path>`

Commit `.chrome-cdp/recipes/` and everyone who clones the repo gets your internal-app automations, in a form a reviewer can read in a pull request.
Project-local winning is what makes that work: the repo's copy beats whatever a teammate happens to have personally.
`chrome-cdp recipe list` marks each entry's source, so it is always visible which copy is about to run.

**Never put a credential in a recipe.**
The premise of this tool is reusing an already-authenticated browser, so a recipe never needs one — and a recipe is a file you are trying to make easy to share.

## When it fails halfway

`--from-step <n>` starts at step *n*, 1-based:

```sh
chrome-cdp recipe run submit-timesheet --set week="Mon, 7/20" --from-step 5
```

This exists because real automations fail halfway and re-running from the top is often wrong — a form that is already partly submitted does not want to be submitted again.

It is a sharp tool.
It assumes every earlier step's effect is already in place, and nothing checks that assumption for you.
Look at the page (or `chrome-cdp snap`) before you use it, and prefer fixing the recipe when the failure was the recipe's fault rather than the app's.

## What is deliberately missing

No conditionals, loops, retries, or branching.
No reading a value out of one step's envelope and using it in the next — genuinely useful, and the thing that would turn this format into a programming language, so it waits for evidence from real recipes.
Recipes cannot invoke recipes, and one recipe is capped at 200 steps.

The moment you want any of that, the answer is a program that calls `session`.
You lose nothing by dropping down: the steps are the same argv lines you already had.
