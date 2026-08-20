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

// normalizeLineEndings rewrites CRLF to LF. A Windows checkout of this repo
// (actions/checkout's default autocrlf) embeds skills/*/SKILL.md and
// references/*.md with \r\n; every read path routes through this helper so
// what `chrome-cdp skill` prints never depends on the checkout's line
// endings.
func normalizeLineEndings(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

// Core returns the stub SKILL.md joined with references/core.md — the core
// loop an agent needs before it reaches for anything else.
func Core() ([]byte, error) {
	stub, err := FS.ReadFile(dir + "/SKILL.md")
	if err != nil {
		return nil, err
	}
	stub = normalizeLineEndings(stub)
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
	refs, err := References()
	if err != nil {
		return nil, err
	}
	for _, name := range refs {
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

// readNamed reads path under an escape guard on name, shared by Reference and
// Skill: both take a bare name from the caller (never a path), reject
// anything that could climb out of the embedded FS, and report the same
// "unknown <noun> %q" whether the guard or the read itself is what failed —
// a caller must not learn from the error whether the name was well-formed.
func readNamed(name, path, noun string) ([]byte, error) {
	if strings.ContainsAny(name, "/\\.") {
		return nil, fmt.Errorf("unknown %s %q", noun, name)
	}
	b, err := FS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("unknown %s %q", noun, name)
	}
	return normalizeLineEndings(b), nil
}

// Reference returns one reference file's content by name (without ".md").
func Reference(name string) ([]byte, error) {
	return readNamed(name, dir+"/references/"+name+".md", "reference")
}

// References returns every reference name, sorted.
func References() ([]string, error) {
	entries, err := fs.ReadDir(FS, dir+"/references")
	if err != nil {
		return nil, fmt.Errorf("read embedded references: %w", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".md"))
	}
	sort.Strings(names)
	return names, nil
}

// Skills returns the name of every embedded skill directory (one that
// carries a SKILL.md), sorted.
func Skills() ([]string, error) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded skills: %w", err)
	}
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
	return names, nil
}

// Skill returns one skill's SKILL.md content by directory name.
func Skill(name string) ([]byte, error) {
	return readNamed(name, name+"/SKILL.md", "skill")
}
