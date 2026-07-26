package cli

// The `upload` verb (RFC-0006): attach local files to a file input.
//
// Every path decision is made HERE, before Chrome is contacted: expand `~`,
// resolve to the absolute path CDP requires, stat it, refuse a directory or an
// unreadable file, then evaluate the upload_roots allow-list. A bad path is
// usage / exit 2 and a path outside the roots is permission_denied / exit 7,
// both without a connection, a launch, or a consent prompt the user should
// never have seen.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

func (a *App) cmdUpload() *cobra.Command {
	var appendFiles bool
	c := &cobra.Command{
		Use:   "upload <selector> <path> [<path>...]",
		Short: "Attach local files to a file input (never opens the OS file dialog)",
		Long: "Attach one or more local files to an <input type=file>.\n\n" +
			"The files are set directly on the input (DOM.setFileInputFiles), which also\n" +
			"fires `change` — the verb never clicks the input, because a click opens the\n" +
			"native OS file dialog, which is invisible to CDP and has no way to be\n" +
			"dismissed.\n\n" +
			"Paths are resolved and checked before Chrome is contacted, so a missing file\n" +
			"is exit 2 with no connection. --wait defaults to `ready` rather than `visible`\n" +
			"for this verb alone: the real input behind a styled drop zone is usually\n" +
			"hidden.\n\n" +
			"  chrome-cdp upload --by label \"Receipt\" ./receipt.pdf\n" +
			"  chrome-cdp upload \"#attachments\" a.pdf b.png c.csv\n" +
			"  chrome-cdp upload \"input[type=file]\" ~/docs/report.pdf --wait-text \"Uploaded\"",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch len(args) {
			case 0:
				a.emitErr("upload", result.CodeUsage, "upload needs a selector and at least one file path: upload <selector> <path> [<path>...]", nil)
				return nil
			case 1:
				a.emitErr("upload", result.CodeUsage, "no paths given: upload <selector> <path> [<path>...]", nil)
				return nil
			}
			paths, rerr := resolveUploadPaths(args[1:], a.uploadRoots(), homeDir())
			if rerr != nil {
				a.emitErr("upload", rerr.Code, rerr.Message, rerr.Details)
				return nil
			}

			q := a.queryOpts()
			// The per-verb default (RFC-0006 US-2): a correct file input is
			// frequently display:none behind a styled drop zone, so waiting for
			// visibility would fail on the targets that need this verb most.
			if !cmd.Flags().Changed("wait") {
				q.Wait = "ready"
			}
			a.runUpload(args[0], paths, chrome.UploadOpts{Append: appendFiles, Query: q})
			return nil
		},
	}
	c.Flags().BoolVar(&appendFiles, "append", false,
		"add to the files THIS session set on the input, instead of replacing them (refused when the input's current files are unknown)")
	return a.withWaitText(c)
}

// runUpload resolves the target, dispatches the upload, and emits the envelope.
//
// It does not reuse runResolved because the driver has two failure modes the
// shared classifier cannot see: an element that resolved but is not a file
// input, and more files than a non-`multiple` input accepts. Both are the
// caller's bug — the selector DID resolve — and must be usage / exit 2 rather
// than the target_timeout / exit 4 an agent reads as "wait longer and retry".
func (a *App) runUpload(selector string, paths []string, opts chrome.UploadOpts) {
	ctx, cancel := a.ctx()
	defer cancel()
	tgt, b, rerr := a.resolveTarget(ctx)
	if rerr != nil {
		a.emitErr("upload", rerr.Code, rerr.Message, rerr.Details)
		return
	}
	res, err := b.Upload(ctx, tgt.ID, selector, paths, opts)
	if err != nil {
		if chrome.IsUploadUsage(err) {
			a.emitErr("upload", result.CodeUsage, err.Error(), nil)
			return
		}
		code, msg, details := a.classifyWithTabHint(b, tgt.ID, err)
		a.emitErr("upload", code, msg, details)
		return
	}
	// --wait-text folds act-and-confirm into one call, exactly as it does for
	// the other action verbs.
	if a.actWaitText != "" {
		if _, werr := b.Wait(ctx, tgt.ID, chrome.WaitCond{Text: a.actWaitText}); werr != nil {
			a.emitErr("upload", classifyActionErr(werr), "action ok but wait-text failed: "+werr.Error(), nil)
			return
		}
		if res != nil {
			res["waited_text"] = a.actWaitText
		}
	}
	a.emitOK("upload", tgt, res)
}

