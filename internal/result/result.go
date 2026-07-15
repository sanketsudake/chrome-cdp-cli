// Package result defines chrome-cdp's uniform output envelope and the stable
// exit-code contract that both humans and the Claude skill depend on.
package result

import "encoding/json"

// Exit codes are a stable, documented contract (see `chrome-cdp help exit-codes`).
// error.code strings are finer-grained and map onto these via ExitCodeFor.
const (
	ExitOK         = 0 // success
	ExitGeneric    = 1 // unclassified error
	ExitUsage      = 2 // bad flags/args, validated before touching Chrome
	ExitConnection = 3 // can't attach to / launch Chrome
	ExitTarget     = 4 // selector not found, nav/wait timeout, ambiguous/unknown target
	ExitCDP        = 5 // a CDP method returned an error
	ExitDaemon     = 6 // daemon not running / already running / stale lock
)

// codeToExit maps stable error.code strings to their process exit code.
var codeToExit = map[string]int{
	"usage":             ExitUsage,
	"connection_failed": ExitConnection,
	"not_debug_enabled": ExitConnection,
	"target_timeout":    ExitTarget,
	"target_not_found":  ExitTarget,
	"ambiguous_target":  ExitTarget,
	"no_current_target": ExitTarget,
	"cdp_error":         ExitCDP,
	"daemon_error":      ExitDaemon,
}

// ExitCodeFor returns the process exit code for an error.code string,
// defaulting to ExitGeneric for anything unrecognized.
func ExitCodeFor(code string) int {
	if c, ok := codeToExit[code]; ok {
		return c
	}
	return ExitGeneric
}

// TargetInfo identifies the tab a command acted on.
type TargetInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Err is a structured command failure with optional context Details.
type Err struct {
	Code    string
	Message string
	Details map[string]any
}

// MarshalJSON flattens Details into the error object alongside code and message
// (e.g. {"code","message","selector",...}). encoding/json sorts map keys, so
// output is deterministic; code and message always win over any Details key.
func (e Err) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, len(e.Details)+2)
	for k, v := range e.Details {
		m[k] = v
	}
	m["code"] = e.Code
	m["message"] = e.Message
	return json.Marshal(m)
}

// Envelope is the single JSON shape every command emits under --json.
type Envelope struct {
	OK        bool        `json:"ok"`
	Command   string      `json:"command"`
	Target    *TargetInfo `json:"target,omitempty"`
	Result    any         `json:"result,omitempty"`
	Error     *Err        `json:"error,omitempty"`
	ElapsedMs int64       `json:"elapsed_ms"`
}

// ExitCode returns the process exit code implied by this envelope.
func (e Envelope) ExitCode() int {
	if e.OK {
		return ExitOK
	}
	if e.Error != nil {
		return ExitCodeFor(e.Error.Code)
	}
	return ExitGeneric
}

// JSON renders the envelope as a single compact JSON value.
func (e Envelope) JSON() ([]byte, error) {
	return json.Marshal(e)
}
