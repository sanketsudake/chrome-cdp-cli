// Package recipe loads, validates, and resolves recipes: saved `session`
// scripts with a small header.
//
// A recipe is deliberately not a language. Every step's `run` is exactly the
// argv array a `session` stdin line carries, so anything valid in `session` is
// valid here and vice versa. This package never touches Chrome and never runs
// anything: it reads a file, validates it, substitutes declared inputs into
// argv elements, and hands back a Plan of argv lines for the caller to feed
// through the existing `session` execution path.
//
// The split is the point. Everything a recipe can get wrong statically —
// malformed YAML, an undeclared `{{placeholder}}`, a missing required input, an
// unknown --set key, an out-of-range --from-step — is an error from this
// package, raised before the CLI connects to a browser.
package recipe

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxSteps caps a recipe's length. A bound removes a whole category of design
// questions (and of runaway shared files) for the price of one constant.
const MaxSteps = 200

// Error abort/continue values for a step's on_error.
const (
	OnErrorAbort    = "abort"
	OnErrorContinue = "continue"
)

// reservedVerbs are commands a step may not invoke.
//
// `recipe` would let recipes call recipes — recursion, and with it lifetime,
// depth, and cycle questions this format exists to avoid. `session` reads the
// process's stdin, which is not the recipe, so a step invoking it would consume
// input the author never wrote.
var reservedVerbs = map[string]string{
	"recipe":  "recipes cannot invoke recipes",
	"session": "a step cannot invoke `session`: it would read the process's stdin, not the recipe",
}

// nameRE constrains both recipe names and input names. Recipe names become path
// elements, so anything outside this set (separators, dots, spaces) is refused
// before it reaches filepath.Join.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// placeholderRE is deliberately permissive inside the braces: it captures
// whatever sits between `{{` and the next `}}` so that a malformed reference
// like `{{1bad}}` is reported as an error rather than silently passed through
// as a literal.
var placeholderRE = regexp.MustCompile(`\{\{([^}]*)\}\}`)

// Input declares one parameter. That is the whole schema — no types, no
// validation expressions; a recipe that needs them should be a program calling
// `session`.
type Input struct {
	Required    bool   `json:"required"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description,omitempty"`
}

// Step is one validated step: an argv array plus its failure policy.
type Step struct {
	Label   string   `json:"label,omitempty"`
	Run     []string `json:"run"`
	OnError string   `json:"on_error"`
}

// Recipe is a loaded, schema-valid recipe. Inputs are unresolved at this stage:
// Run still carries `{{placeholder}}` text, and every placeholder in it is
// guaranteed to name a declared input.
type Recipe struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Inputs      map[string]Input `json:"inputs,omitempty"`
	Target      string           `json:"target,omitempty"`
	Steps       []Step           `json:"steps"`

	// Path is where the recipe was read from; Source names the search-path
	// entry that provided it ("project", "user", or "--dir").
	Path   string `json:"path"`
	Source string `json:"source"`
}

