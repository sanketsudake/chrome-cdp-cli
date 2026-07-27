package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/config"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

// isolateRecipes points the recipe search path at a temp project directory and
// a temp config home, so a test never sees (or writes) the developer's real
// recipes. It uses t.Chdir/t.Setenv rather than an injected seam so the test
// exercises the real resolution order — which is the thing VS-11 is about — and
// therefore these tests are not parallel.
func isolateRecipes(t *testing.T) (project, user string) {
	t.Helper()
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	cfg := filepath.Join(root, "config")
	project = filepath.Join(proj, ".chrome-cdp", "recipes")
	user = filepath.Join(cfg, "chrome-cdp", "recipes")
	for _, d := range []string{project, user} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Chdir(proj)
	return project, user
}

func writeRecipe(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// envelopes parses NDJSON stdout into envelopes, failing on any line that is
// not one JSON value — the stream contract `session` and `recipe run` share.
func envelopes(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("output line is not JSON: %q (%v)", line, err)
		}
		out = append(out, e)
	}
	return out
}

// recordingBrowser records every browser call in order, which is what makes the
// `recipe run` vs `dry-run | session` equivalence assertable (VS-10).
type recordingBrowser struct {
	stubBrowser
	calls []string
	// failOn makes the Nth Fill (1-based) fail, for the abort/continue cases.
	failFillsAt int
	fills       int
}

func (b *recordingBrowser) List(context.Context) ([]target.Info, error) {
	b.calls = append(b.calls, "List")
	return []target.Info{{ID: "aa11", Title: "App", URL: "https://app.test/"}}, nil
}

func (b *recordingBrowser) Navigate(_ context.Context, id, url string) (map[string]any, error) {
	b.calls = append(b.calls, fmt.Sprintf("Navigate(%s,%s)", id, url))
	return map[string]any{"url": url, "status": 200}, nil
}

func (b *recordingBrowser) Fill(_ context.Context, id, sel, value string, q chrome.QueryOpts) (map[string]any, error) {
	b.fills++
	b.calls = append(b.calls, fmt.Sprintf("Fill(%s,%s,%q,by=%s,role=%s)", id, sel, value, q.By, q.Role))
	if b.failFillsAt == b.fills {
		return nil, errors.New("timeout waiting for selector")
	}
	return map[string]any{"filled": true}, nil
}

func (b *recordingBrowser) Wait(_ context.Context, id string, cond chrome.WaitCond) (map[string]any, error) {
	b.calls = append(b.calls, fmt.Sprintf("Wait(%s,idle=%v,text=%q)", id, cond.Idle, cond.Text))
	return map[string]any{"waited": "idle"}, nil
}

func (b *recordingBrowser) Text(_ context.Context, id, sel string, _ chrome.TextOpts) (map[string]any, error) {
	b.calls = append(b.calls, fmt.Sprintf("Text(%s,%s)", id, sel))
	return map[string]any{"text": "hello"}, nil
}

var _ chrome.Browser = (*recordingBrowser)(nil)

const threeStep = `name: three
description: Three steps against a stub.
target: aa11
steps:
  - label: open
    run: ["nav", "https://app.test/one"]
  - run: ["wait", "--idle"]
  - label: read
    run: ["text", "h1"]
`

// VS-1: a recipe emits one envelope per step, in order, then a summary.
func TestRecipeRunRoundTrip(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "three", threeStep)

	var out, errb bytes.Buffer
	app := New(&recordingBrowser{}, &out, &errb)
	if code := app.Execute("recipe", "run", "three"); code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, errb.String())
	}

	envs := envelopes(t, out.String())
	if len(envs) != 4 {
		t.Fatalf("got %d envelopes, want 3 steps + 1 summary:\n%s", len(envs), out.String())
	}
	wantCmds := []string{"nav", "wait", "text", "recipe"}
	for i, want := range wantCmds {
		if envs[i]["command"] != want {
			t.Errorf("envelope %d command = %v, want %s", i, envs[i]["command"], want)
		}
		if envs[i]["ok"] != true {
			t.Errorf("envelope %d = %v, want ok", i, envs[i])
		}
	}
	// Step envelopes carry step/label so a caller correlates without counting
	// lines; the summary is the one envelope that carries neither.
	if envs[0]["step"] != 1.0 || envs[0]["label"] != "open" {
		t.Errorf("step envelope 1 = %v, want step 1 labelled open", envs[0])
	}
	if _, ok := envs[1]["label"]; ok {
		t.Errorf("unlabelled step carries a label: %v", envs[1])
	}
	if envs[2]["step"] != 3.0 || envs[2]["label"] != "read" {
		t.Errorf("step envelope 3 = %v, want step 3 labelled read", envs[2])
	}

	res := envs[3]["result"].(map[string]any)
	if res["recipe"] != "three" || res["steps"] != 3.0 || res["completed"] != 3.0 {
		t.Errorf("summary = %v, want three/3/3", res)
	}
	if res["failed"] != nil {
		t.Errorf("summary failed = %v, want null", res["failed"])
	}
}

