package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

func (a *App) cmdSession() *cobra.Command {
	// The recording flags are locals on this command, like every other flag:
	// the loop below re-enters the whole command tree per line, and each of
	// those re-entries rebuilds `session` with fresh locals of its own.
	var rf sessionRecordFlags
	c := &cobra.Command{
		Use:   "session",
		Short: "Batch mode: read NDJSON argv commands on stdin, run each over one held connection, emit NDJSON results",
		Long: "Read one command per stdin line as a JSON array of argv, run it against a\n" +
			"single held Chrome connection (no per-command process spawn or reconnect),\n" +
			"and print each result as one JSON line. Comment lines (#) and blank lines are\n" +
			"skipped. Combine with `snap`'s element refs and `--by ref` to act on nodes\n" +
			"without re-resolving them:\n\n" +
			"  printf '%s\\n' '[\"use\",\"url:workday\"]' '[\"snap\"]' '[\"click\",\"e42\",\"--by\",\"ref\"]' | chrome-cdp session\n\n" +
			"--record wraps the whole batch in a recording (RFC-0011) and writes it when\n" +
			"stdin is drained — including when a step failed, which is when a recording is\n" +
			"worth the most. It emits one extra result line describing the file.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// `session` is Exempt — it touches no tab itself and every line it
			// re-enters is checked on its own — but verbs_denied is checked
			// ahead of the class precisely so an operator can name it, and
			// "read commands from stdin and run them" is an obvious thing to
			// want off. Without this call site the checker refused it correctly
			// and nothing ever asked, so the rule was accepted and inert.
			if perr := a.checkPolicy(a.policyVerb(), ""); perr != nil {
				a.emitErr("session", perr.Code, perr.Message, perr.Details)
				return nil
			}
			// Validated before anything connects, so a misspelled --record path
			// is exit 2 with Chrome untouched and stdin unread.
			rec, rerr := a.newSessionRecorder(cmd, rf)
			if rerr != nil {
				a.emitErr("session", rerr.Code, rerr.Message, rerr.Details)
				return nil
			}
			// Flags are re-registered per Execute — each stdin line below is a
			// fresh a.Execute(argv) that rebuilds the command tree via newRoot,
			// which re-registers every persistent flag with a.defaults as its
			// default. A line with no --session/--port/--endpoint of its own
			// (every real line) would otherwise silently reset to the config
			// default on line 1 already, the same failure runPlan's freeze
			// (recipe.go) exists to prevent for `recipe run`. So the connection-
			// shaped flags this invocation was actually given are folded into
			// a.defaults for the duration of the batch, and restored after —
			// the defaults are BORROWED, not changed, the same way runPlan
			// borrows and restores them.
			//
			restore := a.freezeConnDefaults()
			defer restore()
			// inSession marks the re-entrant runs below, so a streaming verb
			// rejects itself rather than interleaving many lines into a batch
			// that promises one envelope per command.
			a.inSession = true
			r := a.in
			if r == nil {
				r = os.Stdin
			}
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // tolerate long lines
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				var argv []string
				if err := json.Unmarshal([]byte(line), &argv); err != nil {
					a.emitErr("session", result.CodeUsage, "each line must be a JSON array of argv strings: "+err.Error(), nil)
					continue
				}
				if len(argv) == 0 {
					continue
				}
				// The recording starts as soon as a target exists. A batch whose
				// first line is `use` has none yet, so this retries rather than
				// refusing to record the run at all.
				rec.ensureStarted()
				// Reuse the full command tree per line; the browser connection is
				// cached on the App, so only the first line pays the connect cost.
				a.Execute(argv...)
			}
			if err := sc.Err(); err != nil {
				rec.finish()
				a.emitErr("session", result.CodeGeneric, err.Error(), nil)
				return nil
			}
			// Stopping is the LAST thing, after every line ran, so a batch that
			// failed half way still has the failure on film.
			rec.finish()
			// The session itself succeeded (stdin drained cleanly); per-line success
			// or failure is carried in each NDJSON envelope, not the process exit.
			a.exitCode = result.ExitOK
			return nil
		},
	}
	rf.register(c)
	return c
}
