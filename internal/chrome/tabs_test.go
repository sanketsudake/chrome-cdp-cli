package chrome

// Live-Chrome tests for the tab-lifecycle verbs (RFC-0007 VS-1..VS-4, VS-8..
// VS-11, VS-13), driven against a MANAGED headless Chrome — a throwaway
// browser, never the user's live one. Guarded by testing.Short() and skipped
// gracefully when no Chrome can be launched, and never parallel: they share a
// spawned browser.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// liveCDP launches a throwaway managed headless Chrome, or skips.
func liveCDP(t *testing.T) *CDP {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// tabFixtures serves two distinct pages plus a cacheable asset, and counts how
// often the asset is actually fetched — which is how a hard reload is told from
// a soft one without guessing at cache internals.
func tabFixtures(t *testing.T) (srv *httptest.Server, assetHits *atomic.Int64) {
	t.Helper()
	assetHits = &atomic.Int64{}
	mux := http.NewServeMux()
	page := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store") // the DOCUMENT must not be cached
			fmt.Fprintf(w, `<!doctype html><title>%s</title><script src="/asset.js"></script><body><h1>%s</h1></body>`, name, name)
		}
	}
	mux.HandleFunc("/p1", page("Page One"))
	mux.HandleFunc("/p2", page("Page Two"))
	mux.HandleFunc("/asset.js", func(w http.ResponseWriter, _ *http.Request) {
		assetHits.Add(1)
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "max-age=3600") // cacheable, so a soft reload may reuse it
		fmt.Fprint(w, "window.__asset = true;")
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, assetHits
}

func tabIDs(ctx context.Context, t *testing.T, b *CDP) map[string]bool {
	t.Helper()
	tabs, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := map[string]bool{}
	for _, tb := range tabs {
		ids[tb.ID] = true
	}
	return ids
}

func openTab(ctx context.Context, t *testing.T, b *CDP, url string) string {
	t.Helper()
	res, err := b.Open(ctx, url)
	if err != nil {
		t.Fatalf("Open(%s): %v", url, err)
	}
	id, _ := res["id"].(string)
	if id == "" {
		t.Fatalf("Open(%s) returned no id: %v", url, res)
	}
	return id
}

// VS-3 / VS-4: CloseTabs closes exactly the tabs it is given — one, or several
// in a single call — and reports them.
func TestCloseTabsLive(t *testing.T) {
	b := liveCDP(t)
	srv, _ := tabFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	keep := openTab(ctx, t, b, srv.URL+"/p1")
	victim := openTab(ctx, t, b, srv.URL+"/p2")
	before := len(tabIDs(ctx, t, b))

	// VS-3 — one tab goes, and only that one.
	res, err := b.CloseTabs(ctx, []string{victim})
	if err != nil {
		t.Fatalf("CloseTabs: %v", err)
	}
	if res["count"] != 1 {
		t.Errorf("count = %v, want 1 (result: %v)", res["count"], res)
	}
	closed, _ := res["closed"].([]any)
	if len(closed) != 1 {
		t.Fatalf("closed = %v, want one entry", res["closed"])
	}
	if got := closed[0].(map[string]any)["id"]; got != victim {
		t.Errorf("closed id = %v, want %s", got, victim)
	}
	// The url/title are read before the tab dies, so the report is not empty.
	if got := closed[0].(map[string]any)["url"]; got != srv.URL+"/p2" {
		t.Errorf("closed url = %v, want %s", got, srv.URL+"/p2")
	}

	ids := tabIDs(ctx, t, b)
	if ids[victim] {
		t.Errorf("closed tab %s is still listed", victim)
	}
	if !ids[keep] {
		t.Errorf("close removed the wrong tab: %s is gone", keep)
	}
	if len(ids) != before-1 {
		t.Errorf("tab count = %d, want %d", len(ids), before-1)
	}

	// VS-4 — several tabs in one call.
	bulk1 := openTab(ctx, t, b, srv.URL+"/p1")
	bulk2 := openTab(ctx, t, b, srv.URL+"/p2")
	before = len(tabIDs(ctx, t, b))
	res, err = b.CloseTabs(ctx, []string{bulk1, bulk2})
	if err != nil {
		t.Fatalf("bulk CloseTabs: %v", err)
	}
	if res["count"] != 2 {
		t.Errorf("bulk count = %v, want 2 (result: %v)", res["count"], res)
	}
	ids = tabIDs(ctx, t, b)
	if ids[bulk1] || ids[bulk2] {
		t.Errorf("bulk close left tabs behind: %v", ids)
	}
	if len(ids) != before-2 {
		t.Errorf("tab count = %d, want %d", len(ids), before-2)
	}
	if !ids[keep] {
		t.Errorf("bulk close took an unrelated tab: %s is gone", keep)
	}
}

