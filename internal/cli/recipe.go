package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sanketsudake/chrome-cdp-cli/internal/config"
	"github.com/sanketsudake/chrome-cdp-cli/internal/recipe"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// cmdRecipe wires the recipe verbs. A recipe is a `session` script with a
// header, and the runner keeps it that way: it resolves the file into argv
// lines and feeds them through the same command tree `session` re-enters, so
// there is exactly one execution path for both.
func (a *App) cmdRecipe() *cobra.Command {
	c := &cobra.Command{
		Use:   "recipe",
		Short: "Run, list, and scaffold saved automation scripts (parameterised `session` scripts)",
		Long: "A recipe is a YAML file whose steps are argv arrays — exactly the lines\n" +
			"`session` reads on stdin — with declared inputs substituted into argv\n" +
			"elements. There is no shell in this format, so a recipe someone sent you\n" +
			"can be reviewed by reading it: `recipe show <name>` prints the source and\n" +
			"`recipe run <name> --dry-run` prints the exact commands it would run.\n\n" +
			"Recipes resolve from ./.chrome-cdp/recipes, then\n" +
			"$XDG_CONFIG_HOME/chrome-cdp/recipes, then --dir; the first match wins, so a\n" +
			"recipe committed to a repo beats a personal one of the same name.",
	}
	c.AddCommand(a.cmdRecipeList(), a.cmdRecipeShow(), a.cmdRecipeNew(), a.cmdRecipeRun())
	return c
}

// recipeDirs builds the search path: project-local, then the user config dir,
// then --dir. The config dir is derived from the same location config.toml
// lives in, so recipes follow the CLI's existing $XDG_CONFIG_HOME convention.
func recipeDirs(extra string) []recipe.Dir {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}
	userDir := ""
	if p := config.Path(); p != "" {
		userDir = filepath.Dir(p)
	}
	return recipe.SearchPath(cwd, userDir, extra)
}

// loadRecipe finds and validates a recipe by name, emitting the usage error
// itself so callers stay a straight line. Every static failure is exit 2.
func (a *App) loadRecipe(name, dir string) (*recipe.Recipe, bool) {
	path, source, err := recipe.Find(name, recipeDirs(dir))
	if err != nil {
		a.emitErr("recipe", result.CodeUsage, err.Error(), nil)
		return nil, false
	}
	r, err := recipe.Load(path, source)
	if err != nil {
		a.emitErr("recipe", result.CodeUsage, err.Error(), map[string]any{"path": path})
		return nil, false
	}
	return r, true
}

func (a *App) cmdRecipeList() *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Args: cobra.NoArgs,
		Short: "List available recipes with their descriptions, inputs, and source",
		RunE: func(*cobra.Command, []string) error {
			entries := recipe.List(recipeDirs(dir))
			rows := make([]map[string]any, 0, len(entries))
			for _, e := range entries {
				row := map[string]any{"name": e.Name, "source": e.Source, "path": e.Path}
				if e.Err != nil {
					// A malformed file is reported in place rather than
					// failing the listing: one bad recipe must not hide the
					// rest of them.
					row["error"] = e.Err.Error()
				} else {
					row["description"] = e.Recipe.Description
					row["inputs"] = e.Recipe.Inputs
					if e.Recipe.Target != "" {
						row["target"] = e.Recipe.Target
					}
					row["steps"] = len(e.Recipe.Steps)
				}
				rows = append(rows, row)
			}
			if a.jsonOut {
				a.emitOK("recipe", nil, map[string]any{"recipes": rows, "count": len(rows)})
				return nil
			}
			a.renderRecipeList(entries)
			a.exitCode = result.ExitOK
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "additional directory to search for recipes")
	return c
}

func (a *App) renderRecipeList(entries []recipe.Entry) {
	if len(entries) == 0 {
		fmt.Fprintf(a.err, "no recipes found in %s\n", strings.Join(dirPaths(recipeDirs("")), ", "))
		return
	}
	width := 4
	for _, e := range entries {
		if len(e.Name) > width {
			width = len(e.Name)
		}
	}
	for _, e := range entries {
		if e.Err != nil {
			fmt.Fprintf(a.out, "%-*s  %-7s  (invalid: %s)\n", width, e.Name, e.Source, e.Err)
			continue
		}
		line := fmt.Sprintf("%-*s  %-7s  %s", width, e.Name, e.Source, e.Recipe.Description)
		if in := describeInputs(e.Recipe); in != "" {
			line += "  [" + in + "]"
		}
		fmt.Fprintln(a.out, strings.TrimRight(line, " "))
	}
}

