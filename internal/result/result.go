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
	ExitPermission = 7 // refused by policy (origin, verb, or upload root)
)

// Stable error.code strings emitted in the envelope's error object. Named so
// call sites don't scatter bare string literals.
const (
	CodeGeneric        = "generic"
	CodeUsage          = "usage"
	CodeConnection     = "connection_failed"
	CodeNotDebug       = "not_debug_enabled"
	CodeTargetTimeout  = "target_timeout"
	CodeTargetNotFound = "target_not_found"
	CodeAmbiguous      = "ambiguous_target"
	CodeNoTarget       = "no_current_target"
	CodeCDP            = "cdp_error"
	CodeDaemon         = "daemon_error"
	// CodePermissionDenied is the policy layer's refusal (RFC-0012, RFC-0006).
	// It is deliberately distinct from a target error: an agent must be able to
	// tell "policy forbids this, do not retry, tell the user" from "element not
	// found, retry differently".
	CodePermissionDenied = "permission_denied"
	// CodeAssertFailed is an assertion verdict, not a tool failure: the command
	// ran fine and the thing it was asked to check was true (console
	// --fail-on-match found messages). It maps to ExitGeneric so `nonzero means
	// the assertion tripped` keeps working in a CI `set -e` script, while the
	// code still lets a caller tell "the page logged an error" apart from "the
	// tool broke". Shared with RFC-0003's net --fail-on-match.
	CodeAssertFailed = "assertion_failed"
)

// codeToExit maps stable error.code strings to their process exit code.
var codeToExit = map[string]int{
	CodeGeneric:        ExitGeneric,
	CodeAssertFailed:   ExitGeneric,
	CodeUsage:          ExitUsage,
	CodeConnection:     ExitConnection,
	CodeNotDebug:       ExitConnection,
	CodeTargetTimeout:  ExitTarget,
	CodeTargetNotFound: ExitTarget,
	CodeAmbiguous:      ExitTarget,
	CodeNoTarget:       ExitTarget,
	CodeCDP:            ExitCDP,
	CodeDaemon:         ExitDaemon,

	CodePermissionDenied: ExitPermission,
}

// ExitCodeDoc documents one exit code for `chrome-cdp exit-codes`.
type ExitCodeDoc struct {
	Code int
	Desc string
}

// ExitCodes is the single source for the documented exit-code contract; the
// exit-codes command renders it rather than repeating the strings.
func ExitCodes() []ExitCodeDoc {
	return []ExitCodeDoc{
		{ExitOK, "success"},
		{ExitGeneric, "generic / unclassified (also: an assertion tripped, e.g. console --fail-on-match)"},
		{ExitUsage, "usage (bad flags/args)"},
		{ExitConnection, "connection (attach/launch failed)"},
		{ExitTarget, "target/timeout (selector not found, timeout, ambiguous/unknown target)"},
		{ExitCDP, "cdp protocol error"},
		{ExitDaemon, "daemon error"},
		{ExitPermission, "permission denied by policy (origin, verb, or upload root)"},
	}
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
