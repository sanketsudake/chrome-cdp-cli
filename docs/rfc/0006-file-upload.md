# RFC-0006: File upload — the `upload` verb

- **Status:** Accepted — implemented in [#12](https://github.com/sanketsudake/chrome-cdp-cli/pull/12)
- **Priority:** P1
- **Area:** input
- **Depends on:** —
- **Related:** RFC-0012 (path allow-listing shares the policy mechanism)

## Summary

Add an `upload` verb that attaches one or more local files to a file input on the page, via `DOM.setFileInputFiles`, without opening the OS file picker.

## Summary of why it must not click

Clicking a file input opens a **native OS file dialog**.
That dialog is outside the page, invisible to CDP, and blocks the browser's main thread — the same class of wedge the CLI already guards against with `--on-dialog` for JavaScript dialogs, but worse, because there is no CDP method to dismiss it.
Any automation that reaches a file input by clicking is already broken.
`DOM.setFileInputFiles` sets the input's files directly and fires `change`, which is the only correct way to do this.

## Motivation

File upload is a routine step in the applications this CLI targets — expense receipts, timesheet attachments, document management, bulk import screens, avatar changes, support-ticket attachments.
There is currently no way to perform it at all, and unlike most gaps here, there is no partial workaround: `eval` cannot construct a trusted `FileList`, and clicking makes things worse rather than better.

This means a single upload step can block an otherwise fully automatable workflow end to end.
That is a disproportionate cost for a feature that is one CDP call plus argument validation.

## User stories

**US-1 — Attach a receipt.**
As an automation author, I want to attach a local PDF to an expense form so that the whole submission runs unattended.
*Acceptance:* `chrome-cdp upload --by label "Receipt" ./receipt.pdf` sets the file and the form's validation accepts it.

**US-2 — Attach to a hidden input.**
As an automation author, I want to target the real `<input type=file>` behind a styled drop zone so that a custom uploader widget works too.
*Acceptance:* with a visually hidden input, `upload --no-wait "input[type=file]"` succeeds without requiring visibility.

**US-3 — Attach several files at once.**
As an automation author, I want to pass multiple paths so that a multi-file uploader receives them in one operation.
*Acceptance:* `chrome-cdp upload "#docs" a.pdf b.pdf c.pdf` results in three files on a `multiple` input.

**US-4 — Fail clearly on a wrong target.**
As an agent, I want a distinct error when the element is not a file input so that I correct my selector instead of retrying a doomed action.
*Acceptance:* targeting a text input returns a typed error naming the actual element, not a generic timeout.

**US-5 — Fail clearly on a missing file.**
As an agent, I want a missing or unreadable path to fail before Chrome is contacted so that the failure is unambiguous and cheap.
*Acceptance:* a nonexistent path exits 2 with `usage` and names the path, with no connection attempt.

**US-6 — Know that the page saw it.**
As an automation author, I want confirmation that the `change` event fired so that I do not proceed to Submit before the uploader registered the file.
*Acceptance:* the envelope reports the resulting file names and sizes as read back from the input, and `--wait-text` composes for the app's own confirmation.

**US-7 — Bound what can be read from my disk.**
As a security-conscious user handing this to an agent, I want to restrict which directories are uploadable so that a bad instruction cannot exfiltrate arbitrary local files.
*Acceptance:* with `upload_roots` configured, a path outside those roots is refused with a typed policy error.

## Proposed CLI surface

```
chrome-cdp upload <selector> <path> [<path>...]
```

| Flag | Default | Purpose |
|------|---------|---------|
| `--append` | off | add to the input's existing files instead of replacing them |
| `--wait-text <s>` | — | inherited act-and-confirm flag |
| all `QueryOpts` flags | — | `--by`, `--wait`, `--role`, `--nth`, `--match`, `--in-row`, `--pierce` |

Default `--wait` for this verb is `ready` rather than `visible`, because correct targets are frequently visually hidden (US-2).
That is a deliberate per-verb default, documented in `cli-reference.md`.

Examples:

```sh
chrome-cdp upload --by label "Receipt" ./receipt.pdf
chrome-cdp upload "input[type=file]" ~/docs/report.pdf --wait-text "Uploaded"
chrome-cdp upload "#attachments" a.pdf b.png c.csv
```

## Result envelope

```json
{ "ok": true, "command": "upload",
  "target": {"id":"…","title":"…","url":"…"},
  "result": {
    "files": [{"name":"receipt.pdf","size":48213,"type":"application/pdf"}],
    "count": 1, "multiple": false, "accept": ".pdf,.png",
    "change_fired": true
  },
  "elapsed_ms": 62 }
```

`files` is read back **from the input element after the call**, not echoed from the arguments — that is what makes it evidence rather than an assumption (US-6).
`accept` and `multiple` are reported because a mismatch is the most common reason an upload appears to succeed and then silently does nothing.

## Errors and exit codes

| Situation | `error.code` | Exit |
|-----------|--------------|------|
| No paths given | `usage` | 2 |
| Path does not exist, is a directory, or is unreadable | `usage` | 2 |
| Path outside `upload_roots` when configured | `permission_denied` *(new)* | 7 *(new)* |
| Selector never resolves | `target_timeout` | 4 |
| Resolved element is not `<input type=file>` | `usage` | 2 |
| Multiple paths given to a non-`multiple` input | `usage` | 2 |
| `DOM.setFileInputFiles` rejected | `cdp_error` | 5 |

**New contract entries:** `CodePermissionDenied = "permission_denied"` and a matching `codeToExit` entry.
Per the envelope rule, a new failure mode without both silently degrades to `ExitGeneric`.
This code is shared with RFC-0012, which is why the two RFCs should be reviewed together — whichever lands first defines it.

Note the deliberate choice to classify "wrong element type" as `usage` (exit 2) rather than `target_timeout` (exit 4): the selector *did* resolve, so retrying will not help, and an agent needs to distinguish "wait longer" from "fix your selector" (US-4).

## Design notes

- **Interface:** ```go Upload(ctx context.Context, targetID, selector string, paths []string, opts UploadOpts) (map[string]any, error) ``` with `UploadOpts{Append bool; Query QueryOpts}`.
- **Path handling is entirely CLI-side and happens before connecting.**
  Expand `~`, resolve to absolute, `os.Stat` each path, reject directories and unreadable files, then evaluate `upload_roots` — all as `usage`/`permission_denied` with no browser contact (US-5, US-7).
  CDP requires absolute paths, so resolution is functional, not just hygienic.
- **`--append` requires reading the existing `FileList` first**, and CDP's `setFileInputFiles` replaces wholesale.
  Existing file *paths* are not readable back from the DOM for security reasons, so append can only be honoured for files this CLI itself set during the session.
  Implement by tracking per-input state in the connection and returning a clear `usage` error when append is requested on an input whose prior contents are unknown — an honest limitation beats a silently wrong result.
- **Element type check before dispatch.**
  Resolve the node, verify `nodeName == "INPUT"` and `type == "file"`, and report the actual tag and type in the error message when it is not.
  Cheap, and it turns US-4's confusing timeout into a one-line fix.
- **`accept` mismatch is a warning, not an error.**
  Report `accept` in the envelope and, when a supplied file's extension is not covered, include `accept_mismatch: true`.
  Do not refuse — `accept` is advisory in HTML and some apps set it loosely.
- **`upload_roots`** is a config list of directories (default: unset = unrestricted, preserving current behaviour).
  Comparison is on the cleaned absolute path with symlinks resolved, so `../` traversal and symlink escapes are both handled.
  This must be tested adversarially, not just for the happy path.
- **Drop-zone widgets** that never render an `<input type=file>` and only listen for `drop` events are out of scope here; note the limitation in the docs and cross-reference RFC-0005's drag work.
- **Stub:** `Upload` returns `{"files": []any{}, "count": 0, "change_fired": true}`.

## Verification scenarios

**VS-1 — Single file lands and the page sees it** Given a fixture with `<input type=file>` recording `change` events When `upload "#f" ./testdata/a.txt` runs Then the input's `files[0].name` is `a.txt`, `change_fired` is true, and the fixture recorded one change event.

**VS-2 — Multiple files** Given `<input type=file multiple>` When three paths are uploaded Then `files.length == 3` in the page and `count == 3` in the envelope, in argument order.

**VS-3 — Too many files for a single input** Given a non-`multiple` input When two paths are given Then exit 2 with `usage`, and the page's input is untouched.

**VS-4 — Hidden input works** Given an input with `style="display:none"` When `upload` runs with the default `--wait ready` Then it succeeds.

**VS-5 — Wrong element type** Given `<input type=text id=t>` When `upload "#t" ./testdata/a.txt` runs Then exit 2, `usage`, and the message names `input[type=text]`.

**VS-6 — Missing file never connects** When `upload "#f" ./nope.txt` runs with no Chrome available Then exit 2, `usage`, the path appears in the message, and no browser method was called.

**VS-7 — Directory rejected** When a directory path is given Then exit 2 with `usage`.

**VS-8 — Tilde and relative paths resolve** Given a file in `t.TempDir()` referenced relatively When `upload` runs Then it succeeds and the reported name matches.

**VS-9 — `upload_roots` denies outside paths** Given `upload_roots = ["/tmp/allowed"]` and a file in a different temp dir When `upload` runs Then exit 7 with `permission_denied`, and no browser method was called.

**VS-10 — `upload_roots` resists traversal and symlinks** Given `upload_roots = ["<tmp>/allowed"]` When the path is `<tmp>/allowed/../secret.txt`, or a symlink inside `allowed` pointing outside it Then both are refused with `permission_denied`.

**VS-11 — Envelope reports read-back state, not arguments** Given a fixture whose `change` handler clears the input When `upload` runs Then `files` reflects the post-call DOM state, and does not blindly echo the argument list.

**VS-12 — `accept` mismatch is surfaced but not fatal** Given `<input type=file accept=".pdf">` and a `.txt` file When `upload` runs Then the envelope is `ok` with `accept_mismatch: true`.

**VS-13 — Composes with `--wait-text` and inside `session`** Given a `session` line uploading then a line clicking Submit Then both emit `ok` envelopes in order over one connection.

**VS-14 — No native dialog is ever opened** Given the upload path When the verb runs Then no `Page.javascriptDialogOpening` is observed and the connection remains responsive for the next command.

## Test plan

**Unit — path validation and policy (`internal/cli`, `t.TempDir()`, stub failing on any browser call, `t.Parallel()`)** VS-6, VS-7, VS-8, VS-9, VS-10, plus empty-argument and permission-bit cases.
VS-10 is the security-relevant one and should be table-driven over traversal, symlink, absolute-outside, and case-variant forms.

**Unit — command boundary (`chrometest.StubBrowser`)** VS-3's usage mapping, envelope shape, `--append` rejection when prior state is unknown, `--wait` default is `ready` for this verb only.

**Live Chrome (`internal/chrome`, `testing.Short()`-guarded, not parallel)** VS-1, VS-2, VS-4, VS-5, VS-11, VS-12, VS-14 against fixture pages with files written into `t.TempDir()`.
Fixtures record `change` events into `window.__log`, read back with `eval` — the same pattern RFC-0005 uses.

**Contract test (`internal/result`)** Assert `permission_denied` maps to exit 7 and that the "unknown code → ExitGeneric" property still holds after the addition.
This is the test that enforces the envelope rule for the new code.

**Session integration (`internal/cli`)** VS-13.

## Out of scope

- Drop-zone widgets with no underlying file input (needs synthesized `DataTransfer`; see RFC-0005 Open Questions).
- Uploading generated or in-memory content without a file on disk.
- Downloading files from the page — the inverse operation, and a separate RFC.
- Progress reporting for long uploads.

## Open questions

1. Should `upload_roots` default to unrestricted (current behaviour) or to something conservative like the working directory plus `$HOME/Downloads`?
   Restricting by default is safer but breaks the principle that adding a feature should not change existing behaviour — though there is no existing behaviour here, since the verb is new.
   **Recommendation:** unrestricted by default for the CLI, but **restricted by default in MCP mode** (RFC-0004), where the caller is an agent rather than the user.
2. Should a `--dry-run` exist that validates paths and the target element without dispatching?
   Useful for agents planning a multi-step flow.
   **Recommendation:** defer; `--no-wait` plus a `snap` covers most of it.
3. Should the verb be named `upload` or `attach`?
   `upload` matches user vocabulary; `attach` is more literally accurate since no network transfer happens at this step.
   **Recommendation:** `upload`, because that is what users will search for.
