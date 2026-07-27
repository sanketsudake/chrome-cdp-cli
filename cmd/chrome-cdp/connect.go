package main

import (
	"fmt"
	"io"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/cli"
	"github.com/sanketsudake/chrome-cdp-cli/internal/config"
)

// directConnectOptions builds the chrome.Options for a --no-daemon connect.
//
// It exists as its own function for the hook at the bottom. Options.
// OnConsentPending was only ever assigned by the daemon, so the --no-daemon
// path sat in complete silence for the whole consent budget — up to two
// minutes during which the user's browser is frozen, the tool has printed
// nothing, and RFC-0013's US-2 ("tell me what is happening") is unmet on the
// one path where nothing else can tell them: there is no daemon to publish a
// .pending marker and no Ensure to read it.
func directConnectOptions(portFile string, o cli.ConnOpts, defs config.Defaults, w io.Writer) chrome.Options {
	return chrome.Options{
		PortFile: portFile, NoLaunch: o.NoLaunch, ProfileDir: o.ProfileDir, Port: o.Port,
		ConsentTimeout: o.ConsentTimeout,
		// Fires once, the moment the upgrade is classified as pending — while
		// the dialog is still on screen, which is the only time saying so helps.
		OnConsentPending: func() {
			fmt.Fprintln(w, "chrome-cdp:", browser.ConsentPromptAdvice)
		},
		ConsoleBuffer: defs.ConsoleBuffer, ConsoleMaxEntry: defs.ConsoleMaxEntry,
		NetBuffer: defs.NetBuffer, NetMaxBody: defs.NetMaxBody,
		RecordBuffer: defs.RecordBuffer, RecordMaxBytes: defs.RecordMaxBytes,
	}
}
