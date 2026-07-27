package cli

// One live-Chrome smoke test for MCP mode: navigate → snapshot → click → read.
//
// Deliberately thin. Protocol correctness, the tool surface and the result
// mapping are covered by the stub-backed tests; this only proves the wiring is
// real — that a tool call reaches a browser and comes back with what the page
// actually did.

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/chrome"
	"github.com/sanketsudake/chrome-cdp-cli/internal/mcp"
)

func TestMCPLiveSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome MCP smoke test in -short mode")
	}
	// PortFile points at a path that does not exist, so the connection ladder
	// can only launch a throwaway managed Chrome — it must never attach to the
	// developer's own session and start navigating their tabs. When their
	// Chrome is running, the ladder refuses rather than launching a second
	// browser, and this skips.
	profile, err := os.MkdirTemp("", "chrome-cdp-mcp-live-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profile) })

	b, err := chrome.Connect(context.Background(), chrome.Options{
		Headless:   true,
		ProfileDir: profile,
		PortFile:   filepath.Join(t.TempDir(), "no-such-DevToolsActivePort"),
	})
	if err != nil {
		t.Skipf("no managed Chrome available here: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	app := New(b, &bytes.Buffer{}, &bytes.Buffer{})
	sess := serveMCP(t, app, mcp.Options{})

	tabs := mcpStructured(t, mcpCall(t, sess, "tabs", map[string]any{"action": "list"}))
	rows, _ := tabs["tabs"].([]any)
	if len(rows) == 0 {
		t.Fatal("the browser reported no tabs")
	}
	id, _ := rows[0].(map[string]any)["id"].(string)
	// The sticky current tab is persisted by the real binary, not by a test
	// App, so every call below names the tab explicitly.

	page := `<!doctype html><title>MCP fixture</title>` +
		`<button onclick="document.getElementById('log').textContent='clicked'">Go</button>` +
		`<p id="log">idle</p>`
	if out := mcpCall(t, sess, "navigate", map[string]any{"url": "data:text/html," + url.PathEscape(page), "target": id}); out.IsError {
		t.Fatalf("navigate: %v", out.StructuredContent)
	}

	snap := mcpCall(t, sess, "snapshot", map[string]any{"role": "button", "target": id})
	if snap.IsError {
		t.Fatalf("snapshot: %v", snap.StructuredContent)
	}
	if !strings.Contains(string(mustJSON(snap.StructuredContent)), "Go") {
		t.Fatalf("the snapshot does not show the button: %s", mustJSON(snap.StructuredContent))
	}

	// Click it the way an agent should: by its accessible name.
	if out := mcpCall(t, sess, "click", map[string]any{"selector": "Go", "by": "name", "role": "button", "target": id}); out.IsError {
		t.Fatalf("click: %v", out.StructuredContent)
	}

	read := mcpCall(t, sess, "read", map[string]any{"kind": "text", "selector": "#log", "target": id})
	if read.IsError {
		t.Fatalf("read: %v", read.StructuredContent)
	}
	if got, _ := mcpStructured(t, read)["text"].(string); strings.TrimSpace(got) != "clicked" {
		t.Errorf("#log = %q, want %q — the click did not reach the page", got, "clicked")
	}
}