// resolveUploadPaths turns the caller's path arguments into the absolute paths
// CDP requires, refusing anything that is not a readable regular file and
// anything outside roots. It is pure apart from the filesystem it has to ask,
// and it never touches the browser.
func resolveUploadPaths(args, roots []string, home string) ([]string, *result.Err) {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.TrimSpace(arg) == "" {
			return nil, usageErr("empty file path given")
		}
		abs, err := filepath.Abs(expandTilde(arg, home))
		if err != nil {
			return nil, usageErr("cannot resolve %q to an absolute path: %v", arg, err)
		}
		info, err := os.Stat(abs)
		switch {
		case os.IsNotExist(err):
			return nil, usageErr("no such file: %s (resolved to %s)", arg, abs)
		case err != nil:
			return nil, usageErr("cannot read %s: %v", arg, err)
		case info.IsDir():
			return nil, usageErr("%s is a directory, not a file (resolved to %s)", arg, abs)
		case !info.Mode().IsRegular():
			return nil, usageErr("%s is not a regular file (resolved to %s)", arg, abs)
		}
		// Openability, not just the permission bits: an unreadable file would
		// otherwise fail inside Chrome, far from the argument that caused it.
		f, err := os.Open(abs)
		if err != nil {
			return nil, usageErr("cannot read %s: %v", arg, err)
		}
		_ = f.Close()

		if rerr := checkUploadRoots(abs, roots); rerr != nil {
			return nil, rerr
		}
		out = append(out, abs)
	}
	return out, nil
}

// uploadRoots returns the directories files may be uploaded from: the
// `upload_roots` key of the policy table (RFC-0012's `[policy]`, which RFC-0006
// shares). No roots means unrestricted, which is what the CLI has always done —
// adding a verb must not change existing behaviour — and a policy table that is
// present but disabled is inert, like the rest of that layer.
//
// The allow-list is deliberately neither a flag nor an environment variable.
// Either would be worth nothing here: the point is to bound what a misdirected
// agent can read off the disk, and the agent writes the argv and the
// environment. It has to come from the user's config file.
//
// Which is also why --policy-off does NOT lift it, and why this reads
// configuredChecker rather than policyChecker. For the origin allow-list,
// --policy-off is in the threat model: RFC-0012 says a bad policy that cannot be
// bypassed is worse than none, and the bypass is warned and audited. That
// argument does not transfer to a directory list. upload_roots answers RFC-0006
// US-7, whose threat model is specifically an agent that writes the argv — and
// `--policy-off` is argv. A roots list any caller could switch off by adding a
// flag would bound nobody, and unlike a pattern list there is no "my policy is
// wrong and I am locked out" failure it needs an escape hatch for: the fix for a
// path outside the roots is to widen the roots or move the file.
func (a *App) uploadRoots() []string {
	c := a.configuredChecker()
	if !c.Active() {
		return nil
	}
	roots := c.UploadRoots()
	for i := range roots {
		roots[i] = expandTilde(roots[i], homeDir())
	}
	return roots
}

// checkUploadRoots enforces the upload_roots allow-list.
//
// The comparison lives here rather than in policy.Checker.CheckPath because
// this is the side that does I/O: policy is a pure decision package, and the
// only honest root comparison resolves symlinks on BOTH sides. That is not
// optional on macOS, where /tmp and /var are themselves symlinks — a configured
// "/tmp/receipts" and the real path of a file inside it are different strings.
//
// The comparison is on the cleaned, absolute, SYMLINK-RESOLVED path on both
// sides, which is what makes it resist the two escapes that matter:
// "<root>/../secret.txt" cleans its way out of the root before the compare, and
// a symlink planted inside the root pointing outside it resolves to where it
// really goes. Either would sail through a naive prefix test on the string the
// user typed. The separator in the prefix matters too: without it, a root of
// "/tmp/allowed" would also admit "/tmp/allowed-evil/secret".
//
// A root that cannot itself be resolved is skipped rather than trusted: it
// contains nothing, so failing closed is the only safe reading.
func checkUploadRoots(abs string, roots []string) *result.Err {
	if len(roots) == 0 {
		return nil // unset = unrestricted, the documented default
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		resolved = filepath.Clean(abs)
	}
	for _, root := range roots {
		r, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		r = filepath.Clean(r)
		if resolved == r || strings.HasPrefix(resolved, r+string(os.PathSeparator)) {
			return nil
		}
	}
	return &result.Err{
		Code: result.CodePermissionDenied,
		Message: fmt.Sprintf("%s is outside the configured upload_roots (%s)",
			resolved, strings.Join(roots, ", ")),
		Details: map[string]any{"path": resolved, "upload_roots": roots},
	}
}

// expandTilde expands a leading `~` (the shell does it for an unquoted path;
// nothing does it for a quoted one or a value read from config).
func expandTilde(p, home string) string {
	if home == "" || p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}
