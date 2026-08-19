package skills

import (
	"bytes"
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