func dirPaths(dirs []recipe.Dir) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, d.Path)
	}
	return out
}

// describeInputs renders a recipe's inputs as `week*, hours=8`, where `*` marks
// a required input — the two things a caller needs before running it.
func describeInputs(r *recipe.Recipe) string {
	names := r.InputNames()
	parts := make([]string, 0, len(names))
	for _, n := range names {
		in := r.Inputs[n]
		switch {
		case in.Required:
			parts = append(parts, n+"*")
		case in.Default != "":
			parts = append(parts, n+"="+in.Default)
		default:
			parts = append(parts, n)
		}
	}
	return strings.Join(parts, ", ")
}

func (a *App) cmdRecipeShow() *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use: "show <name>", Args: cobra.ExactArgs(1),
		Short: "Print a recipe's source (read it before you run it)",
		Long: "Print the recipe file as written, after validating it. A recipe drives the\n" +
			"browser you are already signed into, so read one you were sent the same way\n" +
			"you would read a shell script before running it — then check the resolved\n" +
			"commands with `recipe run <name> --dry-run`.",
		RunE: func(_ *cobra.Command, args []string) error {
			r, ok := a.loadRecipe(args[0], dir)
			if !ok {
				return nil
			}
			if a.jsonOut {
				a.emitOK("recipe", nil, map[string]any{"recipe": r})
				return nil
			}
			src, err := os.ReadFile(r.Path)
			if err != nil {
				a.emitErr("recipe", result.CodeGeneric, err.Error(), nil)
				return nil
			}
			if !a.quiet {
				fmt.Fprintf(a.err, "%s (%s)\n", r.Path, r.Source)
			}
			// The source is printed verbatim, comments included: what you are
			// reviewing must be the file itself, not a re-rendering of it.
			a.out.Write(src)
			if len(src) > 0 && src[len(src)-1] != '\n' {
				fmt.Fprintln(a.out)
			}
			a.exitCode = result.ExitOK
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "additional directory to search for recipes")
	return c
}

func (a *App) cmdRecipeNew() *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use: "new <name>", Args: cobra.ExactArgs(1),
		Short: "Write a commented recipe template and print its path",
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			if err := recipe.ValidName(name); err != nil {
				a.emitErr("recipe", result.CodeUsage, err.Error(), nil)
				return nil
			}
			// Project-local by default: the point of a recipe is that it can be
			// committed next to the app it automates.
			target := dir
			if target == "" {
				target = filepath.Join(".chrome-cdp", "recipes")
			}
			path := filepath.Join(target, name+".yaml")
			if _, err := os.Stat(path); err == nil {
				a.emitErr("recipe", result.CodeUsage, "refusing to overwrite an existing recipe: "+path, nil)
				return nil
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				a.emitErr("recipe", result.CodeGeneric, err.Error(), nil)
				return nil
			}
			if err := os.WriteFile(path, []byte(recipe.Template(name)), 0o644); err != nil {
				a.emitErr("recipe", result.CodeGeneric, err.Error(), nil)
				return nil
			}
			// The scaffold is loaded straight back: a template the validator
			// rejects is a terrible first impression, so it fails here, loudly,
			// rather than the first time the author runs it.
			if _, err := recipe.Load(path, "new"); err != nil {
				_ = os.Remove(path)
				a.emitErr("recipe", result.CodeGeneric, "the recipe template failed its own validation (this is a bug): "+err.Error(), nil)
				return nil
			}
			a.emitOK("recipe", nil, map[string]any{"path": path, "name": name})
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "directory to write the recipe into (default ./.chrome-cdp/recipes)")
	return c
}

