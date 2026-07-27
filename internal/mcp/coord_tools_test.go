package mcp

// The MCP surface for RFC-0014: an agent must be able to reach the coordinate
// form and window sizing. Both shipped unreachable — `selector` was required
// even with `at`, and width/height arrived as JSON numbers where the builder
// only read strings.

import "testing"

func TestPointerToolsAcceptCoordinateForm(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	sess := connect(t, r, Options{})

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{prefix + "click", map[string]any{"at": "512,340"}},
		{prefix + "pointer", map[string]any{"action": "dblclick", "at": "512,340"}},
		{prefix + "pointer", map[string]any{"action": "tripleclick", "at": "512,340"}},
		{prefix + "pointer", map[string]any{"action": "drag", "at": "10,10", "to_at": "30,40"}},
	} {
		out := callTool(t, sess, tc.tool, tc.args)
		if out.IsError {
			t.Errorf("%s %v is unreachable: %v", tc.tool, tc.args, structured(t, out))
		}
	}
}

func TestTabsWindowSizeAcceptsNumbers(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	sess := connect(t, r, Options{})

	// A conformant client sends integers as JSON numbers, not strings.
	out := callTool(t, sess, prefix+"tabs", map[string]any{
		"action": "window_size", "width": 1280, "height": 800,
	})
	if out.IsError {
		t.Fatalf("window_size with numeric width/height failed: %v", structured(t, out))
	}
	var sawW, sawH bool
	for _, c := range r.calls {
		for _, a := range c {
			if a == "1280" {
				sawW = true
			}
			if a == "800" {
				sawH = true
			}
		}
	}
	if !sawW || !sawH {
		t.Errorf("dimensions did not reach the CLI argv: %v", r.calls)
	}
}

// The drop form must be reachable through MCP, and must not smuggle a selector
// into the positional args where the CLI would read it as a path.
func TestUploadToolReachesDropForm(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	sess := connect(t, r, Options{})

	out := callTool(t, sess, prefix+"upload", map[string]any{
		"drop": "#zone", "paths": []any{"/tmp/report.pdf"},
	})
	if out.IsError {
		t.Fatalf("upload --drop is unreachable: %v", structured(t, out))
	}
	if len(r.calls) == 0 {
		t.Fatal("no CLI call was made")
	}
	argv := r.calls[len(r.calls)-1]
	// Positionals come after the "--" separator; the selector must not be
	// among them, or the CLI would read it as a file path.
	var positionals []string
	for i, a := range argv {
		if a == "--" {
			positionals = argv[i+1:]
			break
		}
	}
	for _, a := range positionals {
		if a == "#zone" {
			t.Errorf("the drop selector was passed as a positional path: %v", argv)
		}
	}
	if len(positionals) != 1 || positionals[0] != "/tmp/report.pdf" {
		t.Errorf("positionals = %v, want just the path", positionals)
	}
	var sawFlag bool
	for i, a := range argv {
		if a == "--drop" && i+1 < len(argv) && argv[i+1] == "#zone" {
			sawFlag = true
		}
	}
	if !sawFlag {
		t.Errorf("argv = %v, want --drop #zone", argv)
	}

	if out := callTool(t, sess, prefix+"upload", map[string]any{
		"selector": "#in", "drop": "#zone", "paths": []any{"/tmp/a.pdf"},
	}); !out.IsError {
		t.Error("selector and drop together were accepted")
	}
}
