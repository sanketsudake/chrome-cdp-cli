package cli

// Tests that config-resolved defaults (via WithDefaults) feed the global flag
// defaults, and that an explicit flag still overrides them.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/config"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

func clickCapture(t *testing.T, defs config.Defaults, args ...string) *queryCapture {
	t.Helper()
	b := &queryCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}}
	var out, errb bytes.Buffer
	app := New(b, &out, &errb).WithDefaults(defs)
	if code := app.Execute(args...); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	return b
}

func TestConfigDefaultsFeedFlagDefaults(t *testing.T) {
	t.Parallel()
	defs := config.Defaults{By: "search", Wait: "ready", Timeout: 5 * time.Second}
	// No --by/--wait on the command line, so the config defaults take effect.
	b := clickCapture(t, defs, "click", "#x", "--target", "aa11", "--json")
	if b.gotQ.By != "search" || b.gotQ.Wait != "ready" {
		t.Errorf("config defaults did not feed flag defaults: %+v", b.gotQ)
	}
}

func TestExplicitFlagOverridesConfigDefault(t *testing.T) {
	t.Parallel()
	defs := config.Defaults{By: "search", Wait: "ready", Timeout: 5 * time.Second}
	// An explicit --by must win over the config default.
	b := clickCapture(t, defs, "click", "#x", "--target", "aa11", "--by", "id", "--json")
	if b.gotQ.By != "id" {
		t.Errorf("explicit --by should override config default, got %q", b.gotQ.By)
	}
	if b.gotQ.Wait != "ready" {
		t.Errorf("unset --wait should keep config default, got %q", b.gotQ.Wait)
	}
}

func TestConfigTargetIsFallback(t *testing.T) {
	t.Parallel()
	// With no --target and no sticky target, the config default target resolves.
	defs := config.Defaults{By: "css", Wait: "visible", Timeout: 5 * time.Second, Target: "@1"}
	b := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	var out, errb bytes.Buffer
	app := New(b, &out, &errb).WithDefaults(defs)
	if code := app.Execute("eval", "1+1", "--json"); code != 0 {
		t.Fatalf("config target should resolve @1, exit=%d stderr=%s", code, errb.String())
	}

	// An explicit --target still overrides the config default.
	defsBad := config.Defaults{By: "css", Wait: "visible", Timeout: 5 * time.Second, Target: "nope-no-such"}
	b2 := &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}
	var o2, e2 bytes.Buffer
	app2 := New(b2, &o2, &e2).WithDefaults(defsBad)
	if code := app2.Execute("eval", "1+1", "--target", "aa11", "--json"); code != 0 {
		t.Fatalf("explicit --target should override config target, exit=%d stderr=%s", code, e2.String())
	}
}

// TestConsentTimeoutReachesTheConnector pins the last link of RFC-0013's
// precedence chain: the resolved value has to arrive at the connector, because
// the connector is what hands it to the daemon that does the waiting. A key that
// resolves correctly and is then dropped on the floor is the same as no key.
func TestConsentTimeoutReachesTheConnector(t *testing.T) {
	t.Parallel()
	capture := func(args ...string) time.Duration {
		t.Helper()
		var got ConnOpts
		var out, errb bytes.Buffer
		app := New(nil, &out, &errb).
			WithDefaults(config.Defaults{By: "css", Wait: "visible", Timeout: 5 * time.Second, ConsentTimeout: 90 * time.Second}).
			WithConnector(func(_ context.Context, o ConnOpts) (chrome.Browser, error) {
				got = o
				return &fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}, nil
			})
		if code := app.Execute(args...); code != 0 {
			t.Fatalf("exit = %d, stderr=%s", code, errb.String())
		}
		return got.ConsentTimeout
	}

	if got := capture("list", "--json"); got != 90*time.Second {
		t.Errorf("resolved consent_timeout = %v, want the config value 90s", got)
	}
	if got := capture("list", "--json", "--consent-timeout", "10s"); got != 10*time.Second {
		t.Errorf("explicit --consent-timeout = %v, want 10s (a flag must beat the file)", got)
	}
}
