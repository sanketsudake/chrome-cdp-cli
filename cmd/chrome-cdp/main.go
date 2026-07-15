// Command chrome-cdp drives the user's local Chrome over CDP.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/sanketsudake/chrome-cdp-cli/internal/browser"
	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/cli"
	"github.com/sanketsudake/chrome-cdp-cli/internal/daemon"
	"github.com/sanketsudake/chrome-cdp-cli/internal/state"
)

func main() {
	portFile := browser.FindPortFile("")

	// Hidden daemon mode: `chrome-cdp __daemon <socket>` holds the CDP
	// connection and serves commands; its connection options come from env.
	if len(os.Args) >= 3 && os.Args[1] == "__daemon" {
		opts := chrome.Options{
			PortFile:   os.Getenv("CHROME_CDP_PORT_FILE"),
			ProfileDir: os.Getenv("CHROME_CDP_PROFILE"),
			Port:       atoi(os.Getenv("CHROME_CDP_PORT")),
			NoLaunch:   os.Getenv("CHROME_CDP_NO_LAUNCH") == "1",
		}
		if err := daemon.RunDaemon(os.Args[2], opts, 30*time.Minute); err != nil {
			fmt.Fprintln(os.Stderr, "chrome-cdp daemon:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	key := endpointKey(portFile)
	sock := daemon.SocketPath(key)
	exe, _ := os.Executable()

	app := cli.New(nil, os.Stdout, os.Stderr)

	if st, err := state.New(key); err == nil {
		app.WithStickyTarget(st.CurrentTarget, st.SetCurrentTarget)
	}

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
		return env
	}

	app.WithConnector(func(ctx context.Context, o cli.ConnOpts) (chrome.Browser, error) {
		if o.NoDaemon {
			return chrome.Connect(ctx, chrome.Options{PortFile: portFile, NoLaunch: o.NoLaunch, ProfileDir: o.ProfileDir, Port: o.Port})
		}
		client, err := daemon.Ensure(sock, exe, daemonEnv(o))
		if err != nil {
			return nil, err
		}
		return daemon.Remote(client), nil
	})

	app.WithDaemonCtl(
		func(o cli.ConnOpts) (map[string]any, error) {
			if _, err := daemon.Ensure(sock, exe, daemonEnv(o)); err != nil {
				return nil, err
			}
			return map[string]any{"started": true, "socket": sock}, nil
		},
		func() (map[string]any, error) {
			c := daemon.TryConnect(sock)
			if c == nil {
				return map[string]any{"running": false}, nil
			}
			if err := c.Stop(); err != nil {
				return nil, err
			}
			return map[string]any{"stopped": true}, nil
		},
		func() (map[string]any, error) {
			return map[string]any{"running": daemon.TryConnect(sock) != nil, "socket": sock}, nil
		},
	)

	code := app.Execute(os.Args[1:]...)
	app.Close()
	os.Exit(code)
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// endpointKey identifies the debug endpoint (host:port) so the daemon socket and
// sticky target are keyed to the actual Chrome instance, not a fixed port.
func endpointKey(portFile string) string {
	if portFile != "" {
		if ws, err := browser.WSURLFromPortFile(portFile); err == nil {
			if hp, ok := browser.HostPort(ws); ok {
				return hp
			}
		}
	}
	return "default"
}