func (a *App) cmdRecipeRun() *cobra.Command {
	var dir string
	var sets []string
	var dryRun bool
	var fromStep int
	c := &cobra.Command{
		Use: "run <name>", Args: cobra.ExactArgs(1),
		Short: "Run a recipe: one NDJSON envelope per step, then a summary",
		Long: "Resolve a recipe's inputs and run its steps over one held connection,\n" +
			"emitting one envelope per step (with `step` and `label` so a caller can\n" +
			"correlate without counting lines) and a summary at the end. The process\n" +
			"exit code is the failing step's.\n\n" +
			"Validation happens before Chrome is touched: a missing required input, an\n" +
			"unknown --set key, an undeclared {{placeholder}}, or an out-of-range\n" +
			"--from-step is exit 2 with nothing run.\n\n" +
			"  chrome-cdp recipe run submit-timesheet --set week=2026-07-20\n" +
			"  chrome-cdp recipe run submit-timesheet --dry-run | chrome-cdp session",
		RunE: func(_ *cobra.Command, args []string) error {
			set, err := parseSets(sets)
			if err != nil {
				a.emitErr("recipe", result.CodeUsage, err.Error(), nil)
				return nil
			}
			r, ok := a.loadRecipe(args[0], dir)
			if !ok {
				return nil
			}
			// An explicit --target on the run overrides the recipe's own
			// `target:`; both end up injected into each step's argv, so the
			// dry-run listing is exactly what executes.
			plan, err := recipe.Resolve(r, recipe.Opts{
				Set: set, Target: a.targetFlag, FromStep: fromStep,
				Split: stepSplitter(a.defaults),
			})
			if err != nil {
				a.emitErr("recipe", result.CodeUsage, err.Error(), nil)
				return nil
			}
			if dryRun {
				a.emitDryRun(plan)
				return nil
			}
			a.runPlan(plan)
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "additional directory to search for recipes")
	c.Flags().StringArrayVar(&sets, "set", nil, "supply an input as k=v (repeatable)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the resolved argv lines (valid `session` input) and run nothing")
	c.Flags().IntVar(&fromStep, "from-step", 0, "start at step N (1-based) — sharp: earlier steps' effects are assumed done")
	return c
}

// stepSplitter returns a recipe.Splitter backed by the real command tree, so
// "is this element a flag" is answered by the same flag definitions cobra will
// parse with rather than by a table that would drift away from them.
//
// The tree is built on a scratch App: newRoot binds every flag to a field of
// the receiver, and classifying an argv must not write to the App that is about
// to run it. Nothing on the scratch tree is ever executed.
func stepSplitter(d config.Defaults) recipe.Splitter {
	root := (&App{defaults: d}).newRoot()
	return func(argv []string) ([]int, []int, bool) { return splitStepArgv(root, argv) }
}

// splitStepArgv walks a step's argv as written and says which elements the
// command tree may parse (its command path and its flags, each with the value
// it consumes) and which are data.
//
// It mirrors pflag's own rules — `--name`, `--name=v`, `--name v` when the flag
// has no NoOptDefVal, `-abc` shorthand clusters, and `--` — because a
// disagreement here would move an element into the wrong section. The failure
// is not silent either way: an element that lands in the flag section by
// mistake is parsed exactly as it is today, and one that lands in the data
// section makes the command report a wrong argument.
func splitStepArgv(root *cobra.Command, argv []string) (flagIdx, posIdx []int, ok bool) {
	cmd := root
	resolved := false // the argv named a command; nothing but data can follow the last one
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		switch {
		case tok == "--":
			// The step wrote its own terminator: everything past it is data, and
			// buildArgv re-emits the one terminator that belongs there.
			for j := i + 1; j < len(argv); j++ {
				posIdx = append(posIdx, j)
			}
			return flagIdx, posIdx, resolved
		case strings.HasPrefix(tok, "--"):
			flagIdx = append(flagIdx, i)
			name, hasInlineValue := tok[2:], false
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name, hasInlineValue = name[:eq], true
			}
			if !hasInlineValue && longFlagTakesValue(cmd, name) && i+1 < len(argv) {
				i++
				flagIdx = append(flagIdx, i)
			}
		case len(tok) > 1 && tok[0] == '-':
			flagIdx = append(flagIdx, i)
			if shorthandTakesNextValue(cmd, tok[1:]) && i+1 < len(argv) {
				i++
				flagIdx = append(flagIdx, i)
			}
		default:
			if sub := subCommand(cmd, tok); sub != nil {
				cmd, resolved = sub, true
				flagIdx = append(flagIdx, i)
				continue
			}
			if !resolved {
				// argv[0] names no command. Leave the argv alone so cobra
				// reports "unknown command" against what the author wrote.
				return nil, nil, false
			}
			posIdx = append(posIdx, i)
		}
	}
	return flagIdx, posIdx, resolved
}

