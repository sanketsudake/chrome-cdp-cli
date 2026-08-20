// Package state persists the sticky "current target" across CLI invocations,
// keyed per browser endpoint, under $XDG_STATE_HOME.
package state

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Store holds per-endpoint CLI state under an XDG state directory.
type Store struct {
	dir string
	key string
}

// New returns a Store rooted at $XDG_STATE_HOME/chrome-cdp (or ~/.local/state),
// keyed by an endpoint identity (e.g. host:port of the debug endpoint) so
// distinct Chrome instances don't share a sticky target.
func New(key string) (*Store, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "chrome-cdp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, key: sanitize(key)}, nil
}

func (s *Store) file() string {
	return filepath.Join(s.dir, "current-target-"+s.key)
}

// CurrentTarget returns the sticky target spec, or "" if none is set.
func (s *Store) CurrentTarget() string {
	b, err := os.ReadFile(s.file())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SetCurrentTarget persists the sticky target spec.
func (s *Store) SetCurrentTarget(spec string) error {
	return os.WriteFile(s.file(), []byte(spec+"\n"), 0o600)
}

// sanitize maps an endpoint key to a filesystem-safe filename component.
//
// It is a rune-for-rune mapping (every input rune produces exactly one output
// rune; none are dropped or merged), which matters for the caller that builds
// a session-namespaced key as EndpointKey(...) + "/" + session
// (cmd/chrome-cdp/main.go's stateFor): "/" falls into the default case below
// like any other disallowed rune (":" from a host:port key, for instance),
// but two properties still hold given a valid --session, which
// ValidateSession restricts to ^[A-Za-z0-9._-]{1,64}$ — a subset of the runes
// this function already passes through unchanged:
//
//  1. Two different sessions on the SAME endpoint key never collide: the
//     mapped output is sanitize(base) + "-" + session (the session's own
//     runes are already in the allowed set, so they pass through verbatim),
//     and two strings sharing that fixed prefix differ iff their suffixes
//     do — which is exactly session1 != session2.
//  2. A session never collides with no-session on the same endpoint key: the
//     rune-for-rune mapping preserves length, so sanitize(base) has length
//     len(base) while sanitize(base+"/"+session) has length
//     len(base)+1+len(session) — strictly longer for any non-empty session,
//     so the two can never be equal.
//
// (A cross-endpoint collision — two DIFFERENT base keys whose sanitized forms
// coincide once other disallowed runes like ":" collapse to "-" — is a
// pre-existing class unrelated to sessions, and unreachable from real
// browser.EndpointKey output, which is always "host:port" or the literal
// "default".)
func sanitize(key string) string {
	if key == "" {
		return "default"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, key)
}

// sessionPattern is the grammar for --session / CHROME_CDP_SESSION / the TOML
// `session` key: short, filesystem- and shell-friendly names only.
var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidateSession reports whether name is a well-formed session namespace. An
// empty name means "no session" and is valid, the same way
// browser.ValidateEndpoint treats an empty --endpoint as unset — so a caller
// can validate the flag/env/config value unconditionally without special-
// casing "not given".
//
// It is a pure function (no filesystem, no Chrome) so it can run in
// PersistentPreRunE ahead of resolveTarget/getBrowser (a malformed --session
// is usage/exit 2 before anything connects) and, exported for exactly this,
// in internal/config's applyFile/applyEnv, where a malformed
// CHROME_CDP_SESSION or TOML session is dropped silently like a malformed
// endpoint is.
func ValidateSession(name string) error {
	if name == "" {
		return nil
	}
	if !sessionPattern.MatchString(name) {
		return fmt.Errorf("--session %q: must match %s (letters, digits, '.', '_', '-'; 1-64 characters)", name, sessionPattern.String())
	}
	return nil
}
