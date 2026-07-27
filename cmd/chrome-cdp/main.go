// Command chrome-cdp drives the user's local Chrome over CDP.
package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"strconv"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/cli"
	"github.com/sanketsudake/chrome-cdp-cli/internal/config"
	"github.com/sanketsudake/chrome-cdp-cli/internal/daemon"
	"github.com/sanketsudake/chrome-cdp-cli/internal/state"
)

func main() {
	portFile := browser.FindPortFile("")

	// Hidden daemon mode: `chrome-cdp __daemon <socket>` holds the CDP
	// connection and serves commands; its connection options come from env.
	if len(os.Args) >= 3 && os.Args[1] == "__daemon" {
		// The parent handed us a fully-resolved environment; parse it the same
		// way the CLI does (config.FromEnv), not with a second ad-hoc contract.
		env := config.FromEnv()
		opts := chrome.Options{
			PortFile:        os.Getenv("CHROME_CDP_PORT_FILE"),
			ProfileDir:      env.ProfileDir,
			Port:            env.Port,
			NoLaunch:        env.NoLaunch,
			ConsentTimeout:  env.ConsentTimeout,
			ConsoleBuffer:   env.ConsoleBuffer,
			ConsoleMaxEntry: env.ConsoleMaxEntry,
			NetBuffer:       env.NetBuffer,
			NetMaxBody:      env.NetMaxBody,
			RecordBuffer:    env.RecordBuffer,
			RecordMaxBytes:  env.RecordMaxBytes,
		}
		if err := daemon.RunDaemon(os.Args[2], opts, 30*time.Minute); err != nil {
			fmt.Fprintln(os.Stderr, "chrome-cdp daemon:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	exe, _ := os.Executable()

	// The endpoint key (and thus the daemon socket + sticky-state file) depends
	// on the effective --port, which isn't known until cobra parses flags, so it
	// must be computed per command from the connection options — not once here.
	socketFor := func(o cli.ConnOpts) string {
		return daemon.SocketPath(browser.EndpointKey(portFile, o.Port))
	}
	stateFor := func(o cli.ConnOpts) (*state.Store, error) {
		return state.New(browser.EndpointKey(portFile, o.Port))
	}

	// Resolve persistent defaults (config file + CHROME_CDP_* env); a malformed
	// config is a warning, not fatal — the CLI runs on built-ins + env.
	defs, cfgErr := config.Resolve()
	if cfgErr != nil {
		fmt.Fprintln(os.Stderr, "chrome-cdp: ignoring config:", cfgErr)
	}
	// XDG_CONFIG_HOME selects WHICH config file is read, so an environment that
	// points it at a directory without one silently drops the [policy] table too.
	// Say so; a boundary that vanishes without a word is worse than none.
	if note := config.Note(); note != "" {
		fmt.Fprintln(os.Stderr, note)
	}

	app := cli.New(nil, os.Stdout, os.Stderr).WithDefaults(defs)

	app.WithStickyTarget(
		func(o cli.ConnOpts) string {
			st, err := stateFor(o)
			if err != nil {
				return ""
			}
			return st.CurrentTarget()
		},
		func(o cli.ConnOpts, spec string) error {
			st, err := stateFor(o)
			if err != nil {
				return err
			}
			return st.SetCurrentTarget(spec)
		},
	)

	daemonEnv := func(o cli.ConnOpts) []string {
		env := os.Environ()
		if portFile != "" {
			env = append(env, "CHROME_CDP_PORT_FILE="+portFile)
		}
		if o.ProfileDir != "" {
			env = append(env, "CHROME_CDP_PROFILE="+o.ProfileDir)
		}
		if o.Port != 0 {
			env = append(env, "CHROME_CDP_PORT="+strconv.Itoa(o.Port))
		}
		if o.NoLaunch {
			env = append(env, "CHROME_CDP_NO_LAUNCH=1")
		}
		// The daemon is the process that actually waits out the consent prompt,
		// so --consent-timeout has to reach it; it only ever parses the
		// environment.
		if o.ConsentTimeout > 0 {
			env = append(env, "CHROME_CDP_CONSENT_TIMEOUT="+o.ConsentTimeout.String())
		}
		// The daemon parses only the environment, so config-file values for the
		// event-capture bounds have to be forwarded explicitly or the buffers it
		// holds would silently fall back to the built-in sizes.
		env = append(env,
			"CHROME_CDP_CONSOLE_BUFFER="+strconv.Itoa(defs.ConsoleBuffer),
			"CHROME_CDP_CONSOLE_MAX_ENTRY="+strconv.Itoa(defs.ConsoleMaxEntry),
			"CHROME_CDP_NET_BUFFER="+strconv.Itoa(defs.NetBuffer),
			"CHROME_CDP_NET_MAX_BODY="+strconv.Itoa(defs.NetMaxBody),
			"CHROME_CDP_RECORD_BUFFER="+strconv.Itoa(defs.RecordBuffer),
			"CHROME_CDP_RECORD_MAX_BYTES="+strconv.Itoa(defs.RecordMaxBytes),
		)
		return env
	}

	app.WithConnector(func(ctx context.Context, o cli.ConnOpts) (chrome.Browser, error) {
		if o.NoDaemon {
			return chrome.Connect(ctx, chrome.Options{
				PortFile: portFile, NoLaunch: o.NoLaunch, ProfileDir: o.ProfileDir, Port: o.Port,
				ConsentTimeout: o.ConsentTimeout,
				ConsoleBuffer:  defs.ConsoleBuffer, ConsoleMaxEntry: defs.ConsoleMaxEntry,
				NetBuffer: defs.NetBuffer, NetMaxBody: defs.NetMaxBody,
				RecordBuffer: defs.RecordBuffer, RecordMaxBytes: defs.RecordMaxBytes,
			})
		}
		client, err := daemon.Ensure(socketFor(o), exe, daemonEnv(o), o.ConsentTimeout)
		if err != nil {
			return nil, err
		}
		return daemon.Remote(client), nil
	})

	app.WithDaemonCtl(
		func(o cli.ConnOpts) (map[string]any, error) {
			sock := socketFor(o)
			if _, err := daemon.Ensure(sock, exe, daemonEnv(o), o.ConsentTimeout); err != nil {
				return nil, err
			}
			return map[string]any{"started": true, "socket": sock, "endpoint": browser.EndpointKey(portFile, o.Port)}, nil
		},
		func(o cli.ConnOpts) (map[string]any, error) {
			c := daemon.TryConnect(socketFor(o))
			if c == nil {
				return map[string]any{"running": false}, nil
			}
			if err := c.Stop(); err != nil {
				return nil, err
			}
			return map[string]any{"stopped": true}, nil
		},
		func(o cli.ConnOpts) (map[string]any, error) {
			return daemonStatus(socketFor(o), browser.EndpointKey(portFile, o.Port))
		},
	)

	code := app.Execute(os.Args[1:]...)
	app.Close()
	os.Exit(code)
}

// daemonStatus reports whether the daemon for this endpoint is running and, when
// it is, what it's attached to (the live tab list, best-effort).
func daemonStatus(sock, endpoint string) (map[string]any, error) {
	res := map[string]any{"socket": sock, "endpoint": endpoint}
	c := daemon.TryConnect(sock)
	if c == nil {
		res["running"] = false
		return res, nil
	}
	res["running"] = true
	if info, err := c.StatusInfo(); err == nil {
		maps.Copy(res, info)
	}
	return res, nil
}
