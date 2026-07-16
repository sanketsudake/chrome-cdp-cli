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
		{"target_timeout", ExitTarget},
		{"target_not_found", ExitTarget},
		{"ambiguous_target", ExitTarget},
		{"no_current_target", ExitTarget},
		{"cdp_error", ExitCDP},
		{"daemon_error", ExitDaemon},
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