// VS-7: the run stops at the first failure, the summary says which step and
// why, and the process exit is that step's — 4 for a target timeout.
func TestRecipeAbortsAtFailingStepWithLocation(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "abort", `name: abort
target: aa11
steps:
  - run: ["nav", "https://app.test/"]
  - label: save
    run: ["fill", "#a", "1"]
  - label: never runs
    run: ["fill", "#b", "2"]
`)
	b := &recordingBrowser{failFillsAt: 1}
	var out, errb bytes.Buffer
	app := New(b, &out, &errb)
	if code := app.Execute("recipe", "run", "abort"); code != 4 {
		t.Fatalf("exit = %d, want 4 (the failing step's code)\n%s", code, out.String())
	}
	if b.fills != 1 {
		t.Errorf("%d fills, want 1: the step after the failure must not run", b.fills)
	}

	envs := envelopes(t, out.String())
	summary := envs[len(envs)-1]
	if summary["ok"] != false {
		t.Errorf("summary = %v, want ok:false", summary)
	}
	res := summary["result"].(map[string]any)
	if res["completed"] != 1.0 {
		t.Errorf("completed = %v, want 1", res["completed"])
	}
	failed, ok := res["failed"].(map[string]any)
	if !ok {
		t.Fatalf("summary has no failed object: %v", res)
	}
	if failed["index"] != 2.0 || failed["label"] != "save" || failed["code"] != "target_timeout" {
		t.Errorf("failed = %v, want index 2, label save, code target_timeout", failed)
	}
}

// VS-8: on_error: continue keeps going, but the run is still a failure and the
// summary still names the first thing that went wrong.
func TestRecipeOnErrorContinue(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "cont", `name: cont
target: aa11
steps:
  - run: ["nav", "https://app.test/"]
  - label: optional
    run: ["fill", "#a", "1"]
    on_error: continue
  - label: still runs
    run: ["fill", "#b", "2"]
`)
	b := &recordingBrowser{failFillsAt: 1}
	var out, errb bytes.Buffer
	app := New(b, &out, &errb)
	code := app.Execute("recipe", "run", "cont")
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (the failing step's code even when the run continued)", code)
	}
	if b.fills != 2 {
		t.Errorf("%d fills, want 2: on_error continue must run the next step", b.fills)
	}

	envs := envelopes(t, out.String())
	summary := envs[len(envs)-1]
	res := summary["result"].(map[string]any)
	if summary["ok"] != false {
		t.Errorf("summary ok = %v, want false", summary["ok"])
	}
	if res["completed"] != 2.0 {
		t.Errorf("completed = %v, want 2 (the successful steps)", res["completed"])
	}
	if failed := res["failed"].(map[string]any); failed["index"] != 2.0 {
		t.Errorf("failed = %v, want the first failure, index 2", failed)
	}
}

// VS-9: --dry-run resolves and prints, and the browser is never contacted. The
// stub fails the test on any call, so this asserts on behaviour and not just on
// the absence of output.
func TestRecipeDryRunTouchesNothing(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "three", threeStep)

	var out, errb bytes.Buffer
	app := New(noCall(t), &out, &errb)
	if code := app.Execute("recipe", "run", "three", "--dry-run"); code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want one per step:\n%s", len(lines), out.String())
	}
	var first []string
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("dry-run line is not a JSON argv array: %q", lines[0])
	}
	// The step's data follows a `--` terminator, so nothing substituted into it
	// can reach flag position; the recipe's pinned target sits ahead of it.
	want := []string{"nav", "--target", "aa11", "--", "https://app.test/one"}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("line 1 = %#v, want %#v", first, want)
	}
}