// InputNames returns the declared input names in sorted order, so listing and
// showing a recipe is deterministic despite the underlying map.
func (r *Recipe) InputNames() []string {
	names := make([]string, 0, len(r.Inputs))
	for n := range r.Inputs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// fileRecipe mirrors the YAML document. Decoding goes through this shape (with
// KnownFields on) so an unknown top-level key is an error rather than a
// silently ignored typo.
type fileRecipe struct {
	Name        string               `yaml:"name"`
	Description string               `yaml:"description"`
	Inputs      map[string]fileInput `yaml:"inputs"`
	Target      string               `yaml:"target"`
	Steps       []fileStep           `yaml:"steps"`
}

type fileInput struct {
	Required    bool   `yaml:"required"`
	Default     string `yaml:"default"`
	Description string `yaml:"description"`
}

// fileStep keeps `run` as a raw node so the validator can say precisely what is
// wrong with it ("a string, not an array"; "element 2 is a number") instead of
// surfacing a yaml type error.
type fileStep struct {
	Label   string    `yaml:"label"`
	Run     yaml.Node `yaml:"run"`
	OnError string    `yaml:"on_error"`
}

// Dir is one entry in the recipe search path.
type Dir struct {
	Path   string
	Source string // "project" | "user" | "--dir"
}

// SearchPath returns the ordered recipe search path; the first match wins.
//
// Project-local beats user-global on purpose: a repo that carries
// .chrome-cdp/recipes/ hands its automations to everyone who clones it, and a
// teammate's checkout must win over whatever they happen to have in their own
// config dir. Empty arguments are skipped, so a caller that cannot determine a
// working directory simply gets a shorter path.
func SearchPath(cwd, userConfigDir, extra string) []Dir {
	var dirs []Dir
	if cwd != "" {
		dirs = append(dirs, Dir{Path: filepath.Join(cwd, ".chrome-cdp", "recipes"), Source: "project"})
	}
	if userConfigDir != "" {
		dirs = append(dirs, Dir{Path: filepath.Join(userConfigDir, "recipes"), Source: "user"})
	}
	if extra != "" {
		dirs = append(dirs, Dir{Path: extra, Source: "--dir"})
	}
	return dirs
}

// ErrNotFound reports a name that matched no file in any search directory.
var ErrNotFound = errors.New("recipe not found")

// Find locates a recipe file by name, returning the first match in the search
// path along with the source that provided it.
func Find(name string, dirs []Dir) (path, source string, err error) {
	if err := ValidName(name); err != nil {
		return "", "", err
	}
	for _, d := range dirs {
		for _, ext := range []string{".yaml", ".yml"} {
			p := filepath.Join(d.Path, name+ext)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, d.Source, nil
			}
		}
	}
	return "", "", fmt.Errorf("%w: %q is not in %s", ErrNotFound, name, describeDirs(dirs))
}

func describeDirs(dirs []Dir) string {
	if len(dirs) == 0 {
		return "any search directory (none configured)"
	}
	paths := make([]string, 0, len(dirs))
	for _, d := range dirs {
		paths = append(paths, d.Path)
	}
	return strings.Join(paths, ", ")
}

// ValidName reports whether a name is usable as a recipe name. It is enforced
// wherever a name reaches the filesystem: the name is joined into a path, so
// separators and `..` must never survive this check.
func ValidName(name string) error {
	if name == "" {
		return errors.New("recipe name is empty")
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid recipe name %q: use letters, digits, - and _ (no path separators)", name)
	}
	return nil
}

// Load reads and validates one recipe file. source labels which search-path
// entry provided it and is carried through to the envelope.
func Load(path, source string) (*Recipe, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(data, path, source)
}

// parse is Load's pure half: bytes in, validated Recipe out.
func parse(data []byte, path, source string) (*Recipe, error) {
	var f fileRecipe
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// KnownFields turns a typo'd key into an error instead of a silently
	// dropped field — the difference between "your `describtion` did nothing"
	// and a run that quietly ignored half its header.
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %v", path, yamlMsg(err))
	}
	// A second document would be a second recipe in one file; refuse rather
	// than silently running the first.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("%s: file contains more than one YAML document; a recipe is a single document", path)
	}

	r := &Recipe{
		Name:        f.Name,
		Description: f.Description,
		Target:      f.Target,
		Path:        path,
		Source:      source,
	}
	if err := validateHeader(r, &f, path); err != nil {
		return nil, err
	}
	if err := validateSteps(r, &f, path); err != nil {
		return nil, err
	}
	return r, nil
}

// yamlMsg trims yaml.v3's leading "yaml: " / line-noise prefix so the CLI's
// one-line error message stays readable.
func yamlMsg(err error) string {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		return strings.Join(te.Errors, "; ")
	}
	if err.Error() == "EOF" {
		return "file is empty"
	}
	return strings.TrimPrefix(err.Error(), "yaml: ")
}

