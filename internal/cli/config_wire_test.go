package cli

// Tests that config-resolved defaults (via WithDefaults) feed the global flag
// defaults, and that an explicit flag still overrides them.

import (
	"bytes"
	"testing"
	"time"

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
	defs := config.Defaults{By: "search", Wait: "ready", Timeout: 5 * time.Second}
	// No --by/--wait on the command line, so the config defaults take effect.
	b := clickCapture(t, defs, "click", "#x", "--target", "aa11", "--json")
	if b.gotQ.By != "search" || b.gotQ.Wait != "ready" {
		t.Errorf("config defaults did not feed flag defaults: %+v", b.gotQ)
	}
}

func TestExplicitFlagOverridesConfigDefault(t *testing.T) {
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
