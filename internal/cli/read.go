package cli

// The page-reading verbs: `text` (optionally --article) and `eval` (optionally
// --await), per RFC-0010.

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

func (a *App) cmdEval() *cobra.Command {
	var await bool
	c := &cobra.Command{
		Use:   "eval <js>",
		Short: "Evaluate JS in the target tab (--await for REPL semantics)",
		Long: "Evaluate JavaScript in the target tab.\n\n" +
			"By default the argument is evaluated as an expression, which is why a\n" +
			"top-level `await` is a syntax error and a statement list has to be wrapped\n" +
			"in an IIFE. --await switches to the semantics of DevTools' own console:\n" +
			"top-level await resolves, and the last expression's value is returned\n" +
			"without a `return`.\n\n" +
			"  chrome-cdp eval --await 'await fetch(\"/api/me\").then(r => r.json())'\n" +
			"  chrome-cdp eval --await 'const rows = [...document.querySelectorAll(\"tr\")]; rows.length'\n\n" +
			"An awaited promise that REJECTS is an error (exit 5), never a value.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			a.runEval(args[0], chrome.EvalOpts{Await: await})
			return nil
		},
	}
	c.Flags().BoolVar(&await, "await", false, "REPL semantics: top-level `await` resolves and the last expression's value is returned")
	return c
}

// runEval resolves the target, evaluates, and emits.
//
// It does not go through targetAction because a JavaScript exception carries a
// stack that belongs in error.details — and mapping a rejected promise onto a
// successful-looking envelope is the one outcome RFC-0010 calls out as the worst
// possible. Everything else (timeouts, connection failures) classifies exactly
// as any other verb.
func (a *App) runEval(expr string, opts chrome.EvalOpts) {
	ctx, cancel := a.ctx()
	defer cancel()
	tgt, b, rerr := a.resolveTarget(ctx)
	if rerr != nil {
		a.emitErr("eval", rerr.Code, rerr.Message, nil)
		return
	}
	res, err := b.Eval(ctx, tgt.ID, expr, opts)
	if err != nil {
		if js, ok := chrome.JSException(err); ok {
			details := map[string]any{}
			if js.Stack != "" {
				details["stack"] = js.Stack
			}
			if opts.Await {
				details["awaited"] = true
			}
			a.emitErr("eval", result.CodeCDP, js.Message, details)
			return
		}
		code, msg, details := a.classifyWithTabHint(b, tgt.ID, err)
		a.emitErr("eval", code, msg, details)
		return
	}
	a.emitOK("eval", tgt, res)
}

func (a *App) cmdText() *cobra.Command {
	var article, markdown bool
	var minChars int
	c := &cobra.Command{
		Use:   "text [selector]",
		Short: "Visible text of a selector, or the page's main content (--article)",
		Long: "Read text out of the page.\n\n" +
			"With a selector, this is the selector's visible text. With --article, it is\n" +
			"the page's main readable content — navigation, headers, footers, asides, and\n" +
			"cookie banners dropped — scored the way Reader Mode scores it.\n\n" +
			"Extraction is a heuristic, so the envelope reports what it kept: `chars`,\n" +
			"`total_chars`, and `ratio`. When it keeps less than --min-chars it says so\n" +
			"(`extracted: false`, plus a `reason`) and returns the FULL page text with\n" +
			"exit 0, rather than handing back a plausible-looking fragment.\n\n" +
			"--markdown keeps headings, lists, links, code blocks, and blockquotes. It is\n" +
			"deliberately not a general HTML-to-markdown converter: tables, footnotes,\n" +
			"and embedded media are out of scope and come through as plain text.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validation runs BEFORE resolveTarget/getBrowser, so a malformed
			// invocation is exit 2 with Chrome never contacted.
			if msg := validateTextFlags(article, markdown, cmd.Flags().Changed("min-chars"), minChars, len(args)); msg != "" {
				a.emitErr("text", result.CodeUsage, msg, nil)
				return nil
			}
			sel := ""
			if len(args) == 1 {
				sel = args[0]
			}
			a.runResolved("text", func(ctx context.Context, b chrome.Browser, id string) (any, error) {
				return b.Text(ctx, id, sel, chrome.TextOpts{
					Article:  article,
					Markdown: markdown,
					MinChars: minChars,
					Query:    a.queryOpts(),
				})
			})
			return nil
		},
	}
	c.Flags().BoolVar(&article, "article", false, "extract the page's main readable content, dropping navigation and boilerplate")
	c.Flags().BoolVar(&markdown, "markdown", false, "with --article: preserve headings, lists, links, code blocks, and blockquotes as markdown")
	c.Flags().IntVar(&minChars, "min-chars", chrome.DefaultArticleMinChars, "with --article: below this many extracted characters, report extracted:false and return the full text")
	return c
}

// validateTextFlags returns the usage message for a malformed `text`
// invocation, or "" when it is well-formed. It is a pure function so the whole
// table is testable without a browser, and so the check can run before any
// connection is attempted.
//
// --article with a selector stays an error on purpose (RFC-0010 Open Question
// 3): "extract the main content, but only inside this subtree" has no clear
// semantics yet, and allowing it now would fix the wrong one.
func validateTextFlags(article, markdown, minCharsSet bool, minChars, nargs int) string {
	switch {
	case markdown && !article:
		return "--markdown applies to --article only"
	case minCharsSet && !article:
		return "--min-chars applies to --article only"
	case article && nargs > 0:
		return "--article extracts the page's main content and takes no selector; drop one or the other"
	case minCharsSet && minChars < 0:
		return fmt.Sprintf("--min-chars must be zero or greater (got %d)", minChars)
	case !article && nargs == 0:
		return "text needs a selector, or --article to extract the page's main content"
	}
	return ""
}
