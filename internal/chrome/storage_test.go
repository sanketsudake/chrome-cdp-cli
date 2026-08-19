package chrome

// Tests for the `storage` verb (RFC-0019): pure redaction/sort/cap tests that
// need no Chrome, plus testing.Short()-guarded live tests against a real
// headless Chrome. See docs/rfc/0019-web-storage.md.
//
// A `data:` fixture cannot serve VS-1..VS-7: web storage is disabled inside
// data: URLs (measured, RFC's Design notes), so the round-trip tests use an
// httptest fixture (the captureFixture style) and data:/about:blank are kept
// for the opaque-origin test alone.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/domstorage"
)

// TestStorageRedact is VS-9: the pure per-entry redaction rule.
func TestStorageRedact(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, key, value, want string
	}{
		{"plain value kept", "theme", "dark", "dark"},
		{"credential-shaped param name withheld wholesale", "access_token", "x", NetRedacted},
		{"credential-shaped header-style key withheld wholesale", "sb-abc-auth-token", "x", NetRedacted},
		{"jwt key withheld wholesale", "jwt", "x", NetRedacted},
		{"session key withheld wholesale", "session", "x", NetRedacted},
		{
			"redux-persist JSON keeps ui, withholds auth",
			"persist:root", `{"auth":"{\"token\":\"t\"}","ui":"dark"}`,
			`{"auth":"<redacted>","ui":"dark"}`,
		},
		{
			"form-encoded value redacts the token field only",
			"draft", "a=1&token=t", "a=1&token=<redacted>",
		},
		{
			"plain text containing the word token is untouched",
			"note", "plain text with token inside", "plain text with token inside",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := storageRedact(c.key, c.value); got != c.want {
				t.Errorf("storageRedact(%q, %q) = %q, want %q", c.key, c.value, got, c.want)
			}
		})
	}
}

// TestStorageRedactFirebaseAuthUser is VS-9's Firebase case, split out because
// JSON member order is not guaranteed by the redaction rewrite and the
// assertion needs substring checks rather than one equality.
func TestStorageRedactFirebaseAuthUser(t *testing.T) {
	t.Parallel()
	value := `{"uid":"u","stsTokenManager":{"accessToken":"a","refreshToken":"r"}}`
	got := storageRedact("firebase:authUser:k", value)
	for _, want := range []string{`"uid":"u"`} {
		if !strings.Contains(got, want) {
			t.Errorf("storageRedact(firebase...) = %q, want it to contain %q", got, want)
		}
	}
	for _, secret := range []string{"\"a\"", "\"r\""} {
		if strings.Contains(got, "accessToken\":"+secret) || strings.Contains(got, "refreshToken\":"+secret) {
			t.Errorf("storageRedact(firebase...) leaked a token: %q", got)
		}
	}
}

