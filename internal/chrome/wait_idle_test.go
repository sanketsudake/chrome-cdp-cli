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

	b, err := launch(true, tmpProfile(t), 0)
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
