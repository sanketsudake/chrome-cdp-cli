// Package config loads optional persistent defaults from a TOML file and merges
// them with CHROME_CDP_* environment variables into the effective global-flag
// defaults. Precedence, highest first: explicit flags > env > config file >
// built-in defaults. Cobra applies the flags; this package resolves the
// env > config > built-in portion and hands the result to the command tree.
package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
)

// Defaults are the effective global-flag defaults after the config file and
// CHROME_CDP_* env are merged over the built-in values.
type Defaults struct {
	Timeout time.Duration
	// ConsentTimeout bounds the wait for Chrome's browser-modal "Allow remote
	// debugging?" prompt (config key consent_timeout). It is separate from
	// Timeout because it is not a command deadline at all: it is how long a
	// human is given to find and click a dialog that may be behind the window,
	// so it is measured in minutes where Timeout is measured in seconds.
	ConsentTimeout time.Duration
	By             string
	Wait           string
	Target         string
	Port           int
	// Endpoint is an explicit debug endpoint (ws:// or http://) that wins over
	// Port and the DevToolsActivePort file (config key endpoint, env
	// CHROME_CDP_ENDPOINT). See browser.FindEndpoint.
	Endpoint   string
	ProfileDir string
	NoLaunch   bool
	NoDaemon   bool
	JSON       bool
	NoColor    bool

	// Policy is the optional [policy] table (RFC-0012). No CHROME_CDP_* variable
	// sets any of its keys: a safety boundary whose CONTENTS an inherited
	// environment could rewrite would not be much of a boundary. Override it
	// explicitly with --allow / --policy-off instead.
	//
	// It is not, however, immune to the environment, and pretending otherwise
	// would be the more dangerous claim: XDG_CONFIG_HOME (and HOME) decide WHICH
	// file is read, so an environment that points them elsewhere selects a
	// different policy, or none. Nothing here can prevent that — the config file
	// has to be found somehow — so the disappearance is made visible instead:
	// Note() reports a config file that XDG_CONFIG_HOME pointed at and that does
	// not exist, and an unreadable file becomes a Malformed policy the CLI
	// refuses to run with rather than a policy silently absent.
	Policy Policy

	// Event-capture bounds for the observability verbs. They size the buffers
	// the connection holder retains per tab, so they are read by the daemon (or
	// by a --no-daemon connect) rather than by a command flag.
	ConsoleBuffer   int // retained console messages per tab
	ConsoleMaxEntry int // per-message text cap, in bytes
	NetBuffer       int // retained network records per tab
	NetMaxBody      int // per-body cap, in bytes (request and response)

	// Recording bounds (RFC-0011). A frame dwarfs a console line or a request
	// record, so recording is bounded by BOTH a frame count and a byte ceiling:
	// 600 frames is a modest number on a laptop viewport and a large one on a
	// 4K monitor, and only the second bound knows the difference.
	RecordBuffer   int // retained frames per tab
	RecordMaxBytes int // retained frame bytes per tab
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
	return Defaults{
		Timeout: 30 * time.Second, ConsentTimeout: chrome.DefaultConsentTimeout,
		By: "css", Wait: "visible",
		ConsoleBuffer: chrome.DefaultConsoleBuffer, ConsoleMaxEntry: chrome.DefaultConsoleMaxEntry,
		NetBuffer: chrome.DefaultNetBuffer, NetMaxBody: chrome.DefaultNetMaxBody,
		RecordBuffer: chrome.DefaultRecordFrames, RecordMaxBytes: chrome.DefaultRecordMaxBytes,
	}
}

// file mirrors the TOML schema; pointer fields distinguish "set in file" from
// "absent", so an omitted key leaves the built-in (or env) value intact.
type file struct {
	Timeout        *string `toml:"timeout"`
	ConsentTimeout *string `toml:"consent_timeout"`
	By             *string `toml:"by"`
	Wait           *string `toml:"wait"`
	Target         *string `toml:"target"`
	Port           *int    `toml:"port"`
	Endpoint       *string `toml:"endpoint"`
	ProfileDir     *string `toml:"profile_dir"`
	NoLaunch       *bool   `toml:"no_launch"`
	NoDaemon       *bool   `toml:"no_daemon"`
	JSON           *bool   `toml:"json"`
	NoColor        *bool   `toml:"no_color"`

	ConsoleBuffer   *int `toml:"console_buffer"`
	ConsoleMaxEntry *int `toml:"console_max_entry"`
	NetBuffer       *int `toml:"net_buffer"`
	NetMaxBody      *int `toml:"net_max_body"`
	RecordBuffer    *int `toml:"record_buffer"`
	RecordMaxBytes  *int `toml:"record_max_bytes"`

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
	normalise(&d)
	return d, err
}

// FromEnv returns the built-in defaults overlaid with CHROME_CDP_* env vars only
// (no config file). The daemon subprocess uses it: the parent already folded the
// config file into the environment it hands down, so parsing stays in one place.
func FromEnv() Defaults {
	d := Builtin()
	applyEnv(&d, os.Getenv)
	normalise(&d)
	return d
}

