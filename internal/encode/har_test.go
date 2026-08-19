package encode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// i64 and strp are small pointer helpers so the table literals below can stay
// value-shaped; NetEntry's optional fields (status, duration, error, bodies)
// are pointers so the envelope can tell "absent" from "zero".
func i64(n int64) *int64    { return &n }
func strp(s string) *string { return &s }

// harDoc is the shape of the top-level HAR document, unmarshalled generically
// so a test can index straight into nested fields without a second struct
// definition that could silently drift from the encoder's own.
type harDoc struct {
	Log struct {
		Version string `json:"version"`
		Creator struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"creator"`
		Entries []map[string]any `json:"entries"`
	} `json:"log"`
}

func decodeHAR(t *testing.T, b []byte) harDoc {
	t.Helper()
	var doc harDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("HAR output does not parse as JSON: %v\n%s", err, b)
	}
	return doc
}

// TestHARLogSkeleton is VS-7: three entries (cached, network-level failure,
// pending) must produce a valid HAR 1.2 skeleton with every required field
// present, and the worked-states table's per-state values.
func TestHARLogSkeleton(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 19, 10, 15, 0, 0, time.UTC)
	entries := []NetEntry{
		{
			ID: "r1", Method: "GET", URL: "https://app.example/api/cached", Type: "xhr",
			Status: i64(200), StatusText: "OK", StartedAt: base.Format("2006-01-02T15:04:05.000Z07:00"),
			StartedMs: 100, DurationMs: i64(42), RequestSize: 0, ResponseSize: 1024,
			FromCache: true,
		},
		{
			ID: "r2", Method: "GET", URL: "https://app.example/api/broken", Type: "xhr",
			Status: nil, StartedAt: base.Add(time.Second).Format("2006-01-02T15:04:05.000Z07:00"),
			StartedMs: 200, DurationMs: i64(10), Failed: true, Error: strp("net::ERR_CONNECTION_RESET"),
		},
		{
			ID: "r3", Method: "GET", URL: "https://app.example/api/pending", Type: "xhr",
			Status: nil, StartedAt: base.Add(2 * time.Second).Format("2006-01-02T15:04:05.000Z07:00"),
			StartedMs: 300, DurationMs: nil, Pending: true,
		},
	}
	out, err := HAR(entries, HAROpts{Version: "9.9.9", Now: base})
	if err != nil {
		t.Fatalf("HAR: %v", err)
	}
	doc := decodeHAR(t, out)
	if doc.Log.Version != "1.2" {
		t.Errorf("log.version = %q, want 1.2", doc.Log.Version)
	}
	if doc.Log.Creator.Name != "chrome-cdp" || doc.Log.Creator.Version != "9.9.9" {
		t.Errorf("log.creator = %+v", doc.Log.Creator)
	}
	if len(doc.Log.Entries) != 3 {
		t.Fatalf("log.entries has %d entries, want 3", len(doc.Log.Entries))
	}

	required := []string{"startedDateTime", "time", "request", "response", "cache", "timings"}
	for i, e := range doc.Log.Entries {
		for _, k := range required {
			if _, has := e[k]; !has {
				t.Errorf("entry %d missing %q", i, k)
			}
		}
		req := e["request"].(map[string]any)
		for _, k := range []string{"method", "url", "httpVersion", "headers", "queryString", "cookies", "headersSize", "bodySize"} {
			if _, has := req[k]; !has {
				t.Errorf("entry %d request missing %q", i, k)
			}
		}
		resp := e["response"].(map[string]any)
		for _, k := range []string{"status", "statusText", "httpVersion", "headers", "cookies", "content", "redirectURL", "headersSize", "bodySize"} {
			if _, has := resp[k]; !has {
				t.Errorf("entry %d response missing %q", i, k)
			}
		}
		timings := e["timings"].(map[string]any)
		for _, k := range []string{"send", "wait", "receive"} {
			if _, has := timings[k]; !has {
				t.Errorf("entry %d timings missing %q", i, k)
			}
		}
		// time must equal timings.wait on every entry: the whole duration is
		// attributed to wait so the spec-defined sum holds.
		if e["time"].(float64) != timings["wait"].(float64) {
			t.Errorf("entry %d time=%v != timings.wait=%v", i, e["time"], timings["wait"])
		}
	}

	cached := doc.Log.Entries[0]
	cache := cached["cache"].(map[string]any)
	if cache["_fromCache"] != true {
		t.Errorf("cached entry cache = %v, want _fromCache true", cache)
	}

	failed := doc.Log.Entries[1]
	if failed["response"].(map[string]any)["status"].(float64) != 0 {
		t.Errorf("failed entry response.status = %v, want 0", failed["response"].(map[string]any)["status"])
	}
	if failed["_failed"] != true {
		t.Errorf("failed entry _failed = %v, want true", failed["_failed"])
	}
	if failed["_error"] != "net::ERR_CONNECTION_RESET" {
		t.Errorf("failed entry _error = %v", failed["_error"])
	}

	pending := doc.Log.Entries[2]
	if pending["_pending"] != true {
		t.Errorf("pending entry _pending = %v, want true", pending["_pending"])
	}
	if pending["time"].(float64) != 0 {
		t.Errorf("pending entry time = %v, want 0", pending["time"])
	}
}

// TestHAREntriesAreSortedByStart is VS-8: entries supplied newest-first come
// back ascending by startedDateTime, independent of buffer order.
func TestHAREntriesAreSortedByStart(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	entries := []NetEntry{
		{ID: "third", Method: "GET", URL: "https://a/3", StartedAt: base.Add(2 * time.Second).Format("2006-01-02T15:04:05.000Z07:00")},
		{ID: "first", Method: "GET", URL: "https://a/1", StartedAt: base.Format("2006-01-02T15:04:05.000Z07:00")},
		{ID: "second", Method: "GET", URL: "https://a/2", StartedAt: base.Add(time.Second).Format("2006-01-02T15:04:05.000Z07:00")},
	}
	out, err := HAR(entries, HAROpts{Now: base})
	if err != nil {
		t.Fatalf("HAR: %v", err)
	}
	doc := decodeHAR(t, out)
	var prev string
	for i, e := range doc.Log.Entries {
		sdt := e["startedDateTime"].(string)
		if i > 0 && sdt < prev {
			t.Fatalf("entries not sorted ascending: entry %d (%s) < entry %d (%s)", i, sdt, i-1, prev)
		}
		prev = sdt
	}
	if doc.Log.Entries[0]["_id"] != "first" || doc.Log.Entries[2]["_id"] != "third" {
		t.Errorf("order = %v, %v, %v want first, second, third",
			doc.Log.Entries[0]["_id"], doc.Log.Entries[1]["_id"], doc.Log.Entries[2]["_id"])
	}
}

// TestHARMissingStartFallsBackToNow is VS-9: a row with no start (a stale
// daemon's response) is kept, not dropped, and flagged as such.
func TestHARMissingStartFallsBackToNow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	entries := []NetEntry{{ID: "r1", Method: "GET", URL: "https://a/x", StartedAt: ""}}
	out, err := HAR(entries, HAROpts{Now: now})
	if err != nil {
		t.Fatalf("HAR: %v", err)
	}
	doc := decodeHAR(t, out)
	want := now.Format("2006-01-02T15:04:05.000Z07:00")
	if doc.Log.Entries[0]["startedDateTime"] != want {
		t.Errorf("startedDateTime = %v, want %v", doc.Log.Entries[0]["startedDateTime"], want)
	}
	if doc.Log.Entries[0]["_startUnknown"] != true {
		t.Errorf("_startUnknown = %v, want true", doc.Log.Entries[0]["_startUnknown"])
	}
}

// TestHARWritesRedactedValuesLiterally is the SetEscapeHTML half of VS-2: the
// literal "<redacted>" placeholder must survive byte-for-byte, not become
// <redacted>, or grepping the file for it would find nothing.
func TestHARWritesRedactedValuesLiterally(t *testing.T) {
	t.Parallel()
	entries := []NetEntry{{
		ID: "r1", Method: "GET", URL: "https://app.example/cb?code=<redacted>",
		RequestHeaders: map[string]string{"authorization": "<redacted>"},
		StartedAt:      "2026-08-19T10:15:00.000Z",
	}}
	out, err := HAR(entries, HAROpts{Now: time.Now()})
	if err != nil {
		t.Fatalf("HAR: %v", err)
	}
	if !strings.Contains(string(out), `"<redacted>"`) {
		t.Errorf("output does not contain the literal redacted marker:\n%s", out)
	}
	if strings.Contains(string(out), `\u003c`) {
		t.Errorf("output HTML-escaped the redacted marker instead of writing it literally:\n%s", out)
	}
}

// TestHARQueryStringParsing checks URL order, percent-unescaping, a part with
// no "=", and that an already-redacted value is carried through unchanged.
func TestHARQueryStringParsing(t *testing.T) {
	t.Parallel()
	entries := []NetEntry{{
		ID: "r1", Method: "GET",
		URL:       "https://app.example/cb?first=a%20b&flag&code=<redacted>#frag",
		StartedAt: "2026-08-19T10:15:00.000Z",
	}}
	out, err := HAR(entries, HAROpts{Now: time.Now()})
	if err != nil {
		t.Fatalf("HAR: %v", err)
	}
	doc := decodeHAR(t, out)
	req := doc.Log.Entries[0]["request"].(map[string]any)
	if req["url"] != "https://app.example/cb?first=a%20b&flag&code=<redacted>" {
		t.Errorf("request.url = %v, want the fragment stripped", req["url"])
	}
	qs := req["queryString"].([]any)
	if len(qs) != 3 {
		t.Fatalf("queryString has %d entries, want 3: %v", len(qs), qs)
	}
	first := qs[0].(map[string]any)
	if first["name"] != "first" || first["value"] != "a b" {
		t.Errorf("queryString[0] = %v, want first=a b (unescaped)", first)
	}
	flag := qs[1].(map[string]any)
	if flag["name"] != "flag" || flag["value"] != "" {
		t.Errorf("queryString[1] = %v, want flag with empty value", flag)
	}
	code := qs[2].(map[string]any)
	if code["name"] != "code" || code["value"] != "<redacted>" {
		t.Errorf("queryString[2] = %v, want the redacted value preserved literally", code)
	}
}

// TestDecodeNetEntriesNormalisesNumbers is VS-10: rows arriving as Go's native
// int64 (in-process) and as float64 (after a daemon round trip through JSON)
// must decode to identical NetEntry values, and a nil status must become a
// nil pointer, not a pointer to zero.
func TestDecodeNetEntriesNormalisesNumbers(t *testing.T) {
	t.Parallel()
	native := []any{map[string]any{
		"id": "r1", "method": "GET", "url": "https://a/x", "type": "xhr",
		"status": nil, "status_text": "", "started_at": "2026-08-19T10:15:00.000Z",
		"started_ms": int64(150), "duration_ms": int64(30),
		"request_size": int64(0), "response_size": int64(512),
		"from_cache": false, "failed": false, "error": nil, "pending": false,
	}}
	roundTripped := []any{map[string]any{
		"id": "r1", "method": "GET", "url": "https://a/x", "type": "xhr",
		"status": nil, "status_text": "", "started_at": "2026-08-19T10:15:00.000Z",
		"started_ms": float64(150), "duration_ms": float64(30),
		"request_size": float64(0), "response_size": float64(512),
		"from_cache": false, "failed": false, "error": nil, "pending": false,
	}}
	a, err := DecodeNetEntries(native)
	if err != nil {
		t.Fatalf("DecodeNetEntries(native): %v", err)
	}
	b, err := DecodeNetEntries(roundTripped)
	if err != nil {
		t.Fatalf("DecodeNetEntries(roundTripped): %v", err)
	}
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("got %d / %d entries, want 1 / 1", len(a), len(b))
	}
	if a[0].Status != nil || b[0].Status != nil {
		t.Errorf("Status = %v / %v, want both nil", a[0].Status, b[0].Status)
	}
	if a[0].StartedMs != b[0].StartedMs || a[0].StartedMs != 150 {
		t.Errorf("StartedMs = %v / %v, want both 150", a[0].StartedMs, b[0].StartedMs)
	}
	if *a[0].DurationMs != *b[0].DurationMs || *a[0].DurationMs != 30 {
		t.Errorf("DurationMs = %v / %v, want both 30", *a[0].DurationMs, *b[0].DurationMs)
	}
	if a[0].ResponseSize != b[0].ResponseSize || a[0].ResponseSize != 512 {
		t.Errorf("ResponseSize = %v / %v, want both 512", a[0].ResponseSize, b[0].ResponseSize)
	}
}