// subCommand finds a child of cmd by name or alias. A verb with no children
// (`text`, `click`) never matches, so a positional that happens to read like a
// command name stays data.
func subCommand(cmd *cobra.Command, name string) *cobra.Command {
	for _, sub := range cmd.Commands() {
		if sub.Name() == name {
			return sub
		}
		for _, alias := range sub.Aliases {
			if alias == name {
				return sub
			}
		}
	}
	return nil
}

// lookupFlag finds a long flag visible to cmd: its own, plus the persistent
// flags it inherits (every global flag lives on the root).
func lookupFlag(cmd *cobra.Command, name string) *pflag.Flag {
	if f := cmd.Flags().Lookup(name); f != nil {
		return f
	}
	if f := cmd.InheritedFlags().Lookup(name); f != nil {
		return f
	}
	return cmd.Root().PersistentFlags().Lookup(name)
}

// longFlagTakesValue reports whether `--name` consumes the next argv element.
// A flag pflag gave a NoOptDefVal (every bool) does not; an unknown flag is
// assumed not to, so its neighbour stays data rather than being swallowed by a
// typo.
func longFlagTakesValue(cmd *cobra.Command, name string) bool {
	f := lookupFlag(cmd, name)
	return f != nil && f.NoOptDefVal == ""
}

// shorthandTakesNextValue reports whether a `-abc` cluster consumes the NEXT
// argv element. pflag hands the remainder of the cluster to the first flag that
// wants a value, so only a cluster that ends on such a flag reaches past itself.
func shorthandTakesNextValue(cmd *cobra.Command, cluster string) bool {
	if len(cluster) > 1 && cluster[1] == '=' {
		return false // -x=value carries its own
	}
	for i := 0; i < len(cluster); i++ {
		f := cmd.Flags().ShorthandLookup(string(cluster[i]))
		if f == nil {
			f = cmd.Root().PersistentFlags().ShorthandLookup(string(cluster[i]))
		}
		if f == nil {
			return false
		}
		if f.NoOptDefVal == "" {
			return i == len(cluster)-1
		}
	}
	return false
}

// parseSets turns repeated --set k=v flags into a map. A repeated key is an
// error rather than last-one-wins: a duplicated --set is far more often a
// mistake than an intention, and the cost of guessing wrong is a run against
// the wrong value.
func parseSets(sets []string) (map[string]string, error) {
	m := make(map[string]string, len(sets))
	for _, kv := range sets {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--set must be k=v, got %q", kv)
		}
		if _, dup := m[k]; dup {
			return nil, fmt.Errorf("--set %s was given more than once", k)
		}
		m[k] = v
	}
	return m, nil
}

// emitDryRun prints what the recipe would run and nothing else.
//
// In human mode stdout carries only the resolved argv lines, one JSON array per
// line — the exact bytes `session` consumes — so `recipe run x --dry-run |
// chrome-cdp session` is a real pipeline and not a demo. Under --json the same
// plan comes back as a single envelope.
func (a *App) emitDryRun(plan *recipe.Plan) {
	if a.jsonOut {
		a.emitOK("recipe", nil, map[string]any{
			"recipe":    plan.Recipe.Name,
			"dry_run":   true,
			"inputs":    plan.Inputs,
			"from_step": plan.FromStep,
			"steps":     plan.Steps,
		})
		return
	}
	for _, st := range plan.Steps {
		line, err := json.Marshal(st.Argv)
		if err != nil {
			a.emitErr("recipe", result.CodeGeneric, err.Error(), nil)
			return
		}
		fmt.Fprintln(a.out, string(line))
	}
	if !a.quiet {
		fmt.Fprintf(a.err, "dry run: %d step(s) of recipe %s; nothing was executed\n", len(plan.Steps), plan.Recipe.Name)
	}
	a.exitCode = result.ExitOK
}