// normalise pulls resolved values into the range the rest of the program is
// entitled to assume. This is the single place it happens: resolution is where
// flag defaults, environment and config file meet, so a value that is sane here
// is sane in every layer downstream.
//
// The consent budget is the one that needed it. A zero (from consent_timeout =
// "0s", or an env var someone cleared) meant "the default" to chrome.Connect
// and "no wait at all" to daemon.Ensure, which put back the orphaned-prompt
// failure the setting exists to prevent — and an inherited
// CHROME_CDP_CONSENT_TIMEOUT=8760h would have held the daemon spawn lock for a
// year.
func normalise(d *Defaults) {
	d.ConsentTimeout = chrome.ClampConsentTimeout(d.ConsentTimeout)
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
		// A file that EXISTS but cannot be read (EACCES, a bad mount, an I/O
		// error) is the same situation as one that does not parse, and gets the
		// same answer. Leaving the zero Policy here would mean a config file that
		// could not be PARSED refuses to run while one that could not be READ runs
		// wide open — the wrong way round, and a fail-open the user cannot see,
		// since a chmod is not something they did on purpose today.
		//
		// We cannot know whether this file configured a policy, so we assume it
		// did: over-refusing is recoverable (fix the permissions, or --policy-off),
		// and under-refusing is the failure this whole layer exists to prevent.
		d.Policy = Policy{
			Present:   true,
			Enabled:   true,
			Source:    path,
			Malformed: "config file could not be read: " + err.Error(),
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
	if f.ConsentTimeout != nil {
		if t, err := time.ParseDuration(*f.ConsentTimeout); err == nil {
			d.ConsentTimeout = t
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
	if f.Endpoint != nil {
		d.Endpoint = *f.Endpoint
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
	if f.ConsoleBuffer != nil {
		d.ConsoleBuffer = *f.ConsoleBuffer
	}
	if f.ConsoleMaxEntry != nil {
		d.ConsoleMaxEntry = *f.ConsoleMaxEntry
	}
	if f.NetBuffer != nil {
		d.NetBuffer = *f.NetBuffer
	}
	if f.NetMaxBody != nil {
		d.NetMaxBody = *f.NetMaxBody
	}
	if f.RecordBuffer != nil {
		d.RecordBuffer = *f.RecordBuffer
	}
	if f.RecordMaxBytes != nil {
		d.RecordMaxBytes = *f.RecordMaxBytes
	}
	return nil
}

// policyHeader matches a [policy] or [policy.sub] table header.
//
// The inner whitespace is not cosmetic: TOML permits `[ policy ]`, and a scan
// that only knew `[policy]` would skip the fatal-refusal path for a file spelled
// that way — a fail-open reachable by a space.
var policyHeader = regexp.MustCompile(`^\[\s*policy\s*[\].]`)

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
		if policyHeader.MatchString(line) {
			return true
		}
	}
	return false
}

// Note returns a one-line advisory about the config file, or "" when there is
// nothing to say. The CLI prints it to stderr before running.
//
// It exists for one case: XDG_CONFIG_HOME is set and names a directory with no
// config file in it. That is how a policy disappears without anyone noticing —
// the CLI does not stop working, it simply stops being bounded, and there is no
// envelope field and no exit code to catch it. It cannot be made an error
// (running without a config file is the normal case for most users), so it is
// made visible.
func Note() string { return noteFrom(Path(), os.Getenv) }

func noteFrom(path string, getenv func(string) string) string {
	if getenv("XDG_CONFIG_HOME") == "" || path == "" {
		return ""
	}
	if _, err := os.Stat(path); err == nil {
		return ""
	}
	return "chrome-cdp: no config file at " + path +
		" (XDG_CONFIG_HOME is set) — any [policy] table elsewhere is NOT in effect"
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
	if v := getenv("CHROME_CDP_CONSENT_TIMEOUT"); v != "" {
		if t, err := time.ParseDuration(v); err == nil {
			d.ConsentTimeout = t
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
	if v := getenv("CHROME_CDP_ENDPOINT"); v != "" {
		d.Endpoint = v
	}
	if v := getenv("CHROME_CDP_PROFILE"); v != "" {
		d.ProfileDir = v
	}
	setInt(getenv("CHROME_CDP_CONSOLE_BUFFER"), &d.ConsoleBuffer)
	setInt(getenv("CHROME_CDP_CONSOLE_MAX_ENTRY"), &d.ConsoleMaxEntry)
	setInt(getenv("CHROME_CDP_NET_BUFFER"), &d.NetBuffer)
	setInt(getenv("CHROME_CDP_NET_MAX_BODY"), &d.NetMaxBody)
	setInt(getenv("CHROME_CDP_RECORD_BUFFER"), &d.RecordBuffer)
	setInt(getenv("CHROME_CDP_RECORD_MAX_BYTES"), &d.RecordMaxBytes)
	setBool(getenv("CHROME_CDP_NO_LAUNCH"), &d.NoLaunch)
	setBool(getenv("CHROME_CDP_NO_DAEMON"), &d.NoDaemon)
	setBool(getenv("CHROME_CDP_JSON"), &d.JSON)
	setBool(getenv("CHROME_CDP_NO_COLOR"), &d.NoColor)
}

// setInt overlays a numeric env var, LEAVING the existing value in place when
// it is malformed — a bad number is a warning-shaped mistake, not a reason to
// brick the CLI, matching how a malformed config file is handled.
func setInt(v string, dst *int) {
	if v == "" {
		return
	}
	if n, err := strconv.Atoi(v); err == nil {
		*dst = n
	}
}

func setBool(v string, dst *bool) {
	if v == "" {
		return
	}
	if b, err := strconv.ParseBool(v); err == nil {
		*dst = b
	}
}
