// Package config loads optional persistent defaults from a TOML file and merges
// them with CHROME_CDP_* environment variables into the effective global-flag
// defaults. Precedence, highest first: explicit flags > env > config file >
// built-in defaults. Cobra applies the flags; this package resolves the
// env > config > built-in portion and hands the result to the command tree.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Defaults are the effective global-flag defaults after the config file and
// CHROME_CDP_* env are merged over the built-in values.
type Defaults struct {
	Timeout    time.Duration
	By         string
	Wait       string
	Target     string
	Port       int
	ProfileDir string
	NoLaunch   bool
	NoDaemon   bool
	JSON       bool
	NoColor    bool

	// Policy is the optional [policy] table (RFC-0012). It is not overridable
	// by CHROME_CDP_* env vars: a safety boundary that an inherited environment
	// could widen would not be much of a boundary. Override it explicitly with
	// --allow / --policy-off instead.
	Policy Policy
}

// Policy mirrors the [policy] table. It is raw, unvalidated data — parsing the
// patterns is internal/policy's job — but it does record enough for the CLI to
// refuse to run rather than run wide open (see Malformed).
type Policy struct {
	// Present reports that a [policy] table exists at all. With no table the
	// whole layer is inert and nothing about the CLI changes.
	Present bool
	// Enabled defaults to true for a present table: a user who wrote the table
	// meant it. Set enabled = false to keep it on file without applying it.
	Enabled bool

	Allow       []string
	Deny        []string
	ReadOnly    []string
	VerbsDenied []string
	UploadRoots []string

	AuditLog    string
	AuditAll    bool
	OnViolation string

	// Malformed carries the reason a policy table could not be read: either the
	// file did not parse at all while mentioning [policy], or the table held a
	// key this build does not know.
	//
	// It exists because the repo's usual "warn and continue" is the wrong answer
	// here. Continuing means running with a policy the user believes is in force
	// and is not, and a policy that fails open is worse than no policy — so the
	// CLI turns this into a refusal (RFC-0012 VS-15).
	Malformed string

	// Source is the config file the table came from, echoed in refusals so the
	// user knows which file to edit.
	Source string
}

// Builtin returns the hard-coded defaults used when neither the config file nor
// the environment sets a value.
func Builtin() Defaults {
	return Defaults{Timeout: 30 * time.Second, By: "css", Wait: "visible"}
}

// file mirrors the TOML schema; pointer fields distinguish "set in file" from
// "absent", so an omitted key leaves the built-in (or env) value intact.
type file struct {
	Timeout    *string `toml:"timeout"`
	By         *string `toml:"by"`
	Wait       *string `toml:"wait"`
	Target     *string `toml:"target"`
	Port       *int    `toml:"port"`
	ProfileDir *string `toml:"profile_dir"`
	NoLaunch   *bool   `toml:"no_launch"`
	NoDaemon   *bool   `toml:"no_daemon"`
	JSON       *bool   `toml:"json"`
	NoColor    *bool   `toml:"no_color"`

	Policy *policyFile `toml:"policy"`
}

// policyFile mirrors the [policy] table.
type policyFile struct {
	Enabled     *bool    `toml:"enabled"`
	Allow       []string `toml:"allow"`
	Deny        []string `toml:"deny"`
	ReadOnly    []string `toml:"read_only"`
	VerbsDenied []string `toml:"verbs_denied"`
	UploadRoots []string `toml:"upload_roots"`
	AuditLog    *string  `toml:"audit_log"`
	AuditAll    *bool    `toml:"audit_all"`
	OnViolation *string  `toml:"on_violation"`
}

// Path returns the config file location under $XDG_CONFIG_HOME (or ~/.config).
func Path() string { return pathFrom(os.Getenv) }

func pathFrom(getenv func(string) string) string {
	base := getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "chrome-cdp", "config.toml")
}

// Resolve returns the effective flag defaults from the real config path and
// environment. A malformed config yields a non-nil error but still-usable
// defaults (built-ins plus env), so a bad file never bricks the CLI.
func Resolve() (Defaults, error) {
	return ResolveFrom(Path(), os.Getenv)
}

// ResolveFrom is Resolve with the config path and environment injected, for
// testing. It overlays the config file, then env, onto the built-in defaults.
func ResolveFrom(path string, getenv func(string) string) (Defaults, error) {
	d := Builtin()
	err := applyFile(&d, path)
	applyEnv(&d, getenv)
	return d, err
}

// FromEnv returns the built-in defaults overlaid with CHROME_CDP_* env vars only
// (no config file). The daemon subprocess uses it: the parent already folded the
// config file into the environment it hands down, so parsing stays in one place.
func FromEnv() Defaults {
	d := Builtin()
	applyEnv(&d, os.Getenv)
	return d
}

