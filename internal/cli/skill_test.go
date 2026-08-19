package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/skills"
)

// TestSkillPrintsCore proves the bare verb writes the raw core doc — no
// envelope — to stdout in human mode.
func TestSkillPrintsCore(t *testing.T) {
	t.Parallel()
	var out, errb strings.Builder
	app := New(noCall(t), &out, &errb)
	code := app.Execute("skill")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "---\nname: drive-chrome-cdp\n") {
		t.Fatalf("stdout does not start with the skill frontmatter: %q", firstN(out.String(), 60))
	}
}

// TestSkillFullIncludesReferences proves --full pulls in more than the core
// doc alone.
func TestSkillFullIncludesReferences(t *testing.T) {
	t.Parallel()
	var core, full strings.Builder
	var errb strings.Builder
	New(noCall(t), &core, &errb).Execute("skill")
	New(noCall(t), &full, &errb).Execute("skill", "--full")
	if len(full.String()) <= len(core.String()) {
		t.Fatalf("skill --full (%d bytes) must be longer than skill (%d bytes)", len(full.String()), len(core.String()))
	}
}

// TestSkillJSONEnvelope proves --json emits the documented envelope shape and
// that no browser was contacted.
func TestSkillJSONEnvelope(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	app := New(noCall(t), &out, &errb)
	code := app.Execute("skill", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not one JSON value: %v\n%s", err, out.String())
	}
	if env["ok"] != true || env["command"] != "skill" {
		t.Fatalf("envelope = %v", env)
	}
	res, ok := env["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want an object", env["result"])
	}
	if res["name"] != "drive-chrome-cdp" {
		t.Errorf("result.name = %v, want drive-chrome-cdp", res["name"])
	}
	refs, ok := res["references"].([]any)
	if !ok {
		t.Fatalf("result.references = %v, want an array", res["references"])
	}
	var sawCore bool
	for _, r := range refs {
		if r == "core" {
			sawCore = true
		}
	}
	if !sawCore {
		t.Errorf("result.references = %v, want it to contain \"core\"", refs)
	}
	content, ok := res["content"].(string)
	if !ok || !strings.HasPrefix(content, "---\nname: drive-chrome-cdp\n") {
		t.Errorf("result.content = %q, want it to start with the skill frontmatter", firstN(fmt.Sprint(res["content"]), 60))
	}
}

// TestSkillListPrintsReferenceNames proves `skill list` enumerates the
// reference files, one per line in human mode.
func TestSkillListPrintsReferenceNames(t *testing.T) {
	t.Parallel()
	var out, errb strings.Builder
	app := New(noCall(t), &out, &errb)
	code := app.Execute("skill", "list")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	for _, want := range skills.References() {
		if !strings.Contains(out.String(), want) {
			t.Errorf("skill list output missing %q:\n%s", want, out.String())
		}
	}
}

// TestSkillListJSON proves the JSON shape of `skill list`.
func TestSkillListJSON(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	app := New(noCall(t), &out, &errb)
	code := app.Execute("skill", "list", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not one JSON value: %v\n%s", err, out.String())
	}
	res := env["result"].(map[string]any)
	if _, ok := res["content"]; ok {
		t.Errorf("skill list result carries content: %v, want list to omit it", res)
	}
	if _, ok := res["references"].([]any); !ok {
		t.Errorf("skill list result.references = %v, want an array", res["references"])
	}
}

// TestSkillGetPrintsReference proves `skill get <name>` prints that
// reference's raw content.
func TestSkillGetPrintsReference(t *testing.T) {
	t.Parallel()
	var out, errb strings.Builder
	app := New(noCall(t), &out, &errb)
	code := app.Execute("skill", "get", "core")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "# Core") {
		t.Fatalf("stdout does not start with the core reference's heading: %q", firstN(out.String(), 40))
	}
}

// TestSkillGetUnknownReferenceIsUsageError proves an unknown reference name
// fails validation before any browser is contacted.
func TestSkillGetUnknownReferenceIsUsageError(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	app := New(noCall(t), &out, &errb)
	code := app.Execute("skill", "get", "nope", "--json")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage); stderr=%s", code, errb.String())
	}
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not one JSON value: %v\n%s", err, out.String())
	}
	if env["ok"] != false {
		t.Fatalf("ok = %v, want false", env["ok"])
	}
	if code := env["error"].(map[string]any)["code"]; code != "usage" {
		t.Errorf("error.code = %v, want usage", code)
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