func validateHeader(r *Recipe, f *fileRecipe, path string) error {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if f.Name == "" {
		return fmt.Errorf("%s: `name` is required", path)
	}
	if err := ValidName(f.Name); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	// The name in the file and the name on disk must agree: `recipe run` takes
	// the filename, and a recipe that lists itself under a different name than
	// the one that runs it is a trap for whoever reviews it.
	if f.Name != stem {
		return fmt.Errorf("%s: `name: %s` does not match the filename %q — rename one so they agree", path, f.Name, stem)
	}
	if len(f.Inputs) > 0 {
		r.Inputs = make(map[string]Input, len(f.Inputs))
	}
	for name, in := range f.Inputs {
		if err := ValidName(name); err != nil {
			return fmt.Errorf("%s: input %q: use letters, digits, - and _", path, name)
		}
		if in.Required && in.Default != "" {
			return fmt.Errorf("%s: input %q is both required and has a default; pick one", path, name)
		}
		r.Inputs[name] = Input{Required: in.Required, Default: in.Default, Description: in.Description}
	}
	return nil
}

func validateSteps(r *Recipe, f *fileRecipe, path string) error {
	if len(f.Steps) == 0 {
		return fmt.Errorf("%s: `steps` is required and must list at least one step", path)
	}
	if len(f.Steps) > MaxSteps {
		return fmt.Errorf("%s: %d steps exceeds the %d-step cap", path, len(f.Steps), MaxSteps)
	}
	r.Steps = make([]Step, 0, len(f.Steps))
	for i, fs := range f.Steps {
		n := i + 1
		argv, err := argvFromNode(&fs.Run)
		if err != nil {
			return fmt.Errorf("%s: step %d: %v", path, n, err)
		}
		onErr, err := normalizeOnError(fs.OnError)
		if err != nil {
			return fmt.Errorf("%s: step %d: %v", path, n, err)
		}
		if err := checkVerb(argv[0]); err != nil {
			return fmt.Errorf("%s: step %d: %v", path, n, err)
		}
		if err := checkPlaceholders(argv, r.Inputs); err != nil {
			return fmt.Errorf("%s: step %d: %v", path, n, err)
		}
		r.Steps = append(r.Steps, Step{Label: fs.Label, Run: argv, OnError: onErr})
	}
	return nil
}

// argvFromNode enforces the one rule that keeps a recipe a `session` script:
// `run` is an array of strings, exactly what a `session` stdin line carries.
func argvFromNode(n *yaml.Node) ([]string, error) {
	if n.IsZero() {
		return nil, errors.New("`run` is required and must be an array of argv strings")
	}
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("`run` must be an array of argv strings, not %s (e.g. run: [\"nav\", \"https://example.com\"])", nodeKind(n))
	}
	if len(n.Content) == 0 {
		return nil, errors.New("`run` is empty; a step must name a command")
	}
	argv := make([]string, 0, len(n.Content))
	for i, item := range n.Content {
		// Requiring an explicit string tag is what turns `["--nth", 2]` into a
		// clear "quote it" error instead of a silent coercion; argv elements
		// are strings all the way to the command tree.
		if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
			return nil, fmt.Errorf("`run` element %d must be a string (quote it): got %s", i+1, nodeKind(item))
		}
		argv = append(argv, item.Value)
	}
	return argv, nil
}

func nodeKind(n *yaml.Node) string {
	switch n.Kind {
	case yaml.SequenceNode:
		return "an array"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!str":
			return "a string"
		case "!!int", "!!float":
			return "a number"
		case "!!bool":
			return "a boolean"
		case "!!null":
			return "null"
		}
		return "a scalar"
	}
	return "an unsupported value"
}

func normalizeOnError(v string) (string, error) {
	switch v {
	case "":
		return OnErrorAbort, nil
	case OnErrorAbort, OnErrorContinue:
		return v, nil
	}
	return "", fmt.Errorf("`on_error: %s` is not valid; use %q (default) or %q", v, OnErrorAbort, OnErrorContinue)
}