// VS-10: the dry-run output is valid `session` input, and running it through
// `session` drives the browser exactly as `recipe run` does.
//
// This is the structural guard on "recipes are `session` with a header": if the
// runner ever grows a second execution path — an extra flag it applies out of
// band, a target it resolves differently, a step it synthesises — the two call
// sequences stop matching and this test fails.
func TestRecipeRunEqualsDryRunThroughSession(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "equiv", `name: equiv
target: aa11
inputs:
  who: { default: "world" }
steps:
  - label: open
    run: ["nav", "https://app.test/{{who}}"]
  - run: ["wait", "--idle"]
  - label: fill it in
    run: ["fill", "Full name", "{{who}}", "--by", "name", "--role", "textbox"]
  - run: ["text", "h1"]
`)

	viaRecipe := &recordingBrowser{}
	var recipeOut, recipeErr bytes.Buffer
	if code := New(viaRecipe, &recipeOut, &recipeErr).Execute("recipe", "run", "equiv"); code != 0 {
		t.Fatalf("recipe run exit = %d\n%s", code, recipeErr.String())
	}

	var dry, dryErr bytes.Buffer
	if code := New(noCall(t), &dry, &dryErr).Execute("recipe", "run", "equiv", "--dry-run"); code != 0 {
		t.Fatalf("dry run exit = %d\n%s", code, dryErr.String())
	}

	viaSession := &recordingBrowser{}
	var sessionOut, sessionErr bytes.Buffer
	sessionApp := New(viaSession, &sessionOut, &sessionErr).WithInput(strings.NewReader(dry.String()))
	if code := sessionApp.Execute("session"); code != 0 {
		t.Fatalf("session exit = %d\n%s", code, sessionErr.String())
	}

	if !reflect.DeepEqual(viaRecipe.calls, viaSession.calls) {
		t.Errorf("recipe run and dry-run|session drove the browser differently:\n recipe:  %v\n session: %v",
			viaRecipe.calls, viaSession.calls)
	}
	if len(viaRecipe.calls) == 0 {
		t.Fatal("no browser calls recorded; the equivalence assertion would be vacuous")
	}

	// The step envelopes are the same stream too, once the recipe's step/label
	// correlation fields are set aside.
	recipeEnvs := envelopes(t, recipeOut.String())
	sessionEnvs := envelopes(t, sessionOut.String())
	if len(recipeEnvs) != len(sessionEnvs)+1 {
		t.Fatalf("recipe emitted %d envelopes, session %d (want session + 1 summary)", len(recipeEnvs), len(sessionEnvs))
	}
	for i := range sessionEnvs {
		if recipeEnvs[i]["command"] != sessionEnvs[i]["command"] || recipeEnvs[i]["ok"] != sessionEnvs[i]["ok"] {
			t.Errorf("envelope %d differs: recipe %v vs session %v", i, recipeEnvs[i], sessionEnvs[i])
		}
	}
}

// VS-4: a missing required input is exit 2 and the browser is never contacted.
// VS-5, VS-6, VS-12 ride the same table: everything a recipe can get wrong
// statically fails before a connection exists.
func TestRecipeValidationNeverConnects(t *testing.T) {
	cases := map[string]struct {
		body string
		argv []string
		want string // substring of the error message
	}{
		"missing required input": { // VS-4
			body: "name: r\ninputs:\n  week: { required: true }\nsteps:\n  - run: [\"nav\", \"https://x.test/{{week}}\"]\n",
			argv: []string{"recipe", "run", "r"},
			want: "week",
		},
		"undeclared placeholder": { // VS-5
			body: "name: r\nsteps:\n  - run: [\"nav\", \"https://x.test/{{nope}}\"]\n",
			argv: []string{"recipe", "run", "r"},
			want: "not a declared input",
		},
		"unknown --set key": { // VS-6
			body: "name: r\ninputs:\n  hours: { default: \"8\" }\nsteps:\n  - run: [\"snap\"]\n",
			argv: []string{"recipe", "run", "r", "--set", "hurs=9"},
			want: "unknown input",
		},
		"--set without a value": {
			body: "name: r\nsteps:\n  - run: [\"snap\"]\n",
			argv: []string{"recipe", "run", "r", "--set", "hours"},
			want: "must be k=v",
		},
		"repeated --set": {
			body: "name: r\ninputs:\n  h: { default: \"1\" }\nsteps:\n  - run: [\"snap\"]\n",
			argv: []string{"recipe", "run", "r", "--set", "h=1", "--set", "h=2"},
			want: "more than once",
		},
		"malformed yaml": { // VS-12
			body: "name: r\nsteps: [ unclosed\n",
			argv: []string{"recipe", "run", "r"},
			want: "r.yaml",
		},
		"run is not an array": { // VS-12
			body: "name: r\nsteps:\n  - run: \"snap\"\n",
			argv: []string{"recipe", "run", "r"},
			want: "must be an array",
		},
		"recipe invoking a recipe": { // VS-12
			body: "name: r\nsteps:\n  - run: [\"recipe\", \"run\", \"other\"]\n",
			argv: []string{"recipe", "run", "r"},
			want: "cannot invoke recipes",
		},
		"--from-step out of range": {
			body: "name: r\nsteps:\n  - run: [\"snap\"]\n",
			argv: []string{"recipe", "run", "r", "--from-step", "5"},
			want: "out of range",
		},
		"unknown recipe": {
			body: "name: r\nsteps:\n  - run: [\"snap\"]\n",
			argv: []string{"recipe", "run", "absent"},
			want: "not found",
		},
		"unknown recipe on show": {
			body: "name: r\nsteps:\n  - run: [\"snap\"]\n",
			argv: []string{"recipe", "show", "absent"},
			want: "not found",
		},
		"a name that is a path": {
			body: "name: r\nsteps:\n  - run: [\"snap\"]\n",
			argv: []string{"recipe", "run", "../../etc/passwd"},
			want: "invalid recipe name",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			project, _ := isolateRecipes(t)
			writeRecipe(t, project, "r", c.body)

			var out, errb bytes.Buffer
			app := New(noCall(t), &out, &errb)
			if code := app.Execute(append(c.argv, "--json")...); code != 2 {
				t.Fatalf("exit = %d, want 2 (usage)\nstdout: %s", code, out.String())
			}
			env := envelopes(t, out.String())[0]
			e, ok := env["error"].(map[string]any)
			if !ok || e["code"] != "usage" {
				t.Fatalf("envelope = %v, want error.code usage", env)
			}
			if msg, _ := e["message"].(string); !strings.Contains(msg, c.want) {
				t.Errorf("message %q does not contain %q", msg, c.want)
			}
		})
	}
}

