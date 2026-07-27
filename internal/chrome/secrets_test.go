package chrome

// A password field's typed characters must not reach the envelope from ANY
// read path. Chrome masks them in the accessibility tree; the DOM-reading
// verbs have to mask them too, or `value` becomes the unguarded door that
// `find`'s masking only appears to close.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const pwSecret = "hunter2-SECRET"

func secretFixture(t *testing.T) (*CDP, context.Context, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	b, err := launch(true, tmpProfile(t), 0)
	if err != nil {
		t.Skipf("cannot launch a managed headless Chrome here: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `<!doctype html><title>Login</title><body>
<label for="p">Password</label><input type="password" id="p" class="f" value=%q>
<label for="u">Username</label><input type="text" id="u" class="f" value="alice">
</body>`, pwSecret)
	}))
	t.Cleanup(srv.Close)
	id := firstTab(ctx, t, b)
	if _, err := b.Navigate(ctx, id, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	return b, ctx, id
}

func TestValueMasksPasswordFields(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id := secretFixture(t)

	got, err := b.Value(ctx, id, "#p", QueryOpts{})
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if v, _ := got["value"].(string); v != strings.Repeat("•", len(pwSecret)) {
		t.Errorf("password value = %q, want %d bullets", v, len(pwSecret))
	}

	// A non-password field is untouched: masking must not degrade ordinary reads.
	got, err = b.Value(ctx, id, "#u", QueryOpts{})
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if v, _ := got["value"].(string); v != "alice" {
		t.Errorf("text value = %q, want alice", v)
	}
}

func TestValuesMasksPasswordFields(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-Chrome integration in -short mode")
	}
	b, ctx, id := secretFixture(t)

	got, err := b.Values(ctx, id, ".f", QueryOpts{})
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "SECRET") {
		t.Errorf("values --all leaked the password: %s", raw)
	}
	if !strings.Contains(string(raw), "alice") {
		t.Errorf("values --all lost an ordinary value: %s", raw)
	}
}