// checkVerb rejects the commands a step may not invoke, and requires the verb
// itself to be a literal. Allowing `run: ["{{cmd}}"]` would move the recursion
// question to runtime, where --set could smuggle past this check.
//
// A step must also NAME its command in argv[0], because that is the element
// this check reads. Cobra resolves the command after stripping leading flags,
// so `["--json", "recipe", "run", "x"]` looks like a `--json` step here and
// runs `recipe run x` there — which is unbounded recursion, and `["--quiet",
// "session"]` is a step that reads the process's own stdin. Refusing a leading
// flag keeps the validator and the runner looking at the same verb.
func checkVerb(verb string) error {
	if strings.Contains(verb, "{{") {
		return fmt.Errorf("the command name must be a literal, not a placeholder (%q)", verb)
	}
	if strings.HasPrefix(verb, "-") {
		return fmt.Errorf("a step must name its command first: %q is a flag, and a leading flag would hide the command from validation (write the verb, then its flags)", verb)
	}
	if why, bad := reservedVerbs[verb]; bad {
		return fmt.Errorf("%s: step runs %q", why, verb)
	}
	return nil
}

// checkPlaceholders verifies every `{{name}}` in an argv names a declared
// input. This runs at load, so an undeclared placeholder fails even for a step
// that --from-step would have skipped.
func checkPlaceholders(argv []string, inputs map[string]Input) error {
	for _, elem := range argv {
		for _, m := range placeholderRE.FindAllStringSubmatch(elem, -1) {
			name := strings.TrimSpace(m[1])
			if name == "" {
				return fmt.Errorf("empty placeholder %q", m[0])
			}
			if err := ValidName(name); err != nil {
				return fmt.Errorf("malformed placeholder %q: a placeholder names an input", m[0])
			}
			if _, ok := inputs[name]; !ok {
				return fmt.Errorf("placeholder %q is not a declared input (declare it under `inputs:`)", m[0])
			}
		}
	}
	return nil
}

// Entry is one row of a recipe listing: a loaded recipe, or the error that
// stopped it loading. One malformed file must not hide every other recipe.
type Entry struct {
	Name   string
	Path   string
	Source string
	Recipe *Recipe
	Err    error
}