// VS-13: --from-step runs a suffix, and the summary says where it started, so a
// resumed run is distinguishable from a full one in a log.
func TestRecipeFromStep(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "four", `name: four
target: aa11
steps:
  - run: ["nav", "https://app.test/1"]
  - run: ["nav", "https://app.test/2"]
  - label: third
    run: ["text", "h1"]
  - run: ["wait", "--idle"]
`)
	b := &recordingBrowser{}
	var out, errb bytes.Buffer
	if code := New(b, &out, &errb).Execute("recipe", "run", "four", "--from-step", "3"); code != 0 {
		t.Fatalf("exit = %d\n%s", code, errb.String())
	}
	for _, c := range b.calls {
		if strings.HasPrefix(c, "Navigate(") {
			t.Errorf("step before --from-step ran: %s", c)
		}
	}
	envs := envelopes(t, out.String())
	if len(envs) != 3 {
		t.Fatalf("got %d envelopes, want 2 steps + summary:\n%s", len(envs), out.String())
	}
	if envs[0]["step"] != 3.0 {
		t.Errorf("first step envelope = %v, want step 3 (its index in the file)", envs[0])
	}
	res := envs[2]["result"].(map[string]any)
	if res["from_step"] != 3.0 || res["steps"] != 2.0 || res["completed"] != 2.0 {
		t.Errorf("summary = %v, want from_step 3, steps 2, completed 2", res)
	}
}