// TestStorageListResultSortsCountsAndCaps is VS-6 (pure half): sort by key,
// count is len(items), a malformed one-element entry is skipped rather than
// panicking, and the cap marks per-item and top-level truncated.
func TestStorageListResultSortsCountsAndCaps(t *testing.T) {
	t.Parallel()
	entries := []domstorage.Item{
		{"theme", "dark"},
		{"access_token", "SECRET1"},
		{"malformed"}, // one element: skipped
		{"aaa", "first"},
	}
	res := storageListResult("https://app.example", "local", entries, StorageListOpts{})
	if res["scope"] != "local" || res["origin"] != "https://app.example" {
		t.Fatalf("scope/origin = %v/%v", res["scope"], res["origin"])
	}
	items, ok := res["items"].([]map[string]any)
	if !ok {
		t.Fatalf("items has the wrong type: %T", res["items"])
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3 (the malformed entry skipped): %v", len(items), items)
	}
	wantOrder := []string{"aaa", "access_token", "theme"}
	for i, k := range wantOrder {
		if items[i]["key"] != k {
			t.Errorf("items[%d].key = %v, want %q (sorted order)", i, items[i]["key"], k)
		}
	}
	if items[1]["value"] != NetRedacted {
		t.Errorf("access_token value = %v, want redacted", items[1]["value"])
	}
	if res["count"] != 3 {
		t.Errorf("count = %v, want 3", res["count"])
	}
	if res["truncated"] != false {
		t.Errorf("truncated = %v, want false", res["truncated"])
	}

	// Cap: a value longer than MaxValue is cut and marked, top-level truncated
	// flips true; MaxValue 0 means no cap.
	long := strings.Repeat("é", 3000) // multi-byte runes, so the cut must not split one
	res2 := storageListResult("https://app.example", "local", []domstorage.Item{{"blob", long}}, StorageListOpts{MaxValue: 100})
	items2 := res2["items"].([]map[string]any)
	v, _ := items2[0]["value"].(string)
	if len(v) > 100 {
		t.Errorf("capped value is %d bytes, want <= 100", len(v))
	}
	if !strings.HasSuffix(v, "é") && len(v) > 0 {
		// A cut rune would leave an invalid tail; the last full rune must survive intact.
		if !hasValidUTF8Suffix(v) {
			t.Errorf("capped value ends mid-rune: %q", v)
		}
	}
	if items2[0]["truncated"] != true {
		t.Errorf("item truncated = %v, want true", items2[0]["truncated"])
	}
	if res2["truncated"] != true {
		t.Errorf("top-level truncated = %v, want true", res2["truncated"])
	}

	res3 := storageListResult("https://app.example", "local", []domstorage.Item{{"blob", long}}, StorageListOpts{MaxValue: 0})
	items3 := res3["items"].([]map[string]any)
	if items3[0]["value"] != long {
		t.Error("MaxValue 0 must mean no cap: value was cut")
	}
	if _, ok := items3[0]["truncated"]; ok {
		t.Errorf("MaxValue 0: item must carry no truncated key, got %v", items3[0]["truncated"])
	}
	if res3["truncated"] != false {
		t.Errorf("MaxValue 0: top-level truncated = %v, want false", res3["truncated"])
	}
}

func hasValidUTF8Suffix(s string) bool {
	return strings.ToValidUTF8(s, "�") == s
}

// TestStorageListRedactsBeforeTruncating is VS-7: an access_token JSON member
// that straddles the cap must never leak a prefix of the secret — redaction
// runs on the whole value BEFORE the cap, so the member is either fully
// redacted or removed by the cut, never partially visible.
func TestStorageListRedactsBeforeTruncating(t *testing.T) {
	t.Parallel()
	padding := strings.Repeat("a", 4000)
	value := fmt.Sprintf(`{"ui":"%s","access_token":"SECRETLONGVALUETHATWOULDSTRADDLETHECAP"}`, padding)
	res := storageListResult("https://app.example", "local", []domstorage.Item{{"state", value}}, StorageListOpts{MaxValue: 4096})
	items := res["items"].([]map[string]any)
	got, _ := items[0]["value"].(string)
	if strings.Contains(got, "SECRETLONG") {
		t.Fatalf("value leaked a prefix of the secret: %q", got)
	}
}

// TestStorageScopeIsValidated is the driver's own scope→IsLocalStorage
// mapping: a belt to the CLI's own validation, never trusting the RPC's
// lenient decoding.
func TestStorageScopeIsValidated(t *testing.T) {
	t.Parallel()
	cases := []struct {
		scope   string
		wantErr bool
		local   bool
	}{
		{"local", false, true},
		{"session", false, false},
		{"", true, false},
		{"LOCAL", true, false},
		{"bogus", true, false},
	}
	for _, c := range cases {
		t.Run(c.scope, func(t *testing.T) {
			t.Parallel()
			got, err := storageIsLocal(c.scope)
			if c.wantErr {
				if err == nil {
					t.Fatalf("storageIsLocal(%q) = nil error, want one", c.scope)
				}
				return
			}
			if err != nil {
				t.Fatalf("storageIsLocal(%q): %v", c.scope, err)
			}
			if got != c.local {
				t.Errorf("storageIsLocal(%q) = %v, want %v", c.scope, got, c.local)
			}
		})
	}
}