// List enumerates recipes across the search path, first match per name winning
// so a project-local recipe shadows a user-global one of the same name.
func List(dirs []Dir) []Entry {
	seen := map[string]bool{}
	var out []Entry
	for _, d := range dirs {
		ents, err := os.ReadDir(d.Path)
		if err != nil {
			continue // a missing search directory is normal, not an error
		}
		var names []string
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			ext := filepath.Ext(e.Name())
			if ext != ".yaml" && ext != ".yml" {
				continue
			}
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, fn := range names {
			stem := strings.TrimSuffix(fn, filepath.Ext(fn))
			if seen[stem] {
				continue
			}
			seen[stem] = true
			p := filepath.Join(d.Path, fn)
			r, err := Load(p, d.Source)
			out = append(out, Entry{Name: stem, Path: p, Source: d.Source, Recipe: r, Err: err})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// PlanStep is one resolved step: argv with every placeholder substituted, ready
// to hand to the `session` execution path unchanged.
type PlanStep struct {
	// Index is the 1-based position in the recipe file, preserved across
	// --from-step so a summary points at the step the author would recognise.
	Index   int      `json:"step"`
	Label   string   `json:"label,omitempty"`
	Argv    []string `json:"run"`
	OnError string   `json:"on_error"`
}

// Plan is a fully resolved recipe: nothing left to decide, nothing left to
// look up.
type Plan struct {
	Recipe   *Recipe
	Inputs   map[string]string
	Steps    []PlanStep
	FromStep int // 1-based; 1 unless --from-step moved it
	Target   string
}

// Splitter classifies one step's argv AS WRITTEN — before any substitution —
// into the elements the command tree may parse (the command path and the step's
// own flags, with their values) and the elements that must arrive as data.
// Indices are into the argv it was given; an index in neither slice is dropped,
// which is how a step's own `--` is absorbed and re-emitted in the one place it
// belongs. ok is false when the argv names no command it can resolve, in which
// case Resolve leaves the argv alone and lets the command tree produce its
// ordinary usage error.
//
// It reads the argv as written because that is the only text the recipe's
// AUTHOR wrote. What is a flag has to be decided there and never by an input
// value: `--set sel=--target=@2` substituted into a step's positional is data,
// not a second target.
//
// The recipe package cannot implement this — flag arity lives in the command
// tree — so the CLI supplies it. Resolve without one keeps the argv in written
// order, which is what the pure tests here use.
type Splitter func(argv []string) (flagIdx, posIdx []int, ok bool)

// Opts are the run-time knobs Resolve applies.
type Opts struct {
	Set      map[string]string // values from --set
	Target   string            // overrides the recipe's own `target:` when set
	FromStep int               // 1-based start step; 0 means "from the beginning"
	Split    Splitter          // optional; see Splitter
}

// Resolve turns a validated Recipe into a Plan: it checks the supplied inputs
// against the declared ones, substitutes them into argv elements, injects the
// effective --target, and applies --from-step.
//
// Substitution is a single pass, so a value that itself contains `{{...}}` is
// carried through literally rather than re-expanded — an input can never smuggle
// in another input's value.
func Resolve(r *Recipe, opts Opts) (*Plan, error) {
	values, err := resolveInputs(r, opts.Set)
	if err != nil {
		return nil, err
	}
	target := r.Target
	if opts.Target != "" {
		target = opts.Target
	}
	from := opts.FromStep
	if from == 0 {
		from = 1
	}
	if from < 1 || from > len(r.Steps) {
		return nil, fmt.Errorf("--from-step %d is out of range: %s has %d steps (1-%d)", opts.FromStep, r.Name, len(r.Steps), len(r.Steps))
	}
	plan := &Plan{Recipe: r, Inputs: values, FromStep: from, Target: target}
	for i, s := range r.Steps {
		if i+1 < from {
			continue
		}
		argv := buildArgv(s.Run, values, target, opts.Split)
		plan.Steps = append(plan.Steps, PlanStep{Index: i + 1, Label: s.Label, Argv: argv, OnError: s.OnError})
	}
	return plan, nil
}

// buildArgv turns one step's argv as written into the argv that runs.
//
// Two things have to be true of the result, and both are decided from the
// argv AS WRITTEN rather than from the substituted text:
//
//   - The recipe's pinned target survives. A step that names its own --target
//     keeps it, but only when the AUTHOR wrote one: scanning the substituted
//     argv let `--set sel=--target=@2` suppress the header's target and point
//     the step at a different tab.
//   - A substituted value can never reach flag position. The step's data
//     elements are emitted after a `--` terminator, so an input that looks like
//     `--target=@2`, `--policy-off` or `-v` arrives as the one argv element it
//     is. RFC-0009 promises an input substitutes into ONE argv element and
//     never into a command line; without the terminator that held for word
//     splitting but not for flag parsing.
//
// The step's own `--`, if it wrote one, is absorbed by the splitter and
// re-emitted here, so the injected `--target` cannot land after it and be read
// as a positional.
func buildArgv(run []string, values map[string]string, target string, split Splitter) []string {
	sub := make([]string, len(run))
	for i, elem := range run {
		sub[i] = substitute(elem, values)
	}

	var flagIdx, posIdx []int
	ok := false
	if split != nil {
		flagIdx, posIdx, ok = split(run)
	}
	if !ok {
		// No command tree to classify against: keep the argv as written and let
		// the command tree report whatever is wrong with it.
		argv := append(make([]string, 0, len(sub)+2), sub...)
		if target != "" && !hasTargetFlag(run) {
			argv = append(argv, "--target", target)
		}
		return argv
	}

	argv := make([]string, 0, len(sub)+3)
	for _, i := range flagIdx {
		argv = append(argv, sub[i])
	}
	// The target is injected into the argv rather than applied out of band, so
	// the dry-run listing is the whole truth: what it prints is what runs, byte
	// for byte, whether it goes through `recipe run` or a pipe into `session`.
	if target != "" && !hasTargetFlagAt(run, flagIdx) {
		argv = append(argv, "--target", target)
	}
	if len(posIdx) > 0 {
		argv = append(argv, "--")
		for _, i := range posIdx {
			argv = append(argv, sub[i])
		}
	}
	return argv
}

func hasTargetFlag(argv []string) bool {
	for _, a := range argv {
		if isTargetFlag(a) {
			return true
		}
	}
	return false
}

// hasTargetFlagAt looks for the author's own --target among the elements the
// command tree will actually parse as flags, so a `--target` sitting in a
// step's data section does not suppress the header's.
func hasTargetFlagAt(argv []string, flagIdx []int) bool {
	for _, i := range flagIdx {
		if isTargetFlag(argv[i]) {
			return true
		}
	}
	return false
}

func isTargetFlag(a string) bool {
	return a == "--target" || strings.HasPrefix(a, "--target=")
}

// resolveInputs merges --set over the declared defaults, rejecting an unknown
// key and a missing required one.
func resolveInputs(r *Recipe, set map[string]string) (map[string]string, error) {
	// An unknown --set key is rejected rather than ignored: silently dropping
	// `--set hurs=8` would run the recipe with the default the user was trying
	// to override, which is the worst of both outcomes.
	var unknown []string
	for k := range set {
		if _, ok := r.Inputs[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown input%s %s for recipe %s (declared: %s)",
			plural(len(unknown)), quoteList(unknown), r.Name, declaredList(r))
	}

	values := make(map[string]string, len(r.Inputs))
	var missing []string
	for _, name := range r.InputNames() {
		in := r.Inputs[name]
		if v, ok := set[name]; ok {
			values[name] = v
			continue
		}
		if in.Required {
			missing = append(missing, name)
			continue
		}
		values[name] = in.Default
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required input%s %s for recipe %s (supply with --set %s=…)",
			plural(len(missing)), quoteList(missing), r.Name, missing[0])
	}
	return values, nil
}

func declaredList(r *Recipe) string {
	names := r.InputNames()
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func quoteList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(q, ", ")
}

// substitute replaces every `{{name}}` in one argv element with its value.
// The result is one argv element — there is no shell, no word splitting, and no
// second pass over the substituted text.
func substitute(elem string, values map[string]string) string {
	return placeholderRE.ReplaceAllStringFunc(elem, func(m string) string {
		name := strings.TrimSpace(placeholderRE.FindStringSubmatch(m)[1])
		// Load guarantees every placeholder is declared, so a miss here can
		// only mean a caller resolved against a different recipe; leaving the
		// text untouched is the honest failure.
		if v, ok := values[name]; ok {
			return v
		}
		return m
	})
}

// Template returns the scaffold `recipe new` writes. It is a working recipe,
// not a sketch: it loads and validates as-is, because a scaffold the validator
// rejects is the worst possible introduction to the format.
func Template(name string) string {
	return fmt.Sprintf(`# %[1]s — a chrome-cdp recipe.
#
# A recipe is a "session" script with a header: every run: below is exactly the
# argv you would type after "chrome-cdp". There is no shell anywhere in this
# format — an input substitutes into ONE argv element, never into a command
# line — so a recipe you were sent can be reviewed by reading it.
#
#   chrome-cdp recipe run %[1]s --dry-run          # print what it would run
#   chrome-cdp recipe run %[1]s --set url=https://example.com
#
# Never put a credential in a recipe: chrome-cdp drives the browser you are
# already signed into, so a recipe never needs one.
name: %[1]s
description: TODO — one line on what this recipe does.

# Declared inputs. required: true fails before the browser is touched;
# default: supplies a value when --set is omitted. No types, no expressions.
inputs:
  url:
    default: "https://example.com"
    description: "Page to open."

# Optional default --target for every step (idprefix | url:<s> | title:<s> | @N).
# Without it, each step uses the sticky tab set by "chrome-cdp use".
# target: url:example.com

steps:
  - label: open the page
    run: ["nav", "{{url}}"]
  - label: let the page settle
    run: ["wait", "--idle"]
  - label: read the heading
    run: ["text", "h1"]
    # on_error: continue   # default is abort — stop the run at the first failure
`, name)
}
