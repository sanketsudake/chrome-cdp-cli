// Package state persists the sticky "current target" across CLI invocations,
// keyed per browser endpoint, under $XDG_STATE_HOME.
package state

import (
	"os"
	"path/filepath"
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
