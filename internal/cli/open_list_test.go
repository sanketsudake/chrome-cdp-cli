package cli

import (
	"context"
	"testing"

	"github.com/sanketsudake/chrome-cdp-cli/internal/target"
)

type openCapture struct {
	fakeBrowser
	openedURL string
}

func (o *openCapture) Open(_ context.Context, url string) (map[string]any, error) {
	o.openedURL = url
	return map[string]any{"id": "newtab99", "url": url}, nil
}

func TestOpenCommand(t *testing.T) {
	t.Parallel()
	b := &openCapture{fakeBrowser: fakeBrowser{tabs: []target.Info{{ID: "aa11", Title: "A", URL: "u"}}}}
	env, _, code := run(t, b, "open", "https://example.com/x", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if b.openedURL != "https://example.com/x" {
		t.Errorf("opened %q", b.openedURL)
	}
	if env["result"].(map[string]any)["id"] != "newtab99" {
		t.Errorf("result = %v", env["result"])
	}
}

func TestListFilters(t *testing.T) {
	t.Parallel()
	b := &fakeBrowser{tabs: []target.Info{
		{ID: "aa11", Title: "GitHub", URL: "https://github.com/"},
		{ID: "bb22", Title: "Calendar - Outlook", URL: "https://outlook.cloud.microsoft/calendar"},
		{ID: "cc33", Title: "Inbox", URL: "https://mail.google.com/"},
	}}
	// --url filters by URL substring.
	env, _, _ := run(t, b, "list", "--url", "outlook", "--json")
	tabs := env["result"].(map[string]any)["tabs"].([]any)
	if len(tabs) != 1 || tabs[0].(map[string]any)["id"] != "bb22" {
		t.Errorf("--url outlook = %v, want just bb22", tabs)
	}
	// idx reflects the full-list position, so it stays a valid @N target.
	if int(tabs[0].(map[string]any)["idx"].(float64)) != 2 {
		t.Errorf("filtered tab idx = %v, want 2 (full-list position)", tabs[0].(map[string]any)["idx"])
	}
	// --title filters by title substring, case-insensitive.
	env, _, _ = run(t, b, "list", "--title", "inbox", "--json")
	tabs = env["result"].(map[string]any)["tabs"].([]any)
	if len(tabs) != 1 || tabs[0].(map[string]any)["id"] != "cc33" {
		t.Errorf("--title inbox = %v, want just cc33", tabs)
	}
}