// applyFile overlays a config file onto d. A missing file is not an error; a
// present-but-malformed file is (returned so the caller can warn), and d is
// left at its built-in values in that case.
func applyFile(d *Defaults, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		// A file that does not parse normally warns and leaves the built-ins in
		// place. That is the wrong answer when the file was trying to configure
		// a policy: the user would get a CLI that silently permits everything.
		// We cannot know what the table said, so we record that a policy was
		// intended and let the CLI refuse.
		if mentionsPolicyTable(string(data)) {
			d.Policy = Policy{
				Present:   true,
				Enabled:   true,
				Source:    path,
				Malformed: "the config file has a [policy] table but does not parse: " + err.Error(),
			}
		}
		return err
	}
	applyPolicy(d, f.Policy, md, path)
	if f.Timeout != nil {
		if t, err := time.ParseDuration(*f.Timeout); err == nil {
			d.Timeout = t
		}
	}
	if f.By != nil {
		d.By = *f.By
	}
	if f.Wait != nil {
		d.Wait = *f.Wait
	}
	if f.Target != nil {
		d.Target = *f.Target
	}
	if f.Port != nil {
		d.Port = *f.Port
	}
	if f.ProfileDir != nil {
		d.ProfileDir = *f.ProfileDir
	}
	if f.NoLaunch != nil {
		d.NoLaunch = *f.NoLaunch
	}
	if f.NoDaemon != nil {
		d.NoDaemon = *f.NoDaemon
	}
	if f.JSON != nil {
		d.JSON = *f.JSON
	}
	if f.NoColor != nil {
		d.NoColor = *f.NoColor
	}
	return nil
}

// mentionsPolicyTable reports whether an unparseable config file contains a
// [policy] header, ignoring comments. It is a text scan precisely because the
// TOML parse already failed; the only decision it drives is "refuse" rather than
// "silently run without the policy the user wrote".
func mentionsPolicyTable(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[policy]") || strings.HasPrefix(line, "[policy.") {
			return true
		}
	}
	return false
}

// applyPolicy copies the decoded [policy] table onto d.
//
// A key inside [policy] that this build does not recognise is recorded as
// Malformed rather than ignored: a typo like `allowed = [...]` would otherwise
// be a rule the user believes is in force and is not.
func applyPolicy(d *Defaults, pf *policyFile, md toml.MetaData, path string) {
	if pf == nil {
		return
	}
	p := Policy{Present: true, Enabled: true, Source: path}
	if pf.Enabled != nil {
		p.Enabled = *pf.Enabled
	}
	p.Allow, p.Deny, p.ReadOnly = pf.Allow, pf.Deny, pf.ReadOnly
	p.VerbsDenied, p.UploadRoots = pf.VerbsDenied, pf.UploadRoots
	if pf.AuditLog != nil {
		p.AuditLog = *pf.AuditLog
	}
	if pf.AuditAll != nil {
		p.AuditAll = *pf.AuditAll
	}
	if pf.OnViolation != nil {
		p.OnViolation = *pf.OnViolation
	}
	var unknown []string
	for _, k := range md.Undecoded() {
		if len(k) > 1 && k[0] == "policy" {
			unknown = append(unknown, strings.Join(k[1:], "."))
		}
	}
	if len(unknown) > 0 {
		p.Malformed = "unknown key(s) in the [policy] table: " + strings.Join(unknown, ", ")
	}
	d.Policy = p
}

// applyEnv overlays CHROME_CDP_* variables onto d; unset variables are skipped.
func applyEnv(d *Defaults, getenv func(string) string) {
	if v := getenv("CHROME_CDP_TIMEOUT"); v != "" {
		if t, err := time.ParseDuration(v); err == nil {
			d.Timeout = t
		}
	}
	if v := getenv("CHROME_CDP_BY"); v != "" {
		d.By = v
	}
	if v := getenv("CHROME_CDP_WAIT"); v != "" {
		d.Wait = v
	}
	if v := getenv("CHROME_CDP_TARGET"); v != "" {
		d.Target = v
	}
	if v := getenv("CHROME_CDP_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			d.Port = n
		}
	}
	if v := getenv("CHROME_CDP_PROFILE"); v != "" {
		d.ProfileDir = v
	}
	setBool(getenv("CHROME_CDP_NO_LAUNCH"), &d.NoLaunch)
	setBool(getenv("CHROME_CDP_NO_DAEMON"), &d.NoDaemon)
	setBool(getenv("CHROME_CDP_JSON"), &d.JSON)
	setBool(getenv("CHROME_CDP_NO_COLOR"), &d.NoColor)
}

func setBool(v string, dst *bool) {
	if v == "" {
		return
	}
	if b, err := strconv.ParseBool(v); err == nil {
		*dst = b
	}
}