// VS-14: the scaffold `recipe new` writes must load, show, and dry-run cleanly.
// A template the validator rejects is a terrible first impression.
func TestRecipeNewScaffoldIsValid(t *testing.T) {
	isolateRecipes(t)

	var out, errb bytes.Buffer
	app := New(noCall(t), &out, &errb)
	if code := app.Execute("recipe", "new", "demo", "--json"); code != 0 {
		t.Fatalf("recipe new exit = %d\n%s", code, errb.String())
	}
	path, _ := envelopes(t, out.String())[0]["result"].(map[string]any)["path"].(string)
	if path == "" {
		t.Fatalf("recipe new did not report a path: %s", out.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("recipe new reported %s but %v", path, err)
	}

	out.Reset()
	if code := New(noCall(t), &out, &errb).Execute("recipe", "show", "demo"); code != 0 {
		t.Fatalf("recipe show exit = %d\n%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "name: demo") {
		t.Errorf("recipe show did not print the source:\n%s", out.String())
	}

	out.Reset()
	if code := New(noCall(t), &out, &errb).Execute("recipe", "run", "demo", "--dry-run"); code != 0 {
		t.Fatalf("recipe run --dry-run exit = %d\n%s", code, errb.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatalf("scaffold dry-run line is not argv: %q", line)
		}
	}
}

// `recipe new` refuses to clobber an existing recipe: the file may be the only
// copy of an automation someone worked out by hand.
func TestRecipeNewRefusesToOverwrite(t *testing.T) {
	isolateRecipes(t)
	var out, errb bytes.Buffer
	if code := New(noCall(t), &out, &errb).Execute("recipe", "new", "demo"); code != 0 {
		t.Fatalf("first recipe new exit = %d", code)
	}
	out.Reset()
	app := New(noCall(t), &out, &errb)
	if code := app.Execute("recipe", "new", "demo", "--json"); code != 2 {
		t.Fatalf("second recipe new exit = %d, want 2", code)
	}
}

// VS-11 at the CLI: the project copy wins, and `recipe list` marks the source of
// each entry so a user can see which one they are about to run.
func TestRecipeListMarksSourceAndPrecedence(t *testing.T) {
	project, user := isolateRecipes(t)
	writeRecipe(t, project, "shared", "name: shared\ndescription: from the project\nsteps:\n  - run: [\"snap\"]\n")
	writeRecipe(t, user, "shared", "name: shared\ndescription: from the user config\nsteps:\n  - run: [\"snap\"]\n")
	writeRecipe(t, user, "personal", "name: personal\ndescription: only mine\nsteps:\n  - run: [\"snap\"]\n")

	var out, errb bytes.Buffer
	app := New(noCall(t), &out, &errb)
	if code := app.Execute("recipe", "list", "--json"); code != 0 {
		t.Fatalf("exit = %d\n%s", code, errb.String())
	}
	rows := envelopes(t, out.String())[0]["result"].(map[string]any)["recipes"].([]any)
	got := map[string]string{}
	for _, r := range rows {
		row := r.(map[string]any)
		got[row["name"].(string)] = row["source"].(string) + ":" + row["description"].(string)
	}
	want := map[string]string{
		"shared":   "project:from the project",
		"personal": "user:only mine",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("list = %v, want %v", got, want)
	}

	// And the project copy is the one that runs.
	out.Reset()
	if code := New(noCall(t), &out, &errb).Execute("recipe", "show", "shared"); code != 0 {
		t.Fatalf("show exit = %d", code)
	}
	if !strings.Contains(out.String(), "from the project") {
		t.Errorf("show resolved the wrong copy:\n%s", out.String())
	}
}

// VS-15 end to end: a hostile --set value reaches the browser as one argv
// element, byte for byte. There is no shell in the path, and this is the test
// that says so at the boundary that matters.
func TestRecipeSetValuePassesThroughLiterally(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "inject", `name: inject
target: aa11
inputs:
  value: { required: true }
steps:
  - run: ["fill", "#field", "{{value}}"]
`)
	const hostile = "; rm -rf / && echo \"pwned\" `id` $(id) | tee /tmp/x\nsecond line\t"
	b := &recordingBrowser{}
	var out, errb bytes.Buffer
	if code := New(b, &out, &errb).Execute("recipe", "run", "inject", "--set", "value="+hostile); code != 0 {
		t.Fatalf("exit = %d\n%s", code, errb.String())
	}
	want := fmt.Sprintf("Fill(aa11,#field,%q,by=css,role=)", hostile)
	if len(b.calls) < 2 || b.calls[1] != want {
		t.Errorf("browser saw %v\nwant a single call %s", b.calls, want)
	}
}

// binaryBrowser's screenshot bytes contain newlines and a JSON-looking line —
// dumped into an NDJSON stream they do not merely look wrong, they parse wrong.
type binaryBrowser struct{ recordingBrowser }

func (b *binaryBrowser) Screenshot(context.Context, string, chrome.ShotOpts) ([]byte, map[string]any, error) {
	b.calls = append(b.calls, "Screenshot")
	return []byte("\x89PNG\r\n\x1a\n{\"ok\":true}\nIDAT\x00\x01"), nil, nil
}

var _ chrome.Browser = (*binaryBrowser)(nil)

// A step that writes raw bytes to stdout fails the step instead of corrupting
// the recipe's NDJSON.
//
// `screenshot -o -` writes its image straight to stdout and emits no envelope,
// so its bytes landed in the middle of the stream and every later line became
// unparseable for whoever was reading it.
func TestRecipeStepWritingRawBytesFailsTheStep(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "shot", `name: shot
target: aa11
steps:
  - run: ["screenshot", "-o", "-"]
  - label: never runs
    run: ["text", "h1"]
`)
	b := &binaryBrowser{}
	var out, errb bytes.Buffer
	code := New(b, &out, &errb).Execute("recipe", "run", "shot")

	// envelopes() fails on any line that is not one JSON value, which is the
	// assertion that matters: the stream stayed parseable.
	envs := envelopes(t, out.String())
	if len(envs) != 2 {
		t.Fatalf("got %d envelopes, want the failed step + summary:\n%q", len(envs), out.String())
	}
	if envs[0]["ok"] != false || envs[0]["step"] != 1.0 {
		t.Errorf("step envelope = %v, want a failed step 1", envs[0])
	}
	if msg, _ := envs[0]["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "envelope") {
		t.Errorf("step message = %q, want it to explain the raw output", msg)
	}
	if code != 2 {
		t.Errorf("exit = %d, want 2 (usage): a step that cannot run in a recipe is the author's mistake", code)
	}
	for _, c := range b.calls {
		if strings.HasPrefix(c, "Text") {
			t.Errorf("the step after the failure ran: %v", b.calls)
		}
	}
}

