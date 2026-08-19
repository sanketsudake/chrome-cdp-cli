package skills

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestEmbeddedSkillHasFrontmatterAndReferences(t *testing.T) {
	t.Parallel()
	core, err := Core()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(core, []byte("---\nname: drive-chrome-cdp\n")) {
		t.Fatalf("core does not start with the skill frontmatter: %q", core[:40])
	}
	full, _ := Full()
	if len(full) <= len(core) {
		t.Fatal("Full() must include the references")
	}
	for _, want := range []string{"core", "debugging", "batch-and-recipes", "widgets"} {
		if _, err := Reference(want); err != nil {
			t.Fatalf("missing reference %s: %v", want, err)
		}
	}
	if _, err := Reference("nope"); err == nil {
		t.Fatal("unknown reference must error")
	}
}

// TestReferenceRejectsPathEscapes proves Reference refuses names that try to
// escape the references/ directory or already carry the .md suffix.
func TestReferenceRejectsPathEscapes(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"../core", "core.md", "sub/core"} {
		if _, err := Reference(bad); err == nil {
			t.Errorf("Reference(%q) = nil error, want rejection", bad)
		}
	}
}

// TestSkillsListsAllSkillDirs proves Skills() finds every skill directory
// that carries a SKILL.md, sorted by name.
func TestSkillsListsAllSkillDirs(t *testing.T) {
	t.Parallel()
	want := []string{"check-logged-in", "drive-chrome-cdp", "fill-grid-and-confirm"}
	got := Skills()
	if len(got) != len(want) {
		t.Fatalf("Skills() = %v, want %v", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("Skills()[%d] = %q, want %q (Skills()=%v)", i, got[i], name, got)
		}
	}
}

// TestSkillReturnsEachSkillDoc proves Skill(name) returns that skill's
// SKILL.md, frontmatter first.
func TestSkillReturnsEachSkillDoc(t *testing.T) {
	t.Parallel()
	for name, prefix := range map[string]string{
		"check-logged-in":       "---\nname: check-logged-in\n",
		"fill-grid-and-confirm": "---\nname: fill-grid-and-confirm\n",
	} {
		b, err := Skill(name)
		if err != nil {
			t.Fatalf("Skill(%q): %v", name, err)
		}
		if !bytes.HasPrefix(b, []byte(prefix)) {
			t.Errorf("Skill(%q) does not start with %q: %q", name, prefix, b[:min(len(b), 60)])
		}
	}
	if _, err := Skill("nope"); err == nil {
		t.Fatal("Skill(\"nope\") = nil error, want rejection")
	}
}

// TestSkillRejectsPathEscapes proves Skill refuses names that try to escape
// the skills tree.
func TestSkillRejectsPathEscapes(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"../drive-chrome-cdp", "drive-chrome-cdp/SKILL.md", "sub/dir"} {
		if _, err := Skill(bad); err == nil {
			t.Errorf("Skill(%q) = nil error, want rejection", bad)
		}
	}
}

// TestEvalsJSONParsesWithThreeEntries proves the evals file is valid JSON in
// the documented shape with exactly three golden tasks.
func TestEvalsJSONParsesWithThreeEntries(t *testing.T) {
	t.Parallel()
	b, err := FS.ReadFile("drive-chrome-cdp/evals/evals.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SkillName string `json:"skill_name"`
		Evals     []struct {
			ID             string   `json:"id"`
			Name           string   `json:"name"`
			Prompt         string   `json:"prompt"`
			ExpectedOutput string   `json:"expected_output"`
			Files          []string `json:"files"`
		} `json:"evals"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("evals.json does not parse: %v", err)
	}
	if doc.SkillName != "drive-chrome-cdp" {
		t.Errorf("skill_name = %q, want drive-chrome-cdp", doc.SkillName)
	}
	if len(doc.Evals) != 3 {
		t.Fatalf("len(evals) = %d, want 3", len(doc.Evals))
	}
	for i, e := range doc.Evals {
		if e.ID == "" || e.Name == "" || e.Prompt == "" || e.ExpectedOutput == "" {
			t.Errorf("evals[%d] has an empty required field: %+v", i, e)
		}
	}
}
