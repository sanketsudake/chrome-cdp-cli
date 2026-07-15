// Package chrome connects to Chrome over CDP (via chromedp) and exposes the
// Browser port the CLI commands drive. Keeping commands behind this interface
// lets the command boundary be tested in-process with a fake.
package chrome

import (
	"context"
	"encoding/json"

	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// Browser is the set of Chrome operations the CLI commands need. The real
// implementation is CDP (chromedp-backed); tests use a fake.
type Browser interface {
	List(ctx context.Context) ([]target.Info, error)
	Navigate(ctx context.Context, targetID, url string) (map[string]any, error)
	Eval(ctx context.Context, targetID, expr string) (any, error)
	Snapshot(ctx context.Context, targetID string) (any, error)
	Click(ctx context.Context, targetID, selector string) (map[string]any, error)
	Type(ctx context.Context, targetID, selector, text string) (map[string]any, error)
	Screenshot(ctx context.Context, targetID string) ([]byte, error)
	Raw(ctx context.Context, targetID, method string, params json.RawMessage) (any, error)
	Close() error
}