// runPlan executes a resolved plan through the ordinary command tree — the same
// path `session` uses per stdin line — and emits one envelope per step plus a
// summary.
func (a *App) runPlan(plan *recipe.Plan) {
	// Recursion is a property of the runner, not of any one file: the load-time
	// reserved-verb check reads argv[0] of the recipe in front of it and cannot
	// see what a step's command re-enters. This is the backstop that makes
	// "recipes cannot invoke recipes" true rather than merely validated.
	if a.inRecipe {
		a.emitErr("recipe", result.CodeUsage,
			"a recipe cannot run another recipe: "+plan.Recipe.Name+" was invoked from inside a running recipe (recursion is not part of this format)", nil)
		return
	}
	// `recipe run` is Exempt from the origin rules — each step is checked on its
	// own instead — but it is still a verb an operator can name in
	// verbs_denied, and denying "run files other people wrote" is the most
	// obvious thing to want from that list. Checking here rather than in the
	// cobra RunE leaves `--dry-run` and `recipe show` available for reading an
	// untrusted recipe, which is the point of both.
	if perr := a.checkPolicy(a.policyVerb(), ""); perr != nil {
		a.emitErr("recipe", perr.Code, perr.Message, perr.Details)
		return
	}
	a.inRecipe = true
	// A recipe run is a batch with the same one-envelope-per-line contract
	// `session` promises, so a streaming verb has to reject itself here for the
	// same reason. Without this, `console --follow` in a step blocked for the
	// whole --timeout, buffered its stream into execStep's in-memory buffer,
	// and came out as unparseable output with no step or label on it.
	wasInSession := a.inSession
	a.inSession = true
	defer func() { a.inRecipe, a.inSession = false, wasInSession }()

	start := a.start
	// Flags are re-registered per Execute, so anything the user set on the
	// `recipe run` invocation has to be folded into the defaults to survive
	// into each step. Only the connection and timeout flags are propagated:
	// selector semantics (--by, --role, …) belong in the step's own argv, where
	// a reader of the recipe can see them.
	quiet := a.quiet
	a.defaults.Timeout = a.timeout
	a.defaults.NoLaunch = a.noLaunch
	a.defaults.NoDaemon = a.noDaemon
	a.defaults.ProfileDir = a.profileDir
	a.defaults.Port = a.port
	// Per-step output is NDJSON, exactly the stream `session` produces.
	a.defaults.JSON = true

	completed := 0
	var failed map[string]any
	failExit := result.ExitOK
	for _, st := range plan.Steps {
		exit, env := a.execStep(st, quiet)
		if exit == result.ExitOK {
			completed++
			continue
		}
		if failed == nil {
			code := envCode(env)
			failed = map[string]any{"index": st.Index, "label": st.Label, "code": code}
			// The exit follows the code the summary reports, so a caller that
			// branches on the number and one that reads the envelope cannot
			// reach different conclusions. The envelope is the public API; the
			// exit is derived from it everywhere else, and here too.
			failExit = result.ExitCodeFor(code)
		}
		if st.OnError != recipe.OnErrorContinue {
			break
		}
	}

	// The steps reset a.start on every Execute; restore it so the summary's
	// elapsed_ms covers the run and not just its last step.
	a.start = start
	res := map[string]any{
		"recipe":     plan.Recipe.Name,
		"steps":      len(plan.Steps),
		"completed":  completed,
		"failed":     failed, // explicit null on success
		"inputs":     plan.Inputs,
		"from_step":  plan.FromStep,
		"elapsed_ms": time.Since(start).Milliseconds(),
	}
	if failed == nil {
		a.emitOK("recipe", nil, res)
		return
	}
	code, _ := failed["code"].(string)
	a.emit(result.Envelope{
		OK:      false,
		Command: "recipe",
		Result:  res,
		Error: &result.Err{
			Code:    code,
			Message: fmt.Sprintf("recipe %s failed at step %d%s: %s", plan.Recipe.Name, failed["index"], labelSuffix(failed["label"]), code),
		},
	})
	// The contract is that a recipe exits with its failing step's code, so a
	// caller branches on the same exit codes as a single command. Set it after
	// emit, which would otherwise derive an exit from the summary envelope.
	a.exitCode = failExit
}