// The summary's error.code and the process exit code must agree: a caller that
// branches on the exit and a caller that reads the envelope have to reach the
// same conclusion. envCode fell back to `generic` (exit 1) whenever a step
// emitted no envelope, while the process still exited with the step's own code.
func TestRecipeSummaryCodeMatchesTheExit(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "shot", "name: shot\ntarget: aa11\nsteps:\n  - run: [\"screenshot\", \"-o\", \"-\"]\n")

	var out, errb bytes.Buffer
	code := New(&binaryBrowser{}, &out, &errb).Execute("recipe", "run", "shot")
	// The summary is the last line; read it directly rather than through
	// envelopes(), which would fatal on the raw bytes this used to emit before
	// reaching the assertion that matters.
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	var summary map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &summary); err != nil {
		t.Fatalf("last line is not the summary envelope: %q", lines[len(lines)-1])
	}
	errObj, _ := summary["error"].(map[string]any)
	got, _ := errObj["code"].(string)
	if want := codeForExit(code); got != want {
		t.Errorf("summary error.code = %q but the process exited %d (which is %q)", got, code, want)
	}
	failed, _ := summary["result"].(map[string]any)["failed"].(map[string]any)
	if failed == nil || failed["code"] != got {
		t.Errorf("failed = %v, summary error.code = %q; they must name the same code", failed, got)
	}
}

// codeForExit's inverse has to hold for every documented exit code, or the
// summary would name a code that maps somewhere else.
func TestCodeForExitRoundTrips(t *testing.T) {
	t.Parallel()
	for _, doc := range result.ExitCodes() {
		if doc.Code == result.ExitOK {
			continue
		}
		if got := result.ExitCodeFor(codeForExit(doc.Code)); got != doc.Code {
			t.Errorf("codeForExit(%d) = %q, which maps back to %d", doc.Code, codeForExit(doc.Code), got)
		}
	}
}

// runPlan folds the run's connection flags into a.defaults so they survive into
// each step's Execute — and has to put them back. They used to leak into every
// later `session` line, which is the mutation the RFC guidance names outright:
// do not cache flag-derived state on App across invocations.
func TestRecipeRestoresTheAppDefaults(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "three", threeStep)

	var out, errb bytes.Buffer
	app := New(&recordingBrowser{}, &out, &errb)
	before := app.defaults
	if code := app.Execute("recipe", "run", "three", "--timeout", "5s", "--no-daemon", "--port", "1234"); code != 0 {
		t.Fatalf("exit = %d\n%s", code, errb.String())
	}
	if !reflect.DeepEqual(app.defaults, before) {
		t.Errorf("a.defaults after the run = %+v\nwant it restored to %+v", app.defaults, before)
	}
	// inSession is borrowed the same way and must come back too.
	if app.inSession || app.inRecipe {
		t.Errorf("inSession = %v, inRecipe = %v after the run; both must be restored", app.inSession, app.inRecipe)
	}
}

// A streaming step is the same usage error inside a recipe that it is inside
// `session`.
//
// A recipe run is a batch with the same one-envelope-per-line contract, but
// runPlan never marked it as one — so `console --follow` was accepted, blocked
// for the whole --timeout, buffered every streamed envelope into execStep's
// in-memory buffer, and then failed to parse as a single envelope and dumped
// the raw stream with no step or label on it.
func TestRecipeRefusesAStreamingStep(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "stream", `name: stream
target: aa11
steps:
  - run: ["console", "--follow"]
    on_error: continue
  - run: ["net", "--follow"]
`)
	var out, errb bytes.Buffer
	// noCall fails the test on any browser call: the refusal is validation, and
	// validation happens before anything connects.
	app := New(noCall(t), &out, &errb)
	if code := app.Execute("recipe", "run", "stream"); code != 2 {
		t.Fatalf("exit = %d, want 2 (usage)\nstdout: %s\nstderr: %s", code, out.String(), errb.String())
	}
	envs := envelopes(t, out.String())
	if len(envs) != 3 {
		t.Fatalf("got %d envelopes, want 2 steps + summary:\n%s", len(envs), out.String())
	}
	for i, e := range envs[:2] {
		errObj, ok := e["error"].(map[string]any)
		if !ok || errObj["code"] != "usage" {
			t.Errorf("step %d = %v, want a usage error", i+1, e)
			continue
		}
		if !strings.Contains(errObj["message"].(string), "--follow cannot run") {
			t.Errorf("step %d message = %q, want it to name --follow", i+1, errObj["message"])
		}
	}
}

