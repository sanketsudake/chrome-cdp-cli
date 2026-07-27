package cli

// `chrome-cdp mcp` — serve the verbs to a Model Context Protocol client over
// stdio (RFC-0004).
//
// This file is the wiring, not the protocol: internal/mcp owns the tool
// surface, the schemas and the result mapping, and it drives the CLI through
// the Runner below. A tool call becomes exactly the argv a person would type,
// runs through the same cobra tree against the same connection, and its
// envelope is mapped back — the same re-entrant execution `session` and
// `recipe run` use, for the same reason: one execution path cannot drift from
// itself.
//
// Two things here are security- rather than convenience-shaped:
//
//   - MCP mode REFUSES TO START without a policy allow-list. The CLI's default
//     is unrestricted because a person typing a command has already decided to
//     run it; an MCP server hands an assistant a browser that is signed in to
//     everything, which is a different threat model and deserves a different
//     default (RFC-0004 US-5, VS-9).
//
//   - stdout is the protocol stream and nothing else may touch it. The command
//     tree's writer is redirected for the life of the server, and the process's
//     os.Stdout is pointed at stderr so a library that prints cannot corrupt a
//     frame. One stray byte surfaces as an unexplained client failure a long
//     way from its cause.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/mcp"
	"github.com/sanketsudake/chrome-cdp-cli/internal/policy"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

func (a *App) cmdMCP() *cobra.Command {
	var readOnly bool
	var tools string
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Serve these verbs to an MCP client over stdio (requires a configured policy)",
		Long: "Run as a Model Context Protocol server on stdin/stdout, exposing the verbs as\n" +
			"MCP tools backed by the same Chrome connection and the same result envelope.\n\n" +
			"Add it to a client with one stdio entry:\n\n" +
			"  { \"mcpServers\": { \"chrome-cdp\": { \"command\": \"chrome-cdp\", \"args\": [\"mcp\"] } } }\n\n" +
			"A policy allow-list is REQUIRED: handing an assistant your authenticated\n" +
			"browser should not mean handing it every site you are signed in to. Run\n" +
			"`chrome-cdp policy init` on the tab you want it to drive, then start the\n" +
			"server. --read-only exposes only verbs that cannot modify a page, and\n" +
			"--target pins the server to one tab.\n\n" +
			"stdout carries the protocol; diagnostics go to stderr.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			// Everything that can be refused is refused BEFORE the protocol
			// stream exists, so a client sees a process that failed to start
			// with a readable reason rather than a server that answers every
			// call with an error.
			if perr := a.mcpPolicyGate(); perr != nil {
				a.emitErr("mcp", perr.Code, perr.Message, perr.Details)
				return nil
			}
			runner := a.newMCPRunner()
			srv, err := mcp.New(runner, mcp.Options{
				ReadOnly: readOnly,
				Tools:    tools,
				Target:   a.targetFlag,
				Version:  Version,
				Log:      a.err,
			})
			if err != nil {
				a.emitErr("mcp", result.CodeUsage, err.Error(), nil)
				return nil
			}

			// From here on stdout is protocol. protectStdout hands back the
			// real stream and points everything else at stderr.
			out := a.protectStdout()
			in := a.in
			if in == nil {
				in = os.Stdin
			}
			if a.verbose {
				fmt.Fprintf(a.err, "chrome-cdp mcp: serving %d tool(s) on stdio\n", len(mustSpecs(readOnly, tools)))
			}
			if err := srv.Run(context.Background(), in, out); err != nil && err != io.EOF {
				fmt.Fprintf(a.err, "chrome-cdp mcp: %v\n", err)
				a.exitCode = result.ExitGeneric
				return nil
			}
			a.exitCode = result.ExitOK
			return nil
		},
	}
	c.Flags().BoolVar(&readOnly, "read-only", false, "expose only tools that cannot modify page state")
	c.Flags().StringVar(&tools, "tools", mcp.SetDefault, "tool set: default | full | a comma-separated list of tool names")
	return c
}

func mustSpecs(readOnly bool, tools string) []mcp.ToolSpec {
	specs, _ := mcp.Specs(mcp.Options{ReadOnly: readOnly, Tools: tools})
	return specs
}

// mcpPolicyGate refuses to serve without an origin allow-list.
//
// The allow-list is the boundary this mode is built on, so an unconfigured
// server does not start in a degraded state — it does not start. policy.Config's
// RequireAllow expresses the same rule inside the checker (an empty allow-list
// refuses everything rather than permitting everything), and refusing here as
// well means the user learns why at launch instead of from every tool call.
func (a *App) mcpPolicyGate() *result.Err {
	if a.policyOff {
		return &result.Err{
			Code:    result.CodeUsage,
			Message: "refusing to serve MCP with --policy-off: the policy allow-list is the boundary this mode depends on",
		}
	}
	cfg := a.policyConfig()
	// An MCP server is exactly the caller RequireAllow exists for: with no
	// allow-list, nothing is permitted, so there is nothing to serve.
	cfg.RequireAllow = true

	if m := a.defaults.Policy.Malformed; m != "" {
		return a.policyConfigErr(m, a.defaults.Policy.Source)
	}
	if _, err := policy.New(cfg); err != nil {
		return a.policyConfigErr(err.Error(), cfg.Source)
	}
	if !cfg.Present || !cfg.Enabled || len(cfg.Allow) == 0 {
		a.printPolicyRequirement()
		return &result.Err{
			Code: result.CodeUsage,
			Message: "MCP mode requires a policy allow-list: no [policy] table with a non-empty `allow` is configured. " +
				"Run `chrome-cdp policy init` on the tab you want the server to drive, or pass --allow '<origin>'",
			Details: map[string]any{"config": cfg.Source},
		}
	}
	return nil
}