func labelSuffix(label any) string {
	if s, _ := label.(string); s != "" {
		return " (" + s + ")"
	}
	return ""
}

// execStep runs one resolved step through the command tree, capturing its
// envelope so `step` and `label` can be folded in before it reaches stdout.
func (a *App) execStep(st recipe.PlanStep, quiet bool) (int, map[string]any) {
	var buf bytes.Buffer
	real := a.out
	a.out = &buf
	exit := a.Execute(st.Argv...)
	a.out = real

	env, ok := parseStepEnvelope(buf.Bytes())
	if !ok {
		env, exit = rawOutputEnvelope(st, buf.Bytes(), exit)
	}
	env["step"] = st.Index
	if st.Label != "" {
		env["label"] = st.Label
	}
	// Under --quiet only the summary is printed (matching how the rest of the
	// CLI treats verbosity); the run itself is unchanged.
	if !quiet {
		if line, err := json.Marshal(env); err == nil {
			fmt.Fprintln(a.out, string(line))
		}
	}
	return exit, env
}

// parseStepEnvelope parses a step's stdout as exactly one JSON object. Numbers are
// kept as json.Number so re-encoding the envelope with the added step/label
// fields cannot reshape a payload's values.
func parseStepEnvelope(b []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, false
	}
	if dec.More() {
		return nil, false
	}
	return m, true
}

// rawOutputEnvelope replaces a step's unparseable stdout with a failure
// envelope of its own, and returns the exit that envelope implies.
//
// A recipe's stdout is NDJSON. A step that writes raw bytes there —
// `screenshot -o -` and `pdf -o -` write their file to stdout and emit no
// envelope at all — used to have those bytes passed through into the middle of
// the stream, so every reader downstream lost the rest of the run. Failing the
// step says what happened, keeps the stream parseable, and stops the recipe at
// the point the author has to fix.
func rawOutputEnvelope(st recipe.PlanStep, raw []byte, exit int) (map[string]any, int) {
	verb := ""
	if len(st.Argv) > 0 {
		verb = st.Argv[0]
	}
	code := result.CodeUsage
	msg := fmt.Sprintf("step %d (%s) wrote %d bytes to stdout instead of one result envelope; a step that writes a file to stdout (`-o -`) cannot run in a recipe, because the run's output is NDJSON — give it a path instead",
		st.Index, verb, len(raw))
	if exit != result.ExitOK {
		// The step failed AND said nothing parseable. Keep its own exit by
		// naming the code that maps to it, so the summary and the process agree.
		code = codeForExit(exit)
		msg = fmt.Sprintf("step %d (%s) failed without emitting a result envelope; %d bytes of its output were dropped to keep the run's NDJSON parseable",
			st.Index, verb, len(raw))
	}
	env := map[string]any{
		"ok":      false,
		"command": verb,
		"error":   map[string]any{"code": code, "message": msg, "bytes": len(raw)},
	}
	return env, result.ExitCodeFor(code)
}

// codeForExit names an exit code, for the one case where a step's exit is known
// and its error.code is not. Every documented exit has a code that maps to it
// (TestCodeForExitRoundTrips pins that), so the pair can never disagree.
func codeForExit(exit int) string {
	for _, code := range []string{
		result.CodeUsage, result.CodeConnection, result.CodeTargetTimeout,
		result.CodeCDP, result.CodeDaemon, result.CodePermissionDenied,
	} {
		if result.ExitCodeFor(code) == exit {
			return code
		}
	}
	return result.CodeGeneric
}

// envCode digs the stable error.code out of a step envelope, falling back to
// the generic code when a step failed without emitting one.
func envCode(env map[string]any) string {
	if e, ok := env["error"].(map[string]any); ok {
		if c, ok := e["code"].(string); ok && c != "" {
			return c
		}
	}
	return result.CodeGeneric
}
