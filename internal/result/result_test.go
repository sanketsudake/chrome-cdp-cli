package result

import (
	"encoding/json"
	"testing"
)

func TestExitCodeFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code string
		want int
	}{
		{"usage", ExitUsage},
		{"connection_failed", ExitConnection},
		{"not_debug_enabled", ExitConnection},
		// A pending consent prompt is a distinct code on the EXISTING connection
		// exit code: a caller branches on error.code, and the number is contract.
		{"consent_pending", ExitConnection},
		{"target_timeout", ExitTarget},
		{"target_not_found", ExitTarget},
		{"ambiguous_target", ExitTarget},
		{"no_current_target", ExitTarget},
		{"cdp_error", ExitCDP},
		{"daemon_error", ExitDaemon},
		{"permission_denied", ExitPermission},
		// An assertion verdict is nonzero but deliberately NOT a distinct exit
		// code: `--fail-on-match` has to fail a CI step the same way any other
		// failure does, while error.code still says which it was.
		{"assertion_failed", ExitGeneric},
		{"something_unknown", ExitGeneric},
		{"", ExitGeneric},
	}
	for _, c := range cases {
		name := c.code
		if name == "" {
			name = "empty_code"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := ExitCodeFor(c.code); got != c.want {
				t.Errorf("ExitCodeFor(%q) = %d, want %d", c.code, got, c.want)
			}
		})
	}
}

// TestPermissionDeniedIsItsOwnExitCode pins the RFC-0012/RFC-0006 addition.
//
// A policy refusal has to be distinguishable from every other failure: an agent
// must be able to tell "policy forbids this, stop and tell the user" from
// "element not found, retry differently". Without the codeToExit entry the code
// would degrade to ExitGeneric and silently become indistinguishable from any
// other error, which is the exact bug this file exists to catch.
func TestPermissionDeniedIsItsOwnExitCode(t *testing.T) {
	t.Parallel()
	if ExitPermission != 7 {
		t.Errorf("ExitPermission = %d, want 7 (the documented contract)", ExitPermission)
	}
	if got := ExitCodeFor(CodePermissionDenied); got != ExitPermission {
		t.Errorf("ExitCodeFor(%q) = %d, want %d — add the codeToExit entry", CodePermissionDenied, got, ExitPermission)
	}
	for _, taken := range []int{ExitOK, ExitGeneric, ExitUsage, ExitConnection, ExitTarget, ExitCDP, ExitDaemon} {
		if ExitPermission == taken {
			t.Fatalf("ExitPermission collides with an existing exit code %d", taken)
		}
	}
	env := Envelope{OK: false, Command: "click", Error: &Err{Code: CodePermissionDenied}}
	if env.ExitCode() != ExitPermission {
		t.Errorf("envelope exit = %d, want %d", env.ExitCode(), ExitPermission)
	}
}

// TestExitCodesDocumentsEveryMappedCode keeps `chrome-cdp exit-codes` from
// going stale: every exit code a command can actually produce must have a row.
func TestExitCodesDocumentsEveryMappedCode(t *testing.T) {
	t.Parallel()
	documented := map[int]bool{}
	for _, d := range ExitCodes() {
		if documented[d.Code] {
			t.Errorf("exit code %d is documented twice", d.Code)
		}
		documented[d.Code] = true
	}
	for code, exit := range codeToExit {
		if !documented[exit] {
			t.Errorf("error.code %q maps to exit %d, which ExitCodes() does not document", code, exit)
		}
	}
	if !documented[ExitOK] {
		t.Error("ExitCodes() must document success")
	}
}

// The uniform envelope is the contract the Claude skill parses against.
func TestSuccessEnvelopeShape(t *testing.T) {
	t.Parallel()
	env := Envelope{
		OK:        true,
		Command:   "eval",
		Target:    &TargetInfo{ID: "2d64", Title: "GitHub", URL: "https://github.com/"},
		Result:    map[string]any{"value": "GitHub"},
		ElapsedMs: 12,
	}
	m := marshalToMap(t, env)

	if m["ok"] != true {
		t.Errorf("ok = %v, want true", m["ok"])
	}
	if m["command"] != "eval" {
		t.Errorf("command = %v", m["command"])
	}
	if _, has := m["error"]; has {
		t.Error("success envelope must not contain an error field")
	}
	if m["elapsed_ms"].(float64) != 12 {
		t.Errorf("elapsed_ms = %v, want 12", m["elapsed_ms"])
	}
	tgt := m["target"].(map[string]any)
	if tgt["id"] != "2d64" || tgt["title"] != "GitHub" || tgt["url"] != "https://github.com/" {
		t.Errorf("target = %v", tgt)
	}
	if m["result"].(map[string]any)["value"] != "GitHub" {
		t.Errorf("result = %v", m["result"])
	}
}

func TestErrorEnvelopeShape(t *testing.T) {
	t.Parallel()
	env := Envelope{
		OK:      false,
		Command: "click",
		Error: &Err{
			Code:    "target_timeout",
			Message: `selector "#missing" not found after 30s`,
			Details: map[string]any{"selector": "#missing", "timeout_ms": 30000},
		},
		ElapsedMs: 30011,
	}
	m := marshalToMap(t, env)

	if m["ok"] != false {
		t.Errorf("ok = %v, want false", m["ok"])
	}
	if _, has := m["result"]; has {
		t.Error("error envelope must not contain a result field")
	}
	e := m["error"].(map[string]any)
	if e["code"] != "target_timeout" {
		t.Errorf("error.code = %v", e["code"])
	}
	if e["message"] != `selector "#missing" not found after 30s` {
		t.Errorf("error.message = %v", e["message"])
	}
	// Details must be flattened INTO the error object (matches the output prototype),
	// not nested under a "details" key.
	if e["selector"] != "#missing" {
		t.Errorf("error.selector = %v; details must be flattened into the error object", e["selector"])
	}
	if e["timeout_ms"].(float64) != 30000 {
		t.Errorf("error.timeout_ms = %v", e["timeout_ms"])
	}
}

func marshalToMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}
