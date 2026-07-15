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
	if _, err := toml.Decode(string(data), &f); err != nil {
		return err
	}
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