// A --set value that looks like a flag arrives as data, and cannot move the
// step to a different tab.
//
// The substituted argv used to be handed straight to cobra, which parses flags
// at any position: `--set sel=--target=@2` on a step whose selector is
// `{{sel}}` suppressed the recipe's pinned `target:` and read a DIFFERENT tab,
// then dumped its text. RFC-0009 promises an input substitutes into one argv
// element and never into a command line; that has to hold for flag parsing and
// not only for word splitting.
func TestRecipeSetValueThatLooksLikeAFlagIsData(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "pinned", `name: pinned
target: aa11
inputs:
  sel: { required: true }
steps:
  - run: ["text", "{{sel}}"]
`)
	for _, hostile := range []string{"--target=@2", "--target", "--policy-off", "--no-daemon", "-v", "--json"} {
		b := &twoTabBrowser{}
		var out, errb bytes.Buffer
		New(b, &out, &errb).Execute("recipe", "run", "pinned", "--set", "sel="+hostile)

		if len(b.texts) != 1 {
			t.Errorf("--set sel=%s: Text called %d times, want once: %v\n%s", hostile, len(b.texts), b.texts, out.String())
			continue
		}
		if got := b.texts[0]; got != "aa11:"+hostile {
			t.Errorf("--set sel=%s: step read %q, want the pinned tab aa11 and the value as a literal selector", hostile, got)
		}
	}
}

// The splitter has to agree with pflag about what a flag is: an element in the
// wrong section either reaches flag position (the bug) or makes a working
// recipe fail. It is checked against the real command tree.
func TestSplitStepArgv(t *testing.T) {
	t.Parallel()
	sel := func(argv []string, idx []int) []string {
		out := make([]string, 0, len(idx))
		for _, i := range idx {
			out = append(out, argv[i])
		}
		return out
	}
	cases := map[string]struct {
		argv  []string
		flags []string
		pos   []string
		ok    bool
	}{
		"a verb and its selector":    {[]string{"text", "h1"}, []string{"text"}, []string{"h1"}, true},
		"a value flag takes its arg": {[]string{"click", "--by", "name", "Save"}, []string{"click", "--by", "name"}, []string{"Save"}, true},
		"an inline value flag":       {[]string{"click", "--by=name", "Save"}, []string{"click", "--by=name"}, []string{"Save"}, true},
		"a bool flag takes nothing":  {[]string{"wait", "--idle"}, []string{"wait", "--idle"}, nil, true},
		"a subcommand path":          {[]string{"cookie", "set", "k", "v"}, []string{"cookie", "set"}, []string{"k", "v"}, true},
		"a shorthand bool":           {[]string{"text", "-q", "h1"}, []string{"text", "-q"}, []string{"h1"}, true},
		"the step's own terminator":  {[]string{"text", "--", "-h1"}, []string{"text"}, []string{"-h1"}, true},
		// A positional that reads like a subcommand of a DIFFERENT command is
		// still data: `text` has no children.
		"a selector named like a command": {[]string{"text", "list"}, []string{"text"}, []string{"list"}, true},
		// An unknown flag is assumed to take no value, so its neighbour stays
		// data rather than being swallowed by a typo.
		"an unknown flag": {[]string{"text", "--nope", "h1"}, []string{"text", "--nope"}, []string{"h1"}, true},
		// Nothing to classify against: Resolve leaves the argv alone and lets
		// cobra report it.
		"an unknown verb": {[]string{"bogus", "x"}, nil, nil, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// A tree per subtest: cobra's Commands() sorts its child slice in
			// place, so a shared root would be a data race between parallel
			// subtests — the same reason stepSplitter builds a fresh one.
			root := (&App{defaults: config.Builtin()}).newRoot()
			flagIdx, posIdx, ok := splitStepArgv(root, c.argv)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			if got := sel(c.argv, flagIdx); !reflect.DeepEqual(got, c.flags) {
				t.Errorf("flags = %#v, want %#v", got, c.flags)
			}
			if got := sel(c.argv, posIdx); len(got) != 0 || len(c.pos) != 0 {
				if !reflect.DeepEqual(got, c.pos) {
					t.Errorf("positionals = %#v, want %#v", got, c.pos)
				}
			}
		})
	}
}

// twoTabBrowser serves two tabs and records which one Text was asked about,
// with what selector — the two facts a substituted flag could change.
type twoTabBrowser struct {
	stubBrowser
	texts []string
}

func (b *twoTabBrowser) List(context.Context) ([]target.Info, error) {
	return []target.Info{
		{ID: "aa11", Title: "Payroll", URL: "https://payroll.corp/"},
		{ID: "bb22", Title: "Other", URL: "https://other.test/"},
	}, nil
}