// storageFixture serves an httptest page with a script that seeds both
// storage areas, mirroring VS-1's given.
func storageFixture(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!doctype html><title>storage</title><script>
			localStorage.setItem('theme','dark');
			localStorage.setItem('access_token','SECRET1');
			sessionStorage.setItem('draft','hello');
		</script>`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestStorageRoundTrip is VS-1 through VS-5 on one httptest fixture.
func TestStorageRoundTrip(t *testing.T) {
	b := liveCDP(t)
	srv := storageFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	id := openTab(ctx, t, b, srv.URL+"/")

	// VS-1: list reads both areas, sorted, redacted, counted.
	localRes, err := b.StorageList(ctx, id, "local", StorageListOpts{})
	if err != nil {
		t.Fatalf("StorageList(local): %v", err)
	}
	if localRes["count"] != 2 {
		t.Fatalf("local count = %v, want 2: %v", localRes["count"], localRes)
	}
	items := localRes["items"].([]map[string]any)
	if items[0]["key"] != "access_token" || items[0]["value"] != NetRedacted {
		t.Errorf("items[0] = %v, want access_token redacted", items[0])
	}
	if items[1]["key"] != "theme" || items[1]["value"] != "dark" {
		t.Errorf("items[1] = %v, want theme=dark", items[1])
	}
	origin, _ := localRes["origin"].(string)
	if origin == "" || !strings.Contains(srv.URL, origin) {
		t.Errorf("origin = %q, want a prefix of %q", origin, srv.URL)
	}
	for _, it := range items {
		if s, ok := it["value"].(string); ok && strings.Contains(s, "SECRET1") {
			t.Fatalf("SECRET1 leaked into a redacted list: %v", items)
		}
	}

	sessRes, err := b.StorageList(ctx, id, "session", StorageListOpts{})
	if err != nil {
		t.Fatalf("StorageList(session): %v", err)
	}
	if sessRes["count"] != 1 {
		t.Fatalf("session count = %v, want 1: %v", sessRes["count"], sessRes)
	}
	sItems := sessRes["items"].([]map[string]any)
	if sItems[0]["key"] != "draft" || sItems[0]["value"] != "hello" {
		t.Errorf("session items = %v, want draft=hello", sItems)
	}

	// VS-2: --no-redact reports the token.
	noRedact, err := b.StorageList(ctx, id, "local", StorageListOpts{NoRedact: true})
	if err != nil {
		t.Fatalf("StorageList(local, NoRedact): %v", err)
	}
	nrItems := noRedact["items"].([]map[string]any)
	found := false
	for _, it := range nrItems {
		if it["key"] == "access_token" {
			found = true
			if it["value"] != "SECRET1" {
				t.Errorf("--no-redact: access_token = %v, want SECRET1", it["value"])
			}
		}
	}
	if !found {
		t.Fatal("--no-redact: access_token missing from the listing")
	}

	// VS-3: get is raw, uncut, and says when the key is absent.
	got, err := b.StorageGet(ctx, id, "local", "access_token")
	if err != nil {
		t.Fatalf("StorageGet(access_token): %v", err)
	}
	if got["value"] != "SECRET1" || got["present"] != true {
		t.Errorf("StorageGet(access_token) = %v", got)
	}
	absent, err := b.StorageGet(ctx, id, "local", "nope")
	if err != nil {
		t.Fatalf("StorageGet(nope): %v", err)
	}
	if absent["value"] != "" || absent["present"] != false {
		t.Errorf("StorageGet(nope) = %v, want value:\"\" present:false", absent)
	}

	// VS-4/VS-5: set is visible to the page, rm removes (including an absent
	// key), clear empties, scopes are independent.
	setRes, err := b.StorageSet(ctx, id, "local", "flag", "1")
	if err != nil {
		t.Fatalf("StorageSet: %v", err)
	}
	if setRes["set"] != true {
		t.Errorf("StorageSet result = %v", setRes)
	}
	if v, err := b.Eval(ctx, id, "localStorage.getItem('flag')", EvalOpts{}); err != nil {
		t.Fatalf("eval flag: %v", err)
	} else if m, _ := v.(map[string]any); m["value"] != "1" {
		t.Errorf("localStorage.flag = %v, want \"1\"", v)
	}

	rmRes, err := b.StorageRemove(ctx, id, "local", "theme")
	if err != nil {
		t.Fatalf("StorageRemove: %v", err)
	}
	if rmRes["removed"] != true {
		t.Errorf("StorageRemove result = %v", rmRes)
	}
	// VS-5: rm of an absent key still succeeds.
	rmAbsent, err := b.StorageRemove(ctx, id, "local", "never")
	if err != nil {
		t.Fatalf("StorageRemove(never): %v", err)
	}
	if rmAbsent["removed"] != true {
		t.Errorf("StorageRemove(never) = %v, want removed:true", rmAbsent)
	}

	clearRes, err := b.StorageClear(ctx, id, "session")
	if err != nil {
		t.Fatalf("StorageClear(session): %v", err)
	}
	if clearRes["cleared"] != true {
		t.Errorf("StorageClear result = %v", clearRes)
	}
	if v, err := b.Eval(ctx, id, "sessionStorage.length", EvalOpts{}); err != nil {
		t.Fatalf("eval sessionStorage.length: %v", err)
	} else if m, _ := v.(map[string]any); m["value"] != float64(0) {
		t.Errorf("sessionStorage.length = %v, want 0", v)
	}
	if v, err := b.Eval(ctx, id, "localStorage.length", EvalOpts{}); err != nil {
		t.Fatalf("eval localStorage.length: %v", err)
	} else if m, _ := v.(map[string]any); m["value"] != float64(2) {
		// access_token, flag — theme was removed above.
		t.Errorf("localStorage.length = %v, want 2", v)
	}
}

// TestStorageCapLive is VS-6 with a real 5000-byte value served by Chrome, not
// constructed by hand.
func TestStorageCapLive(t *testing.T) {
	b := liveCDP(t)
	srv := captureFixture(t, `<!doctype html><title>cap</title>`)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	id := openTab(ctx, t, b, srv.URL+"/")

	long := strings.Repeat("é", 5000/2) // multi-byte runes near the boundary
	if _, err := b.StorageSet(ctx, id, "local", "big", long); err != nil {
		t.Fatalf("StorageSet: %v", err)
	}
	res, err := b.StorageList(ctx, id, "local", StorageListOpts{MaxValue: 4096})
	if err != nil {
		t.Fatalf("StorageList: %v", err)
	}
	items := res["items"].([]map[string]any)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	v, _ := items[0]["value"].(string)
	if len(v) > 4096 {
		t.Errorf("capped value is %d bytes, want <= 4096", len(v))
	}
	if !hasValidUTF8Suffix(v) {
		t.Errorf("capped value is not valid UTF-8: %q", v)
	}
	if items[0]["truncated"] != true || res["truncated"] != true {
		t.Errorf("truncated flags = item:%v top:%v, want both true", items[0]["truncated"], res["truncated"])
	}

	res0, err := b.StorageList(ctx, id, "local", StorageListOpts{MaxValue: 0})
	if err != nil {
		t.Fatalf("StorageList(MaxValue 0): %v", err)
	}
	items0 := res0["items"].([]map[string]any)
	if items0[0]["value"] != long {
		t.Error("MaxValue 0 must report the value whole")
	}
}

// TestStorageOpaqueOriginIsRefused is VS-8: data: and about:blank tabs refuse
// before any DOMStorage command, with IsOpaqueOrigin true and the page URL
// named in the message.
func TestStorageOpaqueOriginIsRefused(t *testing.T) {
	b := liveCDP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	for _, url := range []string{"data:text/html,<title>d</title>", "about:blank"} {
		t.Run(url, func(t *testing.T) {
			id := openTab(ctx, t, b, url)
			_, err := b.StorageList(ctx, id, "local", StorageListOpts{})
			if err == nil {
				t.Fatal("StorageList on an opaque origin succeeded, want ErrOpaqueOrigin")
			}
			if !IsOpaqueOrigin(err) {
				t.Fatalf("IsOpaqueOrigin(%v) = false", err)
			}
			if !strings.Contains(err.Error(), "opaque origin") {
				t.Errorf("message = %q, want it to mention an opaque origin", err.Error())
			}
		})
	}
}
