// Package skills embeds the agent skills so `chrome-cdp skill` serves docs
// that match the installed binary.
package skills

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed */SKILL.md */references/*.md */evals/*.json
var FS embed.FS

const dir = "drive-chrome-cdp"

// Core returns the stub SKILL.md joined with references/core.md — the core
// loop an agent needs before it reaches for anything else.
func Core() ([]byte, error) {
	stub, err := FS.ReadFile(dir + "/SKILL.md")
	if err != nil {
		return nil, err
	}
	core, err := Reference("core")
	if err != nil {
		return nil, err
	}
	return bytes.Join([][]byte{bytes.TrimRight(stub, "\n"), core}, []byte("\n\n")), nil
}

// Full returns Core plus every other reference, in name order, each preceded
// by a `---` separator.
func Full() ([]byte, error) {
	out, err := Core()
	if err != nil {
		return nil, err
	}
	for _, name := range References() {
		if name == "core" {
			continue
		}
		b, err := Reference(name)
		if err != nil {
			return nil, err
		}
		out = append(out, []byte("\n---\n")...)
		out = append(out, b...)
	}
	return out, nil
}

// Reference returns one reference file's content by name (without ".md").
func Reference(name string) ([]byte, error) {
	if strings.ContainsAny(name, "/\\.") {
		return nil, fmt.Errorf("unknown reference %q", name)
	}
	b, err := FS.ReadFile(dir + "/references/" + name + ".md")
	if err != nil {
		return nil, fmt.Errorf("unknown reference %q", name)
	}
	return b, nil
}

// References returns every reference name, sorted.
func References() []string {
	entries, _ := fs.ReadDir(FS, dir+"/references")
	var names []string
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".md"))
	}
	sort.Strings(names)
	return names
}

// Skills returns the name of every embedded skill directory (one that
// carries a SKILL.md), sorted.
func Skills() []string {
	entries, _ := fs.ReadDir(FS, ".")
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := fs.Stat(FS, e.Name()+"/SKILL.md"); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// Skill returns one skill's SKILL.md content by directory name.
func Skill(name string) ([]byte, error) {
	if strings.ContainsAny(name, "/\\.") {
		return nil, fmt.Errorf("unknown skill %q", name)
	}
	b, err := FS.ReadFile(name + "/SKILL.md")
	if err != nil {
		return nil, fmt.Errorf("unknown skill %q", name)
	}
	return b, nil
}
