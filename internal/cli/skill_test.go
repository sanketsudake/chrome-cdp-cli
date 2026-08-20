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
	refs, err := skills.References()
	if err != nil {
		t.Fatalf("References() error: %v", err)
	}
	for _, want := range refs {
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

// TestSkillListJSONListsSkillsAndReferences proves `skill list --json` lists
// both the three skills and drive-chrome-cdp's references, in the documented
// shape.
func TestSkillListJSONListsSkillsAndReferences(t *testing.T) {
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
	if res["name"] != "drive-chrome-cdp" {
		t.Errorf("result.name = %v, want drive-chrome-cdp", res["name"])
	}
	skillsList, ok := res["skills"].([]any)
	if !ok {
		t.Fatalf("result.skills = %v, want an array", res["skills"])
	}
	wantSkills, err := skills.Skills()
	if err != nil {
		t.Fatalf("Skills() error: %v", err)
	}
	if len(skillsList) != len(wantSkills) {
		t.Fatalf("result.skills = %v, want %v", skillsList, wantSkills)
	}
	for i, name := range wantSkills {
		if skillsList[i] != name {
			t.Errorf("result.skills[%d] = %v, want %q", i, skillsList[i], name)
		}
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
}

// TestSkillListHumanShowsSkillsThenReferences proves the human-mode `skill
// list` output lists skills first, drive-chrome-cdp first among them, then a
// blank line, then the references.
func TestSkillListHumanShowsSkillsThenReferences(t *testing.T) {
	t.Parallel()
	var out, errb strings.Builder
	app := New(noCall(t), &out, &errb)
	code := app.Execute("skill", "list")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) == 0 || lines[0] != "drive-chrome-cdp" {
		t.Fatalf("skill list first line = %q, want drive-chrome-cdp\nfull output:\n%s", lines[0], out.String())
	}
	blankIdx := -1
	for i, l := range lines {
		if l == "" {
			blankIdx = i
			break
		}
	}
	if blankIdx == -1 {
		t.Fatalf("skill list human output has no blank separator line:\n%s", out.String())
	}
	skillNames := lines[:blankIdx]
	refNames := lines[blankIdx+1:]
	wantSkillNames, err := skills.Skills()
	if err != nil {
		t.Fatalf("Skills() error: %v", err)
	}
	for _, want := range wantSkillNames {
		var found bool
		for _, got := range skillNames {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("skill list skills section missing %q: %v", want, skillNames)
		}
	}
	wantRefNames, err := skills.References()
	if err != nil {
		t.Fatalf("References() error: %v", err)
	}
	for _, want := range wantRefNames {
		var found bool
		for _, got := range refNames {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("skill list references section missing %q: %v", want, refNames)
		}
	}
}

// TestSkillGetResolvesSkillName proves `skill get <name>` resolves a bare
// skill name (not a drive-chrome-cdp reference) to that skill's SKILL.md.
func TestSkillGetResolvesSkillName(t *testing.T) {
	t.Parallel()
	var out, errb strings.Builder
	app := New(noCall(t), &out, &errb)
	code := app.Execute("skill", "get", "check-logged-in")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "---\nname: check-logged-in\n") {
		t.Fatalf("stdout does not start with check-logged-in's frontmatter: %q", firstN(out.String(), 60))
	}
}

// TestSkillGetResolvesSkillSlashReference proves `skill get
// drive-chrome-cdp/core` resolves the explicit <skill>/<reference> form.
func TestSkillGetResolvesSkillSlashReference(t *testing.T) {
	t.Parallel()
	var out, errb strings.Builder
	app := New(noCall(t), &out, &errb)
	code := app.Execute("skill", "get", "drive-chrome-cdp/core")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "# Core") {
		t.Fatalf("stdout does not start with the core reference's heading: %q", firstN(out.String(), 40))
	}
}

// TestSkillGetJSONShape proves `skill get`'s JSON envelope carries the
// documented {name, reference, content} shape, with reference empty for a
// bare skill/reference name.
func TestSkillGetJSONShape(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	app := New(noCall(t), &out, &errb)
	code := app.Execute("skill", "get", "core", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, errb.String())
	}
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not one JSON value: %v\n%s", err, out.String())
	}
	res := env["result"].(map[string]any)
	if res["name"] != "drive-chrome-cdp" {
		t.Errorf("result.name = %v, want drive-chrome-cdp", res["name"])
	}
	if res["reference"] != "core" {
		t.Errorf("result.reference = %v, want core", res["reference"])
	}
	if _, ok := res["content"].(string); !ok {
		t.Errorf("result.content = %v, want a string", res["content"])
	}
}

// TestSkillGetUnknownSkillSlashReferenceIsUsageError proves an unresolvable
// <skill>/<reference> form is a usage error, not a browser call.
func TestSkillGetUnknownSkillSlashReferenceIsUsageError(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	app := New(noCall(t), &out, &errb)
	code := app.Execute("skill", "get", "nope/nope", "--json")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage); stderr=%s", code, errb.String())
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