// printPolicyRequirement shows the configuration the server needs. It goes to
// stderr: the point is that a human reads it, and stdout may be a client's
// protocol pipe.
func (a *App) printPolicyRequirement() {
	if a.quiet {
		return
	}
	fmt.Fprint(a.err, "\nchrome-cdp mcp needs a policy allow-list. Add this to your config file\n"+
		"(`chrome-cdp policy init` writes it for the tab you are on):\n\n"+
		"  [policy]\n"+
		"  enabled = true\n"+
		"  allow = [\"*.example.com\"]      # the origins the assistant may drive\n"+
		"  verbs_denied = [\"eval\", \"raw\"] # they can navigate out of the allow-list\n"+
		"  on_violation = \"error\"\n\n"+
		"…or start the server with a one-off list: chrome-cdp mcp --allow '*.example.com'\n\n")
}

// protectStdout claims the process's stdout for the protocol and points every
// other writer at stderr.
//
// The command tree's own writer is replaced by a strayWriter, which reports
// anything written to it rather than letting it reach a frame; the runner swaps
// in a capture buffer around each command, so in normal operation nothing
// reaches the stray writer at all. os.Stdout is reassigned for the same reason
// one level down: a dependency that prints — now or after an upgrade — lands on
// stderr instead of corrupting the stream.
func (a *App) protectStdout() io.Writer {
	out := a.out
	if f, ok := out.(*os.File); ok && f == os.Stdout {
		os.Stdout = os.Stderr
	}
	a.out = &strayWriter{to: a.err}
	return out
}

// strayWriter turns a write that would have corrupted the protocol into a loud
// diagnostic. It exists because the failure it guards against is silent: a
// truncated frame surfaces in the client as an unexplained disconnect.
type strayWriter struct {
	to io.Writer
	mu sync.Mutex
}

func (w *strayWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.to != nil && len(bytes.TrimSpace(p)) > 0 {
		fmt.Fprintf(w.to, "chrome-cdp mcp: suppressed a write to stdout (stdout carries the protocol): %s\n", bytes.TrimSpace(p))
	}
	return len(p), nil
}

// mcpRunner executes tool calls through the CLI command tree.
//
// It is the whole of the MCP layer's coupling to internal/cli: one argv in, one
// envelope out. Calls are serialized because App holds per-invocation flag
// state and a browser is a single-user resource anyway — which also gives
// `batch` the property RFC-0004 VS-10 asks for, that a batch acquires the
// connection once.
type mcpRunner struct {
	mu  sync.Mutex
	app *App
	// real is the unbound connection, cached across calls. a.browser is only
	// ever left holding this, never a request-bound wrapper, so a later call
	// cannot inherit an earlier request's cancellation.
	real chrome.Browser
	// extra are flags re-applied to every call. Flag variables live on App and
	// are re-registered (and so reset) per Execute, so anything that must
	// survive into each tool call has to travel in the argv — --allow above all,
	// since losing it would silently widen the boundary.
	extra []string
}

// newMCPRunner freezes the server's connection-shaped flags into the defaults
// each tool call is parsed with, and returns the runner.
func (a *App) newMCPRunner() *mcpRunner {
	// Only connection and output settings are propagated. Selector semantics
	// (--by, --role, …) belong to the individual tool call, where the client
	// chose them.
	a.defaults.Timeout = a.timeout
	a.defaults.NoLaunch = a.noLaunch
	a.defaults.NoDaemon = a.noDaemon
	a.defaults.ProfileDir = a.profileDir
	a.defaults.Port = a.port
	a.defaults.JSON = true

	r := &mcpRunner{app: a, real: a.browser}
	for _, p := range a.allowFlag {
		r.extra = append(r.extra, "--allow", p)
	}
	return r
}

// Run executes one argv and returns the envelope it printed with its exit code.
func (r *mcpRunner) Run(ctx context.Context, argv []string) ([]byte, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.app

	// Hand the command tree a view of the connection bound to THIS request, so
	// a client that cancels actually stops the work (RFC-0004 VS-12).
	if r.real == nil {
		r.real = a.browser
	}
	if r.real != nil {
		a.browser = mcp.Bind(r.real, ctx)
	}
	connect := a.connect
	if connect != nil {
		a.connect = func(cctx context.Context, o ConnOpts) (chrome.Browser, error) {
			b, err := connect(cctx, o)
			if err != nil {
				return nil, err
			}
			r.real = b
			return mcp.Bind(b, ctx), nil
		}
	}

	var buf bytes.Buffer
	out := a.out
	a.out = &buf
	exit := a.Execute(append(argv, r.extra...)...)
	a.out = out

	a.connect = connect
	// Never leave a request-bound browser cached: the next call gets the real
	// one and binds it to its own context.
	if r.real != nil {
		a.browser = r.real
	}
	return buf.Bytes(), exit
}
