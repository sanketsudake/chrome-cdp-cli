package chrome

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// wait --idle blocks until network activity settles — it waits out a fetch that
// starts after load, so the response has been applied by the time it returns.
func TestWaitIdle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Idle</title><body><script>
window.__done = false;
addEventListener('load', () => setTimeout(() => {
  fetch('/slow').then(() => { window.__done = true; });
}, 50));
</script></body>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	start := time.Now()
	if _, err := b.Wait(ctx, id, WaitCond{Idle: true}); err != nil {
		t.Fatalf("Wait --idle: %v", err)
	}
	// The post-load fetch (started at +50ms, ~300ms) must have completed by the
	// time idle returned.
	if got := evalString(ctx, t, b, id, "String(window.__done)"); got != "true" {
		t.Errorf("window.__done = %q after wait --idle, want true (idle returned too early, in %v)", got, time.Since(start))
	}
}

// wait --idle must still settle when a request is held open indefinitely (a
// websocket / long-poll / EventSource — the shape that made --idle hang on
// Workday): inflight never returns to zero, so it settles via the stalled path
// once the connection goes silent, rather than waiting out --timeout.
func TestWaitIdleStalledStream(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Build the server BEFORE launching Chrome so its Close() defer runs LAST
	// (LIFO), after b.Close() tears Chrome down. Otherwise srv.Close() blocks on
	// the still-open /hang request, which only unblocks when Chrome disconnects
	// — a teardown deadlock. /hang stays open until the request context is
	// cancelled (Chrome dropping the connection at b.Close()).
	mux := http.NewServeMux()
	mux.HandleFunc("/hang", func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>Stalled</title><body><script>
addEventListener('load', () => setTimeout(() => { fetch('/hang'); }, 50));
</script></body>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	b, err := launch(true, tmpProfile(t), 0, "")
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	defer b.Close()

	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// Bound the wait well under the 30s+ a held-open request would otherwise
	// cost: if the fix works it settles via the stalled path in ~2s.
	wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
	defer wcancel()
	start := time.Now()
	if _, err := b.Wait(wctx, id, WaitCond{Idle: true}); err != nil {
		t.Fatalf("Wait --idle hung on a held-open request: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 8*time.Second {
		t.Errorf("wait --idle took %v — it waited for the held-open request instead of settling on silence", elapsed)
	}
}
