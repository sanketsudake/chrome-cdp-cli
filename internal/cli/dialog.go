package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// cmdDialog is the `dialog` verb family (RFC-0018): status | accept [text] |
// dismiss, for a native JavaScript dialog (alert/confirm/prompt/beforeunload)
// that is ALREADY on screen — the recovery for a click that timed out behind
// an unguarded confirm(), which --on-dialog cannot help with because it only
// handles a dialog an action itself opens.
//
// Each subcommand runs its own resolve-call-emit sequence (as console and
// record do) rather than targetAction: the error mapping is its own, and it
// must not inherit --wait-text, which this verb has no meaning for.
func (a *App) cmdDialog() *cobra.Command {
	c := &cobra.Command{
		Use:   "dialog",
		Short: "Inspect or close a native JavaScript dialog already on screen",
		Long: "A native alert/confirm/prompt/beforeunload blocks the page's renderer for as\n" +
			"long as it is up: every eval, DOM read and screenshot hangs until it closes.\n" +
			"`dialog status` answers from an event the daemon retained when the dialog\n" +
			"opened, without touching the blocked renderer; `dialog accept`/`dismiss` close\n" +
			"it with the one CDP command that still works while the renderer is blocked.\n\n" +
			"Retention only covers a tab the connection was already attached to when the\n" +
			"dialog opened — the daemon does this from the first command that touches a\n" +
			"tab, so the normal flow (navigate, click, timeout, dialog accept) is covered\n" +
			"throughout. `--on-dialog` on click/type/fill is unchanged and handles a dialog\n" +
			"the action itself opens; `dialog` is the remedy once one is already up.\n\n" +
			"  chrome-cdp dialog status                      # {\"open\":true,\"type\":\"confirm\",…}\n" +
			"  chrome-cdp dialog accept                      # confirm() -> true\n" +
			"  chrome-cdp dialog accept \"Quarterly report\"   # prompt() -> that text\n" +
			"  chrome-cdp dialog dismiss                     # confirm() -> false / prompt() -> null",
	}
	c.AddCommand(a.cmdDialogStatus(), a.cmdDialogAccept(), a.cmdDialogDismiss())
	return c
}

func (a *App) cmdDialogStatus() *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "Report the native dialog open on the target tab, or open: false",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			a.runDialog(func(ctx context.Context, b chrome.Browser, id string) (map[string]any, error) {
				return b.DialogStatus(ctx, id)
			})
			return nil
		},
	}
}

func (a *App) cmdDialogAccept() *cobra.Command {
	return &cobra.Command{
		Use:   "accept [text]",
		Short: "Close the dialog as OK would (prompt() -> text, or \"\" when none is given)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			text := ""
			if len(args) > 0 {
				text = args[0]
			}
			a.runDialog(func(ctx context.Context, b chrome.Browser, id string) (map[string]any, error) {
				return b.DialogHandle(ctx, id, true, text)
			})
			return nil
		},
	}
}

func (a *App) cmdDialogDismiss() *cobra.Command {
	return &cobra.Command{
		Use: "dismiss", Short: "Close the dialog as Cancel would (confirm() -> false, prompt() -> null)",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			a.runDialog(func(ctx context.Context, b chrome.Browser, id string) (map[string]any, error) {
				return b.DialogHandle(ctx, id, false, "")
			})
			return nil
		},
	}
}

// runDialog resolves the target and runs a dialog verb, classifying its
// errors per classifyDialogErr. The envelope's command is "dialog" for all
// three subcommands, the way cookie/record/window report their family name.
func (a *App) runDialog(fn func(context.Context, chrome.Browser, string) (map[string]any, error)) {
	ctx, cancel := a.ctx()
	defer cancel()
	tgt, b, rerr := a.resolveTarget(ctx)
	if rerr != nil {
		a.emitErr("dialog", rerr.Code, rerr.Message, rerr.Details)
		return
	}
	res, err := fn(ctx, b, tgt.ID)
	if err != nil {
		code, msg, details := classifyDialogErr(err)
		a.emitErr("dialog", code, msg, details)
		return
	}
	a.emitOK("dialog", tgt, res)
}

// classifyDialogErr maps a dialog failure onto the error contract.
//
// "nothing to accept/dismiss" is target_not_found, not usage: the invocation
// is well-formed, and what is missing is the thing on the page, which is what
// exit 4 means everywhere else (selector not found) — an agent's right
// reaction is "re-read the state", not "fix the command".
func classifyDialogErr(err error) (string, string, map[string]any) {
	if chrome.IsNoDialog(err) {
		return result.CodeTargetNotFound, err.Error(), map[string]any{"dialog": "none"}
	}
	return classifyActionErr(err), err.Error(), nil
}