func (b *twoTabBrowser) Text(_ context.Context, id, sel string, _ chrome.TextOpts) (map[string]any, error) {
	b.texts = append(b.texts, id+":"+sel)
	return map[string]any{"text": "secret"}, nil
}

var _ chrome.Browser = (*twoTabBrowser)(nil)

// Open question 2, resolved: streaming per-step for interactive use, a single
// summary under --quiet.
func TestRecipeQuietEmitsOnlyTheSummary(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "three", threeStep)

	var out, errb bytes.Buffer
	if code := New(&recordingBrowser{}, &out, &errb).Execute("recipe", "run", "three", "--quiet"); code != 0 {
		t.Fatalf("exit = %d\n%s", code, errb.String())
	}
	envs := envelopes(t, out.String())
	if len(envs) != 1 || envs[0]["command"] != "recipe" {
		t.Fatalf("--quiet emitted %d envelopes, want just the summary:\n%s", len(envs), out.String())
	}
	if res := envs[0]["result"].(map[string]any); res["completed"] != 3.0 {
		t.Errorf("summary = %v, want completed 3 (the run itself is unchanged)", res)
	}
}

// An explicit --target on the run overrides the recipe's header for every step,
// so one recipe can be pointed at a staging tab without editing the file.
func TestRecipeRunTargetOverridesHeader(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "t", "name: t\ntarget: url:nowhere\nsteps:\n  - run: [\"text\", \"h1\"]\n")

	var out, errb bytes.Buffer
	if code := New(&recordingBrowser{}, &out, &errb).Execute("recipe", "run", "t", "--target", "aa11"); code != 0 {
		t.Fatalf("exit = %d\n%s", code, errb.String())
	}
	if !strings.Contains(out.String(), `"text":"hello"`) {
		t.Errorf("the override did not take effect:\n%s", out.String())
	}
}

// A recipe is an ordinary verb, so `session` can run one as a line — the
// nesting that matters, since the reverse (a recipe invoking `session`) is
// refused at load. The step envelopes still stream out in order.
func TestSessionCanRunARecipe(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "three", threeStep)

	var out, errb bytes.Buffer
	app := New(&recordingBrowser{}, &out, &errb).WithInput(strings.NewReader(`["recipe","run","three"]` + "\n"))
	if code := app.Execute("session"); code != 0 {
		t.Fatalf("exit = %d\n%s", code, errb.String())
	}
	envs := envelopes(t, out.String())
	if len(envs) != 4 {
		t.Fatalf("got %d envelopes, want 3 steps + summary:\n%s", len(envs), out.String())
	}
	if res := envs[3]["result"].(map[string]any); res["completed"] != 3.0 {
		t.Errorf("summary = %v, want completed 3", res)
	}
}

// Recursion is a runner property, not a file property: the load-time
// reserved-verb check reads argv[0] of one recipe and cannot see what a step's
// command re-enters. A `recipe run` reached from inside a running recipe is
// refused before any step executes.
func TestRecipeRefusesToRunInsideARunningRecipe(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "three", threeStep)

	b := &recordingBrowser{}
	var out, errb bytes.Buffer
	app := New(b, &out, &errb)
	// As if a step had re-entered the command tree — which is what the leading
	// flag in `["--json","recipe","run","three"]` used to do, forever.
	app.inRecipe = true
	code := app.Execute("recipe", "run", "three")

	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage)\nstdout: %s\nstderr: %s", code, out.String(), errb.String())
	}
	if len(b.calls) != 0 {
		t.Errorf("browser was contacted %v; a refused recursion must run nothing", b.calls)
	}
	if !strings.Contains(errb.String(), "cannot run another recipe") {
		t.Errorf("stderr = %q, want it to name the recursion", errb.String())
	}
}

// `recipe show --json` returns the parsed recipe, so an agent can read the
// inputs it must supply without parsing YAML itself.
func TestRecipeShowJSON(t *testing.T) {
	project, _ := isolateRecipes(t)
	writeRecipe(t, project, "three", threeStep)

	var out, errb bytes.Buffer
	app := New(noCall(t), &out, &errb)
	if code := app.Execute("recipe", "show", "three", "--json"); code != 0 {
		t.Fatalf("exit = %d\n%s", code, errb.String())
	}
	r := envelopes(t, out.String())[0]["result"].(map[string]any)["recipe"].(map[string]any)
	if r["name"] != "three" || r["source"] != "project" {
		t.Errorf("recipe = %v, want name three from source project", r)
	}
	if steps, ok := r["steps"].([]any); !ok || len(steps) != 3 {
		t.Errorf("steps = %v, want 3", r["steps"])
	}
}