// VS-8 / VS-9 / VS-10: history navigation moves and reports the settled URL,
// and running out of history is a typed error rather than a timeout.
func TestHistoryNavigationLive(t *testing.T) {
	b := liveCDP(t)
	srv, _ := tabFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := firstTab(ctx, t, b)
	for _, p := range []string{"/p1", "/p2"} {
		if _, err := b.Navigate(ctx, id, srv.URL+p); err != nil {
			t.Fatalf("Navigate %s: %v", p, err)
		}
	}

	// VS-8 — back lands on page one, and the envelope reports where it settled.
	res, err := b.History(ctx, id, -1)
	if err != nil {
		t.Fatalf("History(-1): %v", err)
	}
	if got := res["url"]; got != srv.URL+"/p1" {
		t.Fatalf("after back, url = %v, want %s", got, srv.URL+"/p1")
	}

	// VS-9 — and forward returns to page two.
	res, err = b.History(ctx, id, +1)
	if err != nil {
		t.Fatalf("History(+1): %v", err)
	}
	if got := res["url"]; got != srv.URL+"/p2" {
		t.Errorf("after forward, url = %v, want %s", got, srv.URL+"/p2")
	}

	// VS-10 — walk back until there is nowhere left to go. The number of entries
	// a fresh tab starts with varies, so the test drains the history rather than
	// assuming it; what must hold is that running out is ErrNoHistory quickly,
	// not a navigation that never settles.
	for i := 0; i < 8; i++ {
		step, scancel := context.WithTimeout(ctx, 10*time.Second)
		_, err = b.History(step, id, -1)
		scancel()
		if err != nil {
			break
		}
	}
	if !IsNoHistory(err) {
		t.Errorf("draining the history gave %v, want ErrNoHistory (a clean typed error, not a timeout)", err)
	}
}

// VS-11: reload keeps the URL, and a hard reload refetches rather than serving
// the asset from cache.
func TestReloadLive(t *testing.T) {
	b := liveCDP(t)
	srv, assetHits := tabFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := firstTab(ctx, t, b)
	want := srv.URL + "/p1"
	if _, err := b.Navigate(ctx, id, want); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if assetHits.Load() == 0 {
		t.Fatalf("the fixture asset was never fetched — the cache assertion below would be meaningless")
	}

	res, err := b.Reload(ctx, id, false)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if res["url"] != want {
		t.Errorf("soft reload url = %v, want %s", res["url"], want)
	}

	// Only the hard reload's behaviour is asserted: whether a soft reload serves
	// the asset from cache is Chrome's call and varies by build, but bypassing
	// the cache must always produce a fresh request.
	beforeHard := assetHits.Load()
	res, err = b.Reload(ctx, id, true)
	if err != nil {
		t.Fatalf("Reload(hard): %v", err)
	}
	if res["url"] != want {
		t.Errorf("hard reload url = %v, want %s", res["url"], want)
	}
	if got := assetHits.Load(); got <= beforeHard {
		t.Errorf("asset fetches = %d, want more than %d — a hard reload must bypass the cache", got, beforeHard)
	}
}

// VS-2 (and VS-1, guarded): activate foregrounds a tab and reports honestly.
func TestActivateLive(t *testing.T) {
	b := liveCDP(t)
	srv, _ := tabFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	driven := openTab(ctx, t, b, srv.URL+"/p1")

	// VS-2 and VS-1 are separate subtests because VS-1 legitimately skips in
	// environments that never throttle, and that must not hide whether VS-2 ran.
	t.Run("reports honestly", func(t *testing.T) {
		// was_active must match what the tab reported just beforehand, so a retry
		// loop can trust it to mean "it was already foreground".
		wasVisible := b.tabVisible(ctx, driven)
		res, err := b.Activate(ctx, driven)
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		if res["was_active"] != wasVisible {
			t.Errorf("was_active = %v, want %v (sampled before acting)", res["was_active"], wasVisible)
		}
		if _, ok := res["window_focused"].(bool); !ok {
			t.Errorf("window_focused = %v, want a bool reported either way", res["window_focused"])
		}
		if res["activated"] != true {
			t.Errorf("activated = %v, want true after foregrounding a tab", res["activated"])
		}
	})

	// VS-1 — the flagship: a backgrounded tab throttles the accessibility tree,
	// and activate is the documented fix.
	t.Run("unblocks a throttled a11y tree", func(t *testing.T) {
		// Push the driven tab into the background by opening another.
		openTab(ctx, t, b, srv.URL+"/p2")

		probe, pcancel := context.WithTimeout(ctx, 8*time.Second)
		_, throttled := b.Snapshot(probe, driven, SnapOpts{})
		pcancel()
		if throttled == nil {
			// Headless builds frequently never throttle, so the failure this test
			// exists to fix cannot be reproduced. Skipping beats a flaky assertion
			// — the RFC calls for exactly this guard.
			t.Skip("this Chrome does not throttle the accessibility tree on a backgrounded tab, so VS-1's failure mode cannot be reproduced here")
		}

		if _, err := b.Activate(ctx, driven); err != nil {
			t.Fatalf("Activate after backgrounding: %v", err)
		}
		retry, rcancel := context.WithTimeout(ctx, 20*time.Second)
		defer rcancel()
		if _, err := b.Snapshot(retry, driven, SnapOpts{}); err != nil {
			t.Errorf("snapshot still fails after activate: %v — activate must unblock the throttled a11y tree (it was throttled with: %v)", err, throttled)
		}
	})
}

// VS-13: closing the last tab must report an outcome rather than hang. What the
// browser does — exit, or leave a blank tab — varies, so only the bound and the
// fact that the call returns are asserted.
func TestCloseLastTabIsBoundedLive(t *testing.T) {
	b := liveCDP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tabs, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := make([]string, 0, len(tabs))
	for _, tb := range tabs {
		ids = append(ids, tb.ID)
	}
	if len(ids) == 0 {
		t.Skip("no tabs to close")
	}

	// The guard is a separate goroutine, not just the context deadline: a browser
	// that dies mid-close can wedge a call that never consults the deadline.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = b.CloseTabs(ctx, ids)
	}()
	select {
	case <-done:
	case <-time.After(45 * time.Second):
		t.Fatal("CloseTabs on the last tab did not return — it must report an outcome, not hang")
	}
}
