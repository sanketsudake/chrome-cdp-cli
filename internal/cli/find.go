package cli

// The `find` verb (RFC-0015): ranked element search from a plain-language
// query. A read verb — it produces addresses (refs, centre points) for the
// acting verbs to consume, and never dispatches input.

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

func (a *App) cmdFind() *cobra.Command {
	var role, region string
	var limit int
	var all, dedupe bool
	var minScore float64
	c := &cobra.Command{
		Use:   "find <query>",
		Short: "Ranked element search from a plain-language query (\"login button\")",
		Long: "Find elements by describing them: a short query like \"login button\" or\n" +
			"\"search bar\" is matched against the accessibility tree — token overlap,\n" +
			"role words, visibility — and the best matches come back ranked, each with\n" +
			"its ref (for --by ref), exact accessible name, states, and centre point.\n\n" +
			"Matching is a deterministic heuristic, not a model: it handles descriptive\n" +
			"queries, not paraphrase. Role words (button, link, field, box, bar, checkbox,\n" +
			"tab, menu, heading, row, icon) softly steer the ranking; --role is the hard\n" +
			"filter. Finding nothing is an answer, not an error: count 0, exit 0.\n\n" +
			"  chrome-cdp find \"login button\"\n" +
			"  chrome-cdp find \"delete\" --region \"Invoice 4102\" --role button\n" +
			"  chrome-cdp find \"time type\" --role textbox --limit 3",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			// Validation runs BEFORE resolveTarget/getBrowser, so a malformed
			// invocation is exit 2 with Chrome never contacted.
			query := strings.TrimSpace(args[0])
			if msg := validateFindFlags(query, limit, minScore); msg != "" {
				a.emitErr("find", result.CodeUsage, msg, nil)
				return nil
			}
			a.runResolved("find", func(ctx context.Context, b chrome.Browser, id string) (any, error) {
				return b.Find(ctx, id, query, chrome.FindOpts{
					Role:     role,
					Limit:    limit,
					Region:   region,
					All:      all,
					Dedupe:   dedupe,
					MinScore: minScore,
				})
			})
			return nil
		},
	}
	c.Flags().StringVar(&role, "role", "", "hard-filter matches to this ARIA role (button|link|textbox|…)")
	c.Flags().IntVar(&limit, "limit", chrome.DefaultFindLimit, fmt.Sprintf("maximum matches returned (1..%d)", chrome.MaxFindLimit))
	c.Flags().StringVar(&region, "region", "", "only elements within a container whose name contains this")
	c.Flags().BoolVar(&all, "all", false, "include off-screen and ignored nodes (ranked lower)")
	c.Flags().BoolVar(&dedupe, "dedupe", false, "collapse identical role+name matches (for virtualized grids)")
	c.Flags().Float64Var(&minScore, "min-score", 0, "drop matches scoring below this (0..1)")
	return c
}

// validateFindFlags returns the usage message for a malformed `find`
// invocation, or "" when it is well-formed. Pure, so the whole table is
// testable without a browser.
func validateFindFlags(query string, limit int, minScore float64) string {
	switch {
	case query == "":
		return "find needs a query — a few words describing the element (\"login button\", \"search bar\")"
	case limit < 1 || limit > chrome.MaxFindLimit:
		return fmt.Sprintf("--limit must be between 1 and %d (got %d)", chrome.MaxFindLimit, limit)
	case minScore < 0 || minScore > 1:
		return fmt.Sprintf("--min-score must be between 0 and 1 (got %g)", minScore)
	}
	return ""
}
