package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// cmdStorage is the `storage` verb family (RFC-0019): local|session
// list|get|set|rm|clear, over the DevTools DOMStorage domain, deliberately
// shaped like `cookie` (cmdCookie in commands.go) — subcommands rather than
// flags, positional key and value, the family name as the envelope's command.
//
// The group itself is RUNNABLE, with a RunE that emits `usage` for a bad or
// missing scope, so `storage`, `storage local list` and the like are exit 2
// before Chrome is ever touched. A non-runnable group is cobra's default and
// prints help to stdout with exit 0 (what `cookie foo` still does today) —
// which breaks the one-envelope and exit-code contract at exactly the point a
// typo is likeliest, so this group costs one "storage": Exempt policy row to
// close that hole (see internal/policy/policy.go).
func (a *App) cmdStorage() *cobra.Command {
	storage := &cobra.Command{
		Use:   "storage",
		Short: "Read and write the tab's localStorage / sessionStorage",
		Long: "Read or write the DOM Storage area of the tab's TOP FRAME — localStorage\n" +
			"under `local`, sessionStorage under `session` — over DevTools' DOMStorage\n" +
			"domain. Nothing runs in the page's JavaScript context, so it works on a\n" +
			"hidden tab and is unaffected by a page that has shadowed window.localStorage.\n\n" +
			"`list` redacts credential-shaped keys and values by default, the same\n" +
			"predicates `net` applies to headers, URL parameters and bodies — pass\n" +
			"--no-redact for the raw values. `get <key>` is itself the explicit ask and\n" +
			"always returns the raw, uncut value.\n\n" +
			"  chrome-cdp storage local list                   # keys + redacted values\n" +
			"  chrome-cdp storage local get theme               # {\"value\":\"dark\",\"present\":true}\n" +
			"  chrome-cdp storage local set onboarding_done 1\n" +
			"  chrome-cdp storage session rm draft\n" +
			"  chrome-cdp storage local clear",
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			a.emitErr("storage", result.CodeUsage,
				"storage needs a scope (local|session) and an action (list|get|set|rm|clear)", nil)
			return nil
		},
	}
	storage.AddCommand(a.storageScope("local"), a.storageScope("session"))
	return storage
}

// storageScope builds one scope's five leaves. It is called once per scope so
// the local and session subtrees are built from the same code and cannot
// drift apart.
func (a *App) storageScope(scope string) *cobra.Command {
	c := &cobra.Command{Use: scope, Short: "Read and write " + scope + "Storage"}

	c.AddCommand(
		a.storageListCmd(scope),
		&cobra.Command{
			Use: "get <key>", Short: "Read one value raw and uncut; present:false when the key is absent",
			Args: cobra.ExactArgs(1),
			RunE: a.targetAction("storage", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
				return b.StorageGet(ctx, id, scope, args[0])
			}),
		},
		&cobra.Command{
			Use: "set <key> <value>", Short: "Create or overwrite one key",
			Args: cobra.ExactArgs(2),
			RunE: a.targetAction("storage", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
				return b.StorageSet(ctx, id, scope, args[0], args[1])
			}),
		},
		&cobra.Command{
			Use: "rm <key>", Short: "Remove one key; removing an absent key succeeds",
			Args: cobra.ExactArgs(1),
			RunE: a.targetAction("storage", func(ctx context.Context, b chrome.Browser, id string, args []string) (any, error) {
				return b.StorageRemove(ctx, id, scope, args[0])
			}),
		},
		&cobra.Command{
			Use: "clear", Short: "Remove every key in " + scope + "Storage",
			Args: cobra.NoArgs,
			RunE: a.targetAction("storage", func(ctx context.Context, b chrome.Browser, id string, _ []string) (any, error) {
				return b.StorageClear(ctx, id, scope)
			}),
		},
	)
	return c
}

// storageListCmd builds `list`, whose --max-value is validated BEFORE
// targetAction resolves the target (storageListAction), so a negative cap
// never connects.
func (a *App) storageListCmd(scope string) *cobra.Command {
	var noRedact bool
	var maxValue int
	c := &cobra.Command{
		Use: "list", Short: "List every key in " + scope + "Storage, values redacted and size-capped",
		Args: cobra.NoArgs,
	}
	c.RunE = a.storageListAction(scope, &noRedact, &maxValue)
	c.Flags().BoolVar(&noRedact, "no-redact", false, "do NOT redact credential-shaped keys and values")
	c.Flags().IntVar(&maxValue, "max-value", chrome.DefaultStorageMaxValue, "cut each listed value at this many bytes (0 = no cap)")
	return c
}

// storageListAction validates --max-value before targetAction resolves the
// target, so `storage local list --max-value -1` is usage/exit 2 without ever
// touching Chrome (RFC-0019: "0 means no cap; a negative value is usage").
func (a *App) storageListAction(scope string, noRedact *bool, maxValue *int) func(*cobra.Command, []string) error {
	run := a.targetAction("storage", func(ctx context.Context, b chrome.Browser, id string, _ []string) (any, error) {
		return b.StorageList(ctx, id, scope, chrome.StorageListOpts{NoRedact: *noRedact, MaxValue: *maxValue})
	})
	return func(cmd *cobra.Command, args []string) error {
		if *maxValue < 0 {
			a.emitErr("storage", result.CodeUsage,
				fmt.Sprintf("--max-value must be >= 0 (0 means no cap), got %d", *maxValue), nil)
			return nil
		}
		return run(cmd, args)
	}
}
