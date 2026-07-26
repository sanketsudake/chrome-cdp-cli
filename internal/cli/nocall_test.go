package cli

import (
	"context"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// noCallBrowser fails the test if the CLI reaches for Chrome at all.
//
// It enforces the contract that argument and flag validation happens BEFORE
// resolveTarget/getBrowser, so a malformed invocation is usage/exit 2 without
// launching or touching the user's browser. Asserting only on the exit code
// would also pass for a command that connected first and validated afterwards,
// which is the bug this catches: agents rely on exit 2 meaning "your call was
// wrong, don't retry", and a connection attempt means a consent prompt the user
// never should have seen.
//
// resolveTarget calls List before anything else, so failing there catches every
// verb that takes a target.
type noCallBrowser struct {
	stubBrowser
	t *testing.T
}

func (b *noCallBrowser) List(context.Context) ([]target.Info, error) {
	b.t.Helper()
	b.t.Fatal("the browser was contacted for an invocation that should have failed validation first")
	return nil, nil
}

// noCall returns a Browser that fails t if the CLI tries to use it.
func noCall(t *testing.T) *noCallBrowser {
	t.Helper()
	return &noCallBrowser{t: t}
}

var _ chrome.Browser = (*noCallBrowser)(nil)
