package chrome

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"

	"github.com/sanketsudake/chrome-cdp-cli/internal/eventbuf"
	"github.com/sanketsudake/chrome-cdp-cli/internal/result"
)

// ---------------------------------------------------------------------------
// Pure unit tests — no Chrome, so they run under -short. Correlation,
// redaction, and the grammars live here because that is where the regressions
// hide and where a failure is unambiguous.
// ---------------------------------------------------------------------------

// netEpoch is a fixed capture start, so started_ms is deterministic.
var netEpoch = time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)

func mono(t time.Time) *cdp.MonotonicTime {
	m := cdp.MonotonicTime(t)
	return &m
}

// netFeed folds a sequence of synthetic events into a buffer the way
// startNetCapture's listener does, and returns the buffer.
func netFeed(t *testing.T, max int, evs ...any) *eventbuf.Buffer[netRecord] {
	t.Helper()
	b := eventbuf.New[netRecord](max)
	for _, ev := range evs {
		key, mutate, ok := netApply(ev, DefaultNetMaxBody, netEpoch)
		if !ok {
			t.Fatalf("netApply rejected %T", ev)
		}
		b.Upsert(key, mutate)
	}
	return b
}

func willBeSent(id, method, url string, at time.Duration, typ network.ResourceType, headers map[string]any, post string) *network.EventRequestWillBeSent {
	e := &network.EventRequestWillBeSent{
		RequestID: network.RequestID(id),
		Type:      typ,
		Timestamp: mono(netEpoch.Add(at)),
		Request: &network.Request{
			URL: url, Method: method, Headers: network.Headers(headers),
		},
	}
	if post != "" {
		e.Request.HasPostData = true
		e.Request.PostDataEntries = []*network.PostDataEntry{
			{Bytes: base64.StdEncoding.EncodeToString([]byte(post))},
		}
	}
	return e
}

func responseReceived(id string, status int64, at time.Duration, typ network.ResourceType, headers map[string]any) *network.EventResponseReceived {
	return &network.EventResponseReceived{
		RequestID: network.RequestID(id),
		Type:      typ,
		Timestamp: mono(netEpoch.Add(at)),
		Response: &network.Response{
			URL: "https://app.example/api/save", Status: status, StatusText: http.StatusText(int(status)),
			Headers: network.Headers(headers),
		},
	}
}

func loadingFinished(id string, at time.Duration, size float64) *network.EventLoadingFinished {
	return &network.EventLoadingFinished{
		RequestID: network.RequestID(id), Timestamp: mono(netEpoch.Add(at)), EncodedDataLength: size,
	}
}

func loadingFailed(id string, at time.Duration, text string) *network.EventLoadingFailed {
	return &network.EventLoadingFailed{
		RequestID: network.RequestID(id), Timestamp: mono(netEpoch.Add(at)),
		Type: network.ResourceTypeXHR, ErrorText: text,
	}
}

// only returns the single record a buffer holds, failing when there is not
// exactly one — the correlation tests are all about "one record, not three".
func only(t *testing.T, b *eventbuf.Buffer[netRecord]) netRecord {
	t.Helper()
	res := b.Query(eventbuf.Query[netRecord]{})
	if res.Count != 1 {
		t.Fatalf("buffer holds %d records, want exactly 1: %+v", res.Count, res.Entries)
	}
	return res.Entries[0]
}

// The four CDP events that describe one request must fold into ONE record: a
// listing that reported three rows per request would be unusable.
func TestNetCorrelatesFourEventsIntoOneRecord(t *testing.T) {
	t.Parallel()
	b := netFeed(t, 10,
		willBeSent("req-41", "POST", "https://app.example/api/save", 1400*time.Millisecond,
			network.ResourceTypeXHR, map[string]any{"content-type": "application/json"}, `{"hours":8}`),
		responseReceived("req-41", 200, 1600*time.Millisecond, network.ResourceTypeXHR,
			map[string]any{"content-type": "application/json"}),
		loadingFinished("req-41", 1613*time.Millisecond, 88),
	)
	r := only(t, b)
	if r.ID != "req-41" || r.Method != "POST" || r.URL != "https://app.example/api/save" {
		t.Errorf("identity = %+v", r)
	}
	if r.Type != "xhr" {
		t.Errorf("type = %q, want xhr", r.Type)
	}
	if !r.HasStatus || r.Status != 200 || r.StatusText != "OK" {
		t.Errorf("status = %d/%q (has=%v)", r.Status, r.StatusText, r.HasStatus)
	}
	if r.StartedMs != 1400 {
		t.Errorf("started_ms = %d, want 1400 (ms since capture began)", r.StartedMs)
	}
	if got := r.durationMs(); got != 213 {
		t.Errorf("duration_ms = %d, want 213 (start to finish, not start to response)", got)
	}
	if r.ResponseSize != 88 {
		t.Errorf("response_size = %d, want the finished event's encoded length", r.ResponseSize)
	}
	if r.RequestBody != `{"hours":8}` || r.RequestSize != 11 {
		t.Errorf("request body = %q size %d", r.RequestBody, r.RequestSize)
	}
	if !r.Finished || r.failed() {
		t.Errorf("finished=%v failed=%v, want a finished 200 that is not failed", r.Finished, r.failed())
	}
}

// Chrome does not guarantee event order across the connection, and the naive
// implementation drops whichever event arrives before the one that "creates"
// the record. Upsert makes this a non-event: whichever arrives first creates.
func TestNetToleratesOutOfOrderEvents(t *testing.T) {
	t.Parallel()
	orders := map[string][]any{
		"finished first": {
			loadingFinished("r1", 500*time.Millisecond, 42),
			responseReceived("r1", 204, 400*time.Millisecond, network.ResourceTypeFetch, nil),
			willBeSent("r1", "DELETE", "https://app.example/api/save", 300*time.Millisecond, network.ResourceTypeFetch, nil, ""),
		},
		"response first": {
			responseReceived("r1", 204, 400*time.Millisecond, network.ResourceTypeFetch, nil),
			willBeSent("r1", "DELETE", "https://app.example/api/save", 300*time.Millisecond, network.ResourceTypeFetch, nil, ""),
			loadingFinished("r1", 500*time.Millisecond, 42),
		},
	}
	for name, evs := range orders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := only(t, netFeed(t, 10, evs...))
			if r.Method != "DELETE" || !r.HasStatus || r.Status != 204 || !r.Finished {
				t.Errorf("record = %+v, want one merged DELETE 204", r)
			}
			// The earliest timestamp seen wins, so an out-of-order arrival cannot
			// invent a negative or absurd duration.
			if d := r.durationMs(); d < 0 {
				t.Errorf("duration_ms = %d, want a non-negative duration", d)
			}
		})
	}
}

// VS-3's unit half: a network-level failure has no status at all, and reporting
// it as 0 would make it look like a real (bizarre) HTTP response.
func TestNetRecordsANetworkLevelFailure(t *testing.T) {
	t.Parallel()
	r := only(t, netFeed(t, 10,
		willBeSent("r9", "GET", "https://nope.invalid/x", 0, network.ResourceTypeXHR, nil, ""),
		loadingFailed("r9", 90*time.Millisecond, "net::ERR_NAME_NOT_RESOLVED"),
	))
	if r.HasStatus {
		t.Errorf("a failed request reported a status: %+v", r)
	}
	if !r.failed() || r.Error != "net::ERR_NAME_NOT_RESOLVED" {
		t.Errorf("failed=%v error=%q", r.failed(), r.Error)
	}
	if !r.Finished {
		t.Error("a failed request is finished; leaving it pending would hang `net wait` forever")
	}
	rendered := r.render(NetOpts{}, netBody{})
	if rendered["status"] != nil {
		t.Errorf("rendered status = %v, want null", rendered["status"])
	}
	if rendered["error"] != "net::ERR_NAME_NOT_RESOLVED" {
		t.Errorf("rendered error = %v", rendered["error"])
	}
}

// A blocked request's errorText alone says only ERR_FAILED; the reason is the
// only part a user can act on.
func TestNetFailureTextCarriesTheBlockingReason(t *testing.T) {
	t.Parallel()
	e := &network.EventLoadingFailed{
		ErrorText:     "net::ERR_BLOCKED_BY_CLIENT",
		BlockedReason: network.BlockedReasonInspector,
	}
	if got := netFailureText(e); !strings.Contains(got, "inspector") {
		t.Errorf("netFailureText = %q, want the blocked reason", got)
	}
	if got := netFailureText(&network.EventLoadingFailed{Canceled: true}); got != "canceled" {
		t.Errorf("netFailureText(canceled) = %q", got)
	}
}

func TestNetPendingIsTrueUntilTheRequestEnds(t *testing.T) {
	t.Parallel()
	b := netFeed(t, 10, willBeSent("r1", "GET", "https://app.example/slow", 0, network.ResourceTypeXHR, nil, ""))
	if r := only(t, b); r.Finished {
		t.Error("a request with only a requestWillBeSent is not finished")
	}
	key, mutate, _ := netApply(loadingFinished("r1", time.Second, 10), DefaultNetMaxBody, netEpoch)
	b.Upsert(key, mutate)
	if r := only(t, b); !r.Finished {
		t.Error("loadingFinished did not finish the record")
	}
}

// The ring's bound must show up as `dropped`, and an evicted request must not
// come back to life when a late event for it arrives — a resurrected record
// would sit at the front of the history claiming to be recent.
func TestNetEvictionCountsDroppedAndDoesNotResurrect(t *testing.T) {
	t.Parallel()
	b := eventbuf.New[netRecord](2)
	for _, id := range []string{"a", "b", "c"} {
		key, mutate, _ := netApply(
			willBeSent(id, "GET", "https://app.example/"+id, 0, network.ResourceTypeXHR, nil, ""),
			DefaultNetMaxBody, netEpoch)
		b.Upsert(key, mutate)
	}
	if got := b.Dropped(); got != 1 {
		t.Errorf("dropped = %d, want 1 — the caller must be able to tell it read too late", got)
	}
	// A late loadingFinished for the evicted "a" appends a NEW record rather than
	// reviving the old one; what it must never do is silently vanish.
	key, mutate, _ := netApply(loadingFinished("a", time.Second, 5), DefaultNetMaxBody, netEpoch)
	b.Upsert(key, mutate)
	res := b.Query(eventbuf.Query[netRecord]{})
	for _, r := range res.Entries {
		if r.ID == "a" && r.Method == "GET" {
			t.Error("an evicted request was resurrected with its original fields")
		}
	}
	if res.Buffered != 2 {
		t.Errorf("buffered = %d, want the ring bound of 2", res.Buffered)
	}
}

// ---------------------------------------------------------------------------
// Redaction. This CLI drives the user's real, logged-in Chrome, so its buffers
// hold live session credentials by construction.
// ---------------------------------------------------------------------------

func TestRedactedHeaderName(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		// The named credential headers from the RFC.
		"authorization":       true,
		"Authorization":       true,
		"  AUTHORIZATION  ":   true,
		"cookie":              true,
		"Cookie":              true,
		"set-cookie":          true,
		"Set-Cookie":          true,
		"x-api-key":           true,
		"X-API-Key":           true,
		"proxy-authorization": true,
		// The token|secret|password pattern, which no fixed list can enumerate.
		"x-csrf-token":         true,
		"X-Amz-Security-Token": true,
		"app-secret":           true,
		"x-client-secret":      true,
		"x-user-password":      true,
		"refresh_token":        true,
		// Ordinary headers must survive, or a redacted listing tells you nothing.
		"content-type":   false,
		"accept":         false,
		"user-agent":     false,
		"x-request-id":   false,
		"cache-control":  false,
		"content-length": false,
		"":               false,
	}
	for name, want := range cases {
		if got := RedactedHeaderName(name); got != want {
			t.Errorf("RedactedHeaderName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRedactHeadersReplacesValuesNotNames(t *testing.T) {
	t.Parallel()
	in := map[string]string{
		"Authorization": "Bearer secret123",
		"Cookie":        "session=abcdef",
		"Content-Type":  "application/json",
	}
	got := RedactHeaders(in, false)
	if got["Authorization"] != NetRedacted || got["Cookie"] != NetRedacted {
		t.Errorf("credentials survived redaction: %v", got)
	}
	// The NAME must stay: knowing an Authorization header was sent is exactly
	// what makes a 401 diagnosable.
	if _, has := got["Authorization"]; !has {
		t.Error("the header name was dropped; only its value should be withheld")
	}
	if got["Content-Type"] != "application/json" {
		t.Errorf("an ordinary header was redacted: %v", got)
	}
	// --no-redact is the explicit, deliberate opt-out.
	if raw := RedactHeaders(in, true); raw["Authorization"] != "Bearer secret123" {
		t.Errorf("--no-redact did not return the real value: %v", raw)
	}
	// The input map must not be mutated: the buffer keeps what the browser saw,
	// so a redacted read followed by --no-redact still works.
	if in["Authorization"] != "Bearer secret123" {
		t.Error("RedactHeaders mutated the retained record")
	}
}

// RFC-0003 open question 2: a token in a query string leaks exactly as badly as
// one in a header, and an OAuth implicit flow puts it in the fragment.
func TestRedactURL(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ in, want string }{
		"no query":            {"https://app.example/api/save", "https://app.example/api/save"},
		"ordinary params":     {"https://app.example/s?q=hours&page=2", "https://app.example/s?q=hours&page=2"},
		"access_token":        {"https://app.example/cb?access_token=abc123", "https://app.example/cb?access_token=" + NetRedacted},
		"api_key":             {"https://maps.example/v1?api_key=k1&z=3", "https://maps.example/v1?api_key=" + NetRedacted + "&z=3"},
		"bare key":            {"https://maps.example/v1?key=AIzaSy&x=1", "https://maps.example/v1?key=" + NetRedacted + "&x=1"},
		"signature":           {"https://cdn.example/f?sig=deadbeef", "https://cdn.example/f?sig=" + NetRedacted},
		"oauth code":          {"https://app.example/cb?code=xyz&state=s", "https://app.example/cb?code=" + NetRedacted + "&state=s"},
		"fragment token":      {"https://app.example/cb#access_token=abc&token_type=bearer", "https://app.example/cb#access_token=" + NetRedacted + "&token_type=bearer"},
		"plain fragment":      {"https://app.example/doc#section-3", "https://app.example/doc#section-3"},
		"percent-encoded key": {"https://app.example/x?api%5Fkey=zzz", "https://app.example/x?api%5Fkey=" + NetRedacted},
		"case insensitive":    {"https://app.example/x?Access_Token=zzz", "https://app.example/x?Access_Token=" + NetRedacted},
		"empty":               {"", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := RedactURL(c.in); got != c.want {
				t.Errorf("RedactURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	// Parameter ORDER is preserved: re-encoding through url.Values would sort
	// them, so the reported URL would differ from the one the page requested.
	in := "https://app.example/x?z=1&a=2&m=3"
	if got := RedactURL(in); got != in {
		t.Errorf("RedactURL reordered parameters: %q", got)
	}
}

// THE security regression test.
//
// It asserts on the MARSHALLED ENVELOPE BYTES, not on a struct field, because
// the failure mode is a live session token landing in a log or an agent's
// context — and that happens through JSON, not through a Go field. A test that
// checked r.ReqHeaders["authorization"] would pass while a second, unredacted
// copy of the value rode along in `url` or `set-cookie`.
func TestRedactionKeepsCredentialsOutOfTheMarshalledEnvelope(t *testing.T) {
	t.Parallel()
	const secret = "secret123"
	const cookie = "sid=deadbeefcafe"
	r := netRecord{
		ID: "req-1", Method: "POST", Type: "xhr",
		URL:       "https://app.example/api/save?access_token=" + secret,
		HasStatus: true, Status: 200, StatusText: "OK", Finished: true,
		ReqHeaders: map[string]string{
			"Authorization": "Bearer " + secret,
			"Cookie":        cookie,
			"X-Csrf-Token":  secret,
			"Content-Type":  "application/json",
		},
		RespHeaders: map[string]string{
			"Set-Cookie":   cookie + "; HttpOnly",
			"Content-Type": "application/json",
		},
	}
	env := result.Envelope{
		OK: true, Command: "net",
		Target: &result.TargetInfo{ID: "aa11", Title: "App", URL: "https://app.example/"},
		Result: map[string]any{
			"requests": []any{r.render(NetOpts{Headers: true}, netBody{})},
			"count":    1, "buffered": 1, "dropped": 0, "truncated": false, "pending": 0,
		},
	}
	raw, err := env.JSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leak := range []string{secret, cookie, "deadbeefcafe"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("REDACTION FAILED: %q appears in the emitted envelope.\n"+
				"This is a live session credential leaking into a log; fix the redaction, do not relax this test.\n%s",
				leak, raw)
		}
	}
	// And the redaction must be VISIBLE, not silently dropped: a caller has to be
	// able to tell "the header was absent" from "it was present and withheld".
	// Asserted after decoding, because encoding/json escapes the angle brackets.
	var decoded struct {
		Result struct {
			Requests []struct {
				RequestHeaders  map[string]string `json:"request_headers"`
				ResponseHeaders map[string]string `json:"response_headers"`
				URL             string            `json:"url"`
			} `json:"requests"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := decoded.Result.Requests[0]
	for name, want := range map[string]string{
		"Authorization": NetRedacted, "Cookie": NetRedacted, "X-Csrf-Token": NetRedacted,
		"Content-Type": "application/json",
	} {
		if got.RequestHeaders[name] != want {
			t.Errorf("request_headers[%q] = %q, want %q", name, got.RequestHeaders[name], want)
		}
	}
	if got.ResponseHeaders["Set-Cookie"] != NetRedacted {
		t.Errorf("response_headers[Set-Cookie] = %q, want %s", got.ResponseHeaders["Set-Cookie"], NetRedacted)
	}
	if !strings.HasSuffix(got.URL, "access_token="+NetRedacted) {
		t.Errorf("url = %q, want the access_token value withheld", got.URL)
	}

	// --no-redact is the ONLY path that emits the real value, and it must
	// actually work — otherwise the flag is a lie and people stop trusting it.
	env.Result = map[string]any{"requests": []any{r.render(NetOpts{Headers: true, NoRedact: true}, netBody{})}}
	raw, err = env.JSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), secret) {
		t.Errorf("--no-redact withheld the value anyway:\n%s", raw)
	}
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// VS-6's negative half: the keys must be ABSENT, not null. A null tells a caller
// "there was no body"; absent tells them "you did not ask".
func TestRenderOmitsHeaderAndBodyKeysUnlessRequested(t *testing.T) {
	t.Parallel()
	r := netRecord{
		ID: "r1", Method: "POST", URL: "https://app.example/api", Type: "xhr",
		HasStatus: true, Status: 200, Finished: true, RequestBody: `{"hours":8}`,
		ReqHeaders: map[string]string{"content-type": "application/json"},
	}
	plain := r.render(NetOpts{}, netBody{})
	for _, k := range []string{"request_headers", "response_headers", "request_body", "response_body", "body_unavailable", "body_truncated"} {
		if _, has := plain[k]; has {
			t.Errorf("%q is present without being asked for; the default listing must stay small", k)
		}
	}
	// The documented always-present keys must all be there, so a consumer can
	// index them without existence checks.
	for _, k := range []string{"id", "method", "url", "type", "status", "status_text", "started_ms", "duration_ms", "request_size", "response_size", "from_cache", "failed", "error"} {
		if _, has := plain[k]; !has {
			t.Errorf("result is missing %q — it is part of the documented envelope", k)
		}
	}

	withBody := r.render(NetOpts{Body: true}, netBody{Text: `{"ok":true}`, Available: true})
	if withBody["request_body"] != `{"hours":8}` || withBody["response_body"] != `{"ok":true}` {
		t.Errorf("--body did not carry both bodies: %v", withBody)
	}
	if _, has := withBody["body_unavailable"]; has {
		t.Error("an available body was marked unavailable")
	}
}

// VS-14's unit half: a body that is gone is null WITH a marker, and the read is
// still ok. Erroring would throw away everything else the listing knows.
func TestRenderReportsAnUnavailableBodyRatherThanFailing(t *testing.T) {
	t.Parallel()
	r := netRecord{ID: "r1", URL: "https://app.example/api", HasStatus: true, Status: 200, Finished: true}
	got := r.render(NetOpts{Body: true}, netBody{})
	if got["response_body"] != nil {
		t.Errorf("response_body = %v, want null", got["response_body"])
	}
	if got["body_unavailable"] != true {
		t.Error("a missing body must be marked body_unavailable, or a caller cannot tell it from an empty response")
	}
}

// VS-8's unit half: the cap marks what it cut, so a caller never mistakes a
// truncated payload for the whole thing.
func TestRenderMarksATruncatedBody(t *testing.T) {
	t.Parallel()
	r := netRecord{ID: "r1", HasStatus: true, Status: 200, Finished: true}
	got := r.render(NetOpts{Body: true}, netBody{Text: strings.Repeat("x", 10), Available: true, Truncated: true})
	if got["body_truncated"] != true {
		t.Errorf("body_truncated = %v, want true", got["body_truncated"])
	}
	// A truncated REQUEST body counts too — the marker is about the record.
	r.RequestBodyTruncated, r.RequestBody = true, "xxx"
	if got := r.render(NetOpts{Body: true}, netBody{Text: "y", Available: true}); got["body_truncated"] != true {
		t.Errorf("a truncated request body was not marked: %v", got)
	}
}

func TestRenderStatusIsNullWhileInFlight(t *testing.T) {
	t.Parallel()
	got := netRecord{ID: "r1", Method: "GET", URL: "https://app.example/slow"}.render(NetOpts{}, netBody{})
	if got["status"] != nil || got["duration_ms"] != nil {
		t.Errorf("in-flight record = %v, want null status and duration", got)
	}
	if got["failed"] != false {
		t.Error("a request still in flight is not failed")
	}
	if got["pending"] != true {
		t.Error("an unfinished record must say so")
	}
}

// ---------------------------------------------------------------------------
// The --status grammar
// ---------------------------------------------------------------------------

// VS-4: the first four specs parse and match as expected; the rest are usage.
func TestParseNetStatus(t *testing.T) {
	t.Parallel()
	valid := map[string]struct {
		spec  string
		match []int64
		miss  []int64
	}{
		"exact":        {"200", []int64{200}, []int64{201, 404, 500}},
		"class":        {"2xx", []int64{200, 204, 299}, []int64{199, 300, 404}},
		"class 4xx":    {"4xx", []int64{400, 404, 499}, []int64{399, 500}},
		"comparison":   {">=400", []int64{400, 404, 500}, []int64{399, 200}},
		"negated":      {"!2xx", []int64{404, 500, 301}, []int64{200, 204}},
		"not equal":    {"!=204", []int64{200, 404}, []int64{204}},
		"less than":    {"<400", []int64{200, 301, 399}, []int64{400, 500}},
		"spaced":       {" >= 500 ", []int64{500, 503}, []int64{499}},
		"upper class":  {"2XX", []int64{200}, []int64{404}},
		"negated code": {"!404", []int64{200, 500}, []int64{404}},
	}
	for name, c := range valid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, err := ParseNetStatus(c.spec)
			if err != nil {
				t.Fatalf("ParseNetStatus(%q): %v", c.spec, err)
			}
			for _, s := range c.match {
				if !m(s, true) {
					t.Errorf("%q did not match %d", c.spec, s)
				}
			}
			for _, s := range c.miss {
				if m(s, true) {
					t.Errorf("%q matched %d", c.spec, s)
				}
			}
		})
	}

	invalid := []string{"20x", "abc", "", "  ", "2xxx", "x", "0xx", "6xx", "20", "1234", ">=", ">=abc", "!", "!!200", ">=4000", "-200", "2 0 0"}
	for _, spec := range invalid {
		t.Run("rejects "+spec, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseNetStatus(spec); err == nil {
				t.Errorf("ParseNetStatus(%q) parsed; a spec that is not exactly valid must be rejected, "+
					"not silently turned into a filter that matches everything", spec)
			}
		})
	}
}

// A record with no status is a network-level failure or a request still in
// flight. Letting it satisfy "!2xx" would quietly make every pending request an
// assertion failure.
func TestNetStatusNeverMatchesAStatuslessRecord(t *testing.T) {
	t.Parallel()
	for _, spec := range []string{"200", "2xx", ">=400", "!2xx", "!=204", "<=599"} {
		m, err := ParseNetStatus(spec)
		if err != nil {
			t.Fatalf("ParseNetStatus(%q): %v", spec, err)
		}
		if m(0, false) {
			t.Errorf("%q matched a record with no status", spec)
		}
	}
}

func TestNormalizeNetType(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in   string
		want string
		ok   bool
	}{
		"document":      {"document", "document", true},
		"xhr":           {"xhr", "xhr", true},
		"fetch":         {"fetch", "fetch", true},
		"case tolerant": {"  Image ", "image", true},
		"css alias":     {"css", "stylesheet", true},
		"img alias":     {"img", "image", true},
		"ws alias":      {"ws", "websocket", true},
		"chrome's XHR":  {"XHR", "xhr", true},
		"unknown":       {"widget", "", false},
		"empty":         {"", "", false},
		"almost a type": {"images", "", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := NormalizeNetType(c.in)
			if got != c.want || ok != c.ok {
				t.Errorf("NormalizeNetType(%q) = %q,%v; want %q,%v", c.in, got, ok, c.want, c.ok)
			}
		})
	}
	// Every CDP resource type must normalize, or a real request lands with a
	// type no --type value can select.
	for _, rt := range []network.ResourceType{
		network.ResourceTypeDocument, network.ResourceTypeStylesheet, network.ResourceTypeImage,
		network.ResourceTypeMedia, network.ResourceTypeFont, network.ResourceTypeScript,
		network.ResourceTypeTextTrack, network.ResourceTypeXHR, network.ResourceTypeFetch,
		network.ResourceTypePrefetch, network.ResourceTypeEventSource, network.ResourceTypeWebSocket,
		network.ResourceTypeManifest, network.ResourceTypeSignedExchange, network.ResourceTypePing,
		network.ResourceTypeCSPViolationReport, network.ResourceTypePreflight, network.ResourceTypeFedCM,
		network.ResourceTypeOther,
	} {
		if _, ok := NormalizeNetType(netType(rt)); !ok {
			t.Errorf("CDP resource type %q does not normalize; --type could never select it", rt)
		}
	}
}

func TestNetURLMatcher(t *testing.T) {
	t.Parallel()
	sub, err := NetURLMatcher("/api/save")
	if err != nil {
		t.Fatal(err)
	}
	if !sub("https://app.example/api/save?x=1") || sub("https://app.example/api/load") {
		t.Error("substring matching is wrong")
	}
	re, err := NetURLMatcher(`re:/api/(save|load)$`)
	if err != nil {
		t.Fatal(err)
	}
	if !re("https://app.example/api/load") || re("https://app.example/api/other") {
		t.Error("re: matching is wrong")
	}
	if _, err := NetURLMatcher("re:("); err == nil {
		t.Error("an invalid regex after re: must be an error even at the daemon boundary")
	}
	if m, err := NetURLMatcher(""); err != nil || m != nil {
		t.Error("an empty --url must be a nil matcher, so the buffer skips the per-entry call")
	}
}

func TestNetKeepComposesFilters(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 11, 0, 0, 0, time.UTC)
	recs := []netRecord{
		{URL: "https://app/api/save", Method: "POST", Type: "xhr", HasStatus: true, Status: 200, Started: now.Add(-time.Second)},
		{URL: "https://app/api/save", Method: "GET", Type: "xhr", HasStatus: true, Status: 200, Started: now.Add(-time.Second)},
		{URL: "https://app/api/load", Method: "POST", Type: "xhr", HasStatus: true, Status: 200, Started: now.Add(-time.Second)},
		{URL: "https://app/api/save", Method: "POST", Type: "image", HasStatus: true, Status: 200, Started: now.Add(-time.Second)},
		{URL: "https://app/api/save", Method: "POST", Type: "xhr", HasStatus: true, Status: 500, Started: now.Add(-time.Second)},
		{URL: "https://app/api/save", Method: "POST", Type: "xhr", HasStatus: true, Status: 200, Started: now.Add(-time.Hour)},
	}
	keep, err := netKeep(NetOpts{
		URL: "/api/save", Methods: []string{"post"}, Types: []string{"xhr"},
		Status: "2xx", Since: time.Minute,
	}, now)
	if err != nil {
		t.Fatalf("netKeep: %v", err)
	}
	kept := 0
	for _, r := range recs {
		if keep(r) {
			kept++
		}
	}
	if kept != 1 {
		t.Errorf("kept %d records, want only the recent POST xhr /api/save 200", kept)
	}

	nilKeep, err := netKeep(NetOpts{Limit: 10, Headers: true}, now)
	if err != nil {
		t.Errorf("netKeep(no filters): %v", err)
	}
	if nilKeep != nil {
		t.Error("netKeep with no filters must return a nil predicate, so the buffer skips the per-entry call")
	}

	if _, err := netKeep(NetOpts{Status: "abc"}, now); err == nil {
		t.Error("an invalid --status must be an error even at the daemon boundary")
	}
	if _, err := netKeep(NetOpts{URL: "re:("}, now); err == nil {
		t.Error("an invalid --url regex must be an error even at the daemon boundary")
	}
}

// --failed means "non-2xx OR a network-level failure", which is one predicate
// with three quite different sources.
func TestNetFailedCoversStatusAndTransport(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		r    netRecord
		want bool
	}{
		"200":               {netRecord{HasStatus: true, Status: 200}, false},
		"204":               {netRecord{HasStatus: true, Status: 204}, false},
		"301":               {netRecord{HasStatus: true, Status: 301}, true},
		"401":               {netRecord{HasStatus: true, Status: 401}, true},
		"500":               {netRecord{HasStatus: true, Status: 500}, true},
		"transport failure": {netRecord{NetFailed: true}, true},
		"in flight":         {netRecord{}, false},
	}
	for name, c := range cases {
		if got := c.r.failed(); got != c.want {
			t.Errorf("%s: failed() = %v, want %v", name, got, c.want)
		}
	}
}

func TestNetPostDataDecodesBase64Entries(t *testing.T) {
	t.Parallel()
	req := &network.Request{PostDataEntries: []*network.PostDataEntry{
		{Bytes: base64.StdEncoding.EncodeToString([]byte(`{"a":`))},
		{Bytes: base64.StdEncoding.EncodeToString([]byte(`1}`))},
	}}
	if got := netPostData(req); got != `{"a":1}` {
		t.Errorf("netPostData = %q, want the reassembled body", got)
	}
	if got := netPostData(&network.Request{}); got != "" {
		t.Errorf("netPostData(no entries) = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Live Chrome — skipped under -short, not parallel (they share a browser).
// ---------------------------------------------------------------------------

// netFixtures serves everything the live tests need over a LOCAL httptest
// server: a 200 JSON endpoint, a 500, a slow endpoint, an oversized body, an
// image, and a stylesheet. Local rather than the public internet, so the tests
// are hermetic and CI-safe.
func netFixtures(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/api/ok", func(w http.ResponseWriter, r *http.Request) {
		// A small delay, so duration_ms is meaningfully positive rather than a
		// sub-millisecond round trip that rounds to zero.
		time.Sleep(40 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "sid=livecredential; Path=/")
		_, _ = io_WriteString(w, `{"ok":true}`)
		_ = r
	})
	mux.HandleFunc("/api/boom", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io_WriteString(w, `{"error":"boom"}`)
	})
	mux.HandleFunc("/api/slow", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io_WriteString(w, `{"slow":true}`)
	})
	mux.HandleFunc("/api/big", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io_WriteString(w, strings.Repeat("A", 8<<10))
	})
	// The same binary payload at two sizes, for the size-independence of the
	// "this is not text" answer. 100 KB is over the 64 KB body cap; 1 KB is not.
	mux.HandleFunc("/api/blob", func(w http.ResponseWriter, r *http.Request) {
		n := 100 << 10
		if r.URL.Query().Get("small") != "" {
			n = 1 << 10
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(bytes.Repeat([]byte{0xff}, n))
	})
	mux.HandleFunc("/style.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = io_WriteString(w, "body{color:#333}")
	})
	mux.HandleFunc("/pixel.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(onePixelPNG())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// A page that loads a stylesheet and an image at parse time and exposes the
	// API calls behind buttons, so a test can scope a read to one action.
	page := fmt.Sprintf(`<!doctype html><title>Net fixture</title>
<link rel="stylesheet" href="%s/style.css">
<img src="%s/pixel.png" width="1" height="1">
<button id="ok" onclick="fetch('%s/api/ok')">ok</button>
<button id="post" onclick="fetch('%s/api/ok',{method:'POST',headers:{'Authorization':'Bearer secret123','Content-Type':'application/json'},body:JSON.stringify({hours:8})})">post</button>
<button id="boom" onclick="fetch('%s/api/boom')">boom</button>
<button id="slow" onclick="fetch('%s/api/slow')">slow</button>
<button id="big" onclick="fetch('%s/api/big')">big</button>
<button id="blobbig" onclick="fetch('%s/api/blob')">blob big</button>
<button id="blobsmall" onclick="fetch('%s/api/blob?small=1')">blob small</button>
<button id="dead" onclick="fetch('%s/gone').catch(()=>{})">dead</button>`,
		srv.URL, srv.URL, srv.URL, srv.URL, srv.URL, srv.URL, srv.URL, srv.URL, srv.URL, deadAddr(t))
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io_WriteString(w, page)
	})
	return srv, srv.URL + "/page"
}

// io_WriteString keeps the fixture handlers to one import.
func io_WriteString(w http.ResponseWriter, s string) (int, error) { return w.Write([]byte(s)) }

// onePixelPNG is a minimal valid PNG, so the image request is a real image
// rather than a 404 with an image content-type.
func onePixelPNG() []byte {
	b, _ := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	return b
}

// deadAddr returns an address nothing is listening on, for the network-level
// failure case — hermetic, unlike a made-up public hostname whose resolution
// depends on the CI network and its DNS wildcards.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

// netRequests unpacks a Net result into the rendered request objects.
func netRequests(t *testing.T, res any) []map[string]any {
	t.Helper()
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("net result is %T, want a map", res)
	}
	raw, _ := m["requests"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		rm, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("request is %T, want a map", r)
		}
		out = append(out, rm)
	}
	return out
}

// awaitNet polls Net until want is satisfied, so a test asserts on what the page
// requested rather than on how fast the event loop delivered it. It returns the
// last read either way, so the failure message shows what arrived.
func awaitNet(ctx context.Context, t *testing.T, b *CDP, id string, opts NetOpts, want func([]map[string]any) bool) (any, []map[string]any) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastRes any
	var last []map[string]any
	for {
		res, err := b.Net(ctx, id, opts)
		if err != nil {
			t.Fatalf("Net: %v", err)
		}
		lastRes, last = res, netRequests(t, res)
		if want(last) || time.Now().After(deadline) {
			return lastRes, last
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// netDone reports that a request for sub is present AND finished.
//
// `status != nil` is NOT the same condition: responseReceived lands before
// loadingFinished, and a read in that window sees a record with a status but no
// `duration_ms` and no retained body. That is not a bug — it is precisely what
// the envelope's `pending` flag exists to say — so a test asserting on a
// COMPLETED request has to wait for completion rather than for a status.
func netDone(sub string) func([]map[string]any) bool {
	return func(reqs []map[string]any) bool {
		got := findURL(reqs, sub)
		return got != nil && got["pending"] == false
	}
}

// awaitNetDone blocks until a request matching cond has completed, using the
// product's own waiting primitive (`net wait` / `wait --request`) rather than a
// guess about how fast Chrome is.
//
// NetWait only answers for a record the buffer already holds as finished — the
// subscriber is notified after the entry is written — so a `net` read taken
// afterwards deterministically sees the completed record.
func awaitNetDone(ctx context.Context, t *testing.T, b *CDP, id string, cond NetCond) {
	t.Helper()
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := b.NetWait(wctx, id, cond); err != nil {
		t.Fatalf("no request matching %s completed: %v", cond.describe(), err)
	}
}

func findURL(reqs []map[string]any, sub string) map[string]any {
	for _, r := range reqs {
		if u, _ := r["url"].(string); strings.Contains(u, sub) {
			return r
		}
	}
	return nil
}

func hasURL(reqs []map[string]any, sub string) bool { return findURL(reqs, sub) != nil }

// nextStreamed returns the next streamed record whose URL contains sub, skipping
// any others, or nil if none arrives within the window.
func nextStreamed(ch <-chan map[string]any, sub string, within time.Duration) map[string]any {
	deadline := time.After(within)
	for {
		select {
		case r := <-ch:
			if u, _ := r["url"].(string); strings.Contains(u, sub) {
				return r
			}
		case <-deadline:
			return nil
		}
	}
}

// netLiveTab launches Chrome, opens the fixture page, and returns the tab.
func netLiveTab(ctx context.Context, t *testing.T, b *CDP, page string) string {
	t.Helper()
	id := firstTab(ctx, t, b)
	// Attaching is what starts capture — this stands in for the earlier command
	// a real session would have run before reading.
	if _, err := b.Navigate(ctx, id, "about:blank"); err != nil {
		t.Fatalf("attach nav: %v", err)
	}
	if _, err := b.Navigate(ctx, id, page); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	return id
}

// VS-1: a completed XHR is captured with its method, URL, status, and a positive
// duration.
func TestNetCapturesACompletedXHR(t *testing.T) {
	b := liveChrome(t)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	if _, err := b.Pointer(ctx, id, "#ok", PointerOpts{Action: PointerClick}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	// The scenario is a COMPLETED XHR, so block on completion with the verb that
	// exists for exactly that. Reading as soon as a status appears races
	// loadingFinished and reports a still-pending record with no duration.
	awaitNetDone(ctx, t, b, id, NetCond{URL: "/api/ok", Status: "2xx"})

	res, err := b.Net(ctx, id, NetOpts{Types: []string{"xhr", "fetch"}, Limit: 100})
	if err != nil {
		t.Fatalf("Net: %v", err)
	}
	reqs := netRequests(t, res)
	got := findURL(reqs, "/api/ok")
	if got == nil {
		t.Fatalf("the fetch was not captured; --xhr returned %v", reqs)
	}
	if got["pending"] != false {
		t.Errorf("pending = %v after the request completed; the rest of this scenario is about a finished request", got["pending"])
	}
	if got["method"] != "GET" {
		t.Errorf("method = %v, want GET", got["method"])
	}
	if s, _ := got["status"].(int64); s != 200 {
		t.Errorf("status = %v, want 200", got["status"])
	}
	if d, _ := got["duration_ms"].(int64); d <= 0 {
		t.Errorf("duration_ms = %v, want a positive duration", got["duration_ms"])
	}
	if got["failed"] != false {
		t.Errorf("failed = %v, want false for a 200", got["failed"])
	}
}

// VS-2: a delivered 500 has BOTH a status and failed=true — the status proves it
// reached the server, failed makes it findable with --failed.
func TestNetCapturesAFailedResponse(t *testing.T) {
	b := liveChrome(t)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	if _, err := b.Pointer(ctx, id, "#boom", PointerOpts{Action: PointerClick}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	_, reqs := awaitNet(ctx, t, b, id, NetOpts{Failed: true, Limit: 100}, func(r []map[string]any) bool {
		return hasURL(r, "/api/boom")
	})
	got := findURL(reqs, "/api/boom")
	if got == nil {
		t.Fatalf("--failed did not list the 500: %v", reqs)
	}
	if s, _ := got["status"].(int64); s != 500 {
		t.Errorf("status = %v, want 500", got["status"])
	}
	if got["failed"] != true {
		t.Errorf("failed = %v, want true", got["failed"])
	}
}

// VS-3: a request that never reached a server has no status at all, and the
// error text is the only thing that explains it.
func TestNetCapturesANetworkLevelFailure(t *testing.T) {
	b := liveChrome(t)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	if _, err := b.Pointer(ctx, id, "#dead", PointerOpts{Action: PointerClick}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	_, reqs := awaitNet(ctx, t, b, id, NetOpts{Failed: true, Limit: 100}, func(r []map[string]any) bool {
		got := findURL(r, "/gone")
		return got != nil && got["error"] != nil
	})
	got := findURL(reqs, "/gone")
	if got == nil {
		t.Fatalf("--failed did not list the unreachable request: %v", reqs)
	}
	if got["failed"] != true {
		t.Errorf("failed = %v, want true", got["failed"])
	}
	if got["status"] != nil {
		t.Errorf("status = %v, want null — nothing answered", got["status"])
	}
	if s, _ := got["error"].(string); s == "" {
		t.Error("error is empty; it is the only thing that explains a transport failure")
	}
}

// VS-5: type filtering keeps the listing to the handful of API calls that matter
// while `buffered` still shows the page loaded much more than that.
func TestNetTypeFilteringExcludesImagesAndStylesheets(t *testing.T) {
	b := liveChrome(t)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	if _, err := b.Pointer(ctx, id, "#ok", PointerOpts{Action: PointerClick}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	res, reqs := awaitNet(ctx, t, b, id, NetOpts{Types: []string{"xhr", "fetch"}, Limit: 100}, func(r []map[string]any) bool {
		return hasURL(r, "/api/ok")
	})
	if len(reqs) != 1 {
		t.Errorf("--xhr returned %d requests, want 1: %v", len(reqs), reqs)
	}
	m := res.(map[string]any)
	if n, _ := m["buffered"].(int); n < 3 {
		t.Errorf("buffered = %v, want >= 3 (the document, the stylesheet, and the image are all retained)", m["buffered"])
	}
	if hasURL(reqs, "style.css") || hasURL(reqs, "pixel.png") {
		t.Errorf("--xhr leaked a stylesheet or an image: %v", reqs)
	}
}

// VS-6: bodies are opt-in, and when opted into they carry what the fixture sent.
func TestNetBodiesAreOptIn(t *testing.T) {
	b := liveChrome(t)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	if _, err := b.Pointer(ctx, id, "#post", PointerOpts{Action: PointerClick}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	// Bodies are fetched lazily at read time, and Chrome only retains one for a
	// request that has finished — so the read has to happen after completion, not
	// merely after a status. Waiting is the caller's job and `net wait` is how it
	// is expressed.
	awaitNetDone(ctx, t, b, id, NetCond{URL: "/api/ok", Methods: []string{"POST"}, Status: "2xx"})

	base := NetOpts{URL: "/api/ok", Limit: 10}
	plain, err := b.Net(ctx, id, base)
	if err != nil {
		t.Fatalf("Net: %v", err)
	}
	reqs := netRequests(t, plain)
	if len(reqs) == 0 {
		t.Fatal("the POST was not captured")
	}
	for _, k := range []string{"request_body", "response_body"} {
		if _, has := reqs[0][k]; has {
			t.Errorf("%q is present without --body", k)
		}
	}

	withBody := base
	withBody.Body = true
	res, err := b.Net(ctx, id, withBody)
	if err != nil {
		t.Fatalf("Net --body: %v", err)
	}
	got := findURL(netRequests(t, res), "/api/ok")
	if got == nil {
		t.Fatal("the POST disappeared on the --body read")
	}
	if s, _ := got["request_body"].(string); !strings.Contains(s, `"hours":8`) {
		t.Errorf("request_body = %v, want the posted JSON", got["request_body"])
	}
	if s, _ := got["response_body"].(string); !strings.Contains(s, `"ok":true`) {
		t.Errorf("response_body = %v, want the served JSON", got["response_body"])
	}
}

// VS-7 (live half): the Authorization header the page sent is reported as
// present-but-withheld, and the secret is nowhere in the marshalled envelope.
func TestNetRedactsLiveCredentialsInTheEnvelope(t *testing.T) {
	b := liveChrome(t)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	if _, err := b.Pointer(ctx, id, "#post", PointerOpts{Action: PointerClick}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	opts := NetOpts{URL: "/api/ok", Headers: true, Limit: 10}
	// Wait for the whole exchange, not just the request half: the Set-Cookie
	// assertion below is about RESPONSE headers, and asserting a secret is absent
	// from headers that have not arrived yet proves nothing.
	res, reqs := awaitNet(ctx, t, b, id, opts, func(r []map[string]any) bool {
		if !netDone("/api/ok")(r) {
			return false
		}
		h, _ := findURL(r, "/api/ok")["request_headers"].(map[string]string)
		_, ok := h["Authorization"]
		return ok
	})
	got := findURL(reqs, "/api/ok")
	if got == nil {
		t.Fatalf("the POST was not captured: %v", reqs)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "secret123") {
		t.Fatalf("REDACTION FAILED: the live Authorization value appears in the result:\n%s", raw)
	}
	if strings.Contains(string(raw), "livecredential") {
		t.Fatalf("REDACTION FAILED: the live Set-Cookie value appears in the result:\n%s", raw)
	}

	// --no-redact is the explicit opt-out and must actually produce the value,
	// or nobody will trust that the default was doing anything.
	opts.NoRedact = true
	rawRes, err := b.Net(ctx, id, opts)
	if err != nil {
		t.Fatalf("Net --no-redact: %v", err)
	}
	plain, err := json.Marshal(rawRes)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(plain), "secret123") {
		t.Errorf("--no-redact withheld the value anyway:\n%s", plain)
	}
}

// VS-8: a response larger than net_max_body is cut at the cap and says so.
func TestNetTruncatesAnOversizedBody(t *testing.T) {
	b := liveChrome(t)
	// Shrink the cap BEFORE the first attach, so the test does not have to serve
	// a 64 KB payload to prove a 64 KB rule.
	const cap = 1024
	b.configureNetCapture(DefaultNetBuffer, cap)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	if _, err := b.Pointer(ctx, id, "#big", PointerOpts{Action: PointerClick}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	opts := NetOpts{URL: "/api/big", Body: true, Limit: 10}
	_, reqs := awaitNet(ctx, t, b, id, opts, func(r []map[string]any) bool {
		got := findURL(r, "/api/big")
		return got != nil && got["response_body"] != nil
	})
	got := findURL(reqs, "/api/big")
	if got == nil {
		t.Fatalf("the oversized response was not captured: %v", reqs)
	}
	body, _ := got["response_body"].(string)
	if len(body) != cap {
		t.Errorf("response_body is %d bytes, want it cut to the %d-byte cap", len(body), cap)
	}
	if got["body_truncated"] != true {
		t.Error("body_truncated is not set; a caller would take the cut payload for the whole thing")
	}
}

// Regression: whether a body is text must not depend on its SIZE.
//
// The check used to run on the TRUNCATED text, so a binary payload under the cap
// was correctly reported unavailable while the same payload over the cap was cut
// (to "", before the truncation fix) and emitted as an empty-but-present body
// with body_truncated set. Identical content, opposite answers, decided by
// nothing the caller can see.
func TestNetReportsABinaryBodyAsUnavailableAtEverySize(t *testing.T) {
	b := liveChrome(t)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	for _, c := range []struct {
		name, button, url string
	}{
		{"over the body cap", "#blobbig", "/api/blob"},
		{"under the body cap", "#blobsmall", "/api/blob?small=1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := b.Pointer(ctx, id, c.button, PointerOpts{Action: PointerClick}); err != nil {
				t.Fatalf("Click: %v", err)
			}
			awaitNetDone(ctx, t, b, id, NetCond{URL: c.url})
			_, reqs := awaitNet(ctx, t, b, id, NetOpts{URL: c.url, Body: true, Limit: 10}, netDone(c.url))
			got := findURL(reqs, c.url)
			if got == nil {
				t.Fatalf("the binary response was not captured: %v", reqs)
			}
			if got["body_unavailable"] != true {
				t.Errorf("a binary body was not marked body_unavailable: %v", got)
			}
			if got["response_body"] != nil {
				t.Errorf("response_body = %q, want null — a binary payload is not text", got["response_body"])
			}
		})
	}
}

// VS-9: a wait returns as soon as the request lands, not when --timeout expires.
func TestNetWaitReturnsWhenTheRequestCompletes(t *testing.T) {
	b := liveChrome(t)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	if _, err := b.Pointer(ctx, id, "#slow", PointerOpts{Action: PointerClick}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	start := time.Now()
	res, err := b.NetWait(wctx, id, NetCond{URL: "/api/slow", Status: "2xx"})
	if err != nil {
		t.Fatalf("NetWait: %v", err)
	}
	elapsed := time.Since(start)
	if res["matched"] != true {
		t.Errorf("result = %v, want matched", res)
	}
	req, _ := res["request"].(map[string]any)
	if req == nil || !strings.Contains(req["url"].(string), "/api/slow") {
		t.Errorf("request = %v, want the slow endpoint", res["request"])
	}
	if s, _ := req["status"].(int64); s != 200 {
		t.Errorf("status = %v, want 200", req["status"])
	}
	if elapsed > 20*time.Second {
		t.Errorf("NetWait took %v; it must return on completion, not wait out the timeout", elapsed)
	}
}

// VS-10: the race a naive implementation loses — the request completed BEFORE
// the wait was invoked, so a wait that only subscribes would time out.
func TestNetWaitDoesNotMissAnAlreadyCompletedRequest(t *testing.T) {
	b := liveChrome(t)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	if _, err := b.Pointer(ctx, id, "#ok", PointerOpts{Action: PointerClick}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	// Let it FINISH and be buffered, so the wait below is answered entirely from
	// history — nothing new will arrive for it. Polling `net` rather than using
	// NetWait keeps the setup independent of the primitive under test; waiting
	// only for a status would leave the request still in flight, and the wait
	// below would then be answered by the subscription it is supposed to bypass.
	awaitNet(ctx, t, b, id, NetOpts{URL: "/api/ok", Limit: 10}, netDone("/api/ok"))

	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	defer wcancel()
	start := time.Now()
	res, err := b.NetWait(wctx, id, NetCond{URL: "/api/ok", Status: "2xx"})
	if err != nil {
		t.Fatalf("NetWait timed out on a request that had already completed and was still buffered: %v", err)
	}
	if res["matched"] != true {
		t.Errorf("result = %v", res)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("NetWait took %v for an already-buffered match; it must answer from history immediately", d)
	}
}

// VS-11: nothing matches, so the wait times out — as a deadline error the CLI
// maps to target_timeout / exit 4, not as a silent empty success.
func TestNetWaitTimesOutWithNoMatch(t *testing.T) {
	b := liveChrome(t)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	wctx, wcancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer wcancel()
	res, err := b.NetWait(wctx, id, NetCond{URL: "/never/happens"})
	if err == nil {
		t.Fatalf("NetWait succeeded with no matching request: %v", res)
	}
	if !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("err = %v; the CLI classifies a timeout by its deadline text, so it must survive", err)
	}
	if got := classifyActionErrForTest(err); got != result.CodeTargetTimeout {
		t.Errorf("a NetWait timeout classifies as %q, want target_timeout (exit 4)", got)
	}
}

// classifyActionErrForTest mirrors the CLI's classification, so the chrome-side
// test can assert the timeout reaches exit 4 without importing the cli package.
func classifyActionErrForTest(err error) string {
	if strings.Contains(strings.ToLower(err.Error()), "deadline exceeded") {
		return result.CodeTargetTimeout
	}
	return result.CodeCDP
}

// VS-12: clear, act, read — the second read shows only what the action produced.
func TestNetClearScopesTheReadToOneAction(t *testing.T) {
	b := liveChrome(t)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	// Let the page's own load traffic (document, stylesheet, image) land first,
	// so the clear below has something to clear.
	awaitNet(ctx, t, b, id, NetOpts{Limit: 100}, func(r []map[string]any) bool {
		return hasURL(r, "style.css") && hasURL(r, "pixel.png")
	})
	if _, err := b.Net(ctx, id, NetOpts{Clear: true}); err != nil {
		t.Fatalf("Net --clear: %v", err)
	}
	if _, err := b.Pointer(ctx, id, "#ok", PointerOpts{Action: PointerClick}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	_, reqs := awaitNet(ctx, t, b, id, NetOpts{Types: []string{"xhr", "fetch"}, Limit: 100}, func(r []map[string]any) bool {
		return hasURL(r, "/api/ok")
	})
	if len(reqs) != 1 {
		t.Errorf("after clear+click, --xhr shows %d requests, want exactly the click's fetch: %v", len(reqs), reqs)
	}
}

// VS-13: `pending` is what lets a caller tell "nothing matched" from "not
// finished yet" — without it, an empty listing during a slow save is
// indistinguishable from a save that never fired.
func TestNetPendingCountsInFlightRequests(t *testing.T) {
	b := liveChrome(t)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	if _, err := b.Pointer(ctx, id, "#slow", PointerOpts{Action: PointerClick}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := b.Net(ctx, id, NetOpts{Limit: 100})
		if err != nil {
			t.Fatalf("Net: %v", err)
		}
		if n, _ := res.(map[string]any)["pending"].(int); n >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending never reached 1 while a 1.5s request was in flight: %v", res)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// VS-14: a body that is gone after a navigation is reported as null WITH the
// marker, and the read still succeeds. The invariant is checkable without
// forcing Chrome to evict: a null body must never appear unmarked.
func TestNetBodyUnavailableIsMarkedNotErrored(t *testing.T) {
	b := liveChrome(t)
	srv, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	if _, err := b.Pointer(ctx, id, "#ok", PointerOpts{Action: PointerClick}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	// Wait for completion first: a body missing because the request has not
	// finished yet is a different thing from one evicted by the navigation, and
	// only the second is what this scenario is about.
	awaitNet(ctx, t, b, id, NetOpts{URL: "/api/ok", Limit: 10}, netDone("/api/ok"))
	// Navigate away: the renderer that held the response body is gone.
	if _, err := b.Navigate(ctx, id, srv.URL+"/api/ok"); err != nil {
		t.Fatalf("Navigate away: %v", err)
	}

	res, err := b.Net(ctx, id, NetOpts{Body: true, Limit: 100})
	if err != nil {
		t.Fatalf("Net --body after navigation must still succeed, not error: %v", err)
	}
	reqs := netRequests(t, res)
	if len(reqs) == 0 {
		t.Fatal("the pre-navigation requests were all lost")
	}
	for _, r := range reqs {
		body, hasBody := r["response_body"]
		if !hasBody {
			t.Errorf("--body left response_body absent on %v; it must be present, null or not", r["url"])
			continue
		}
		if body == nil && r["body_unavailable"] != true {
			t.Errorf("a null response_body on %v carries no body_unavailable marker, so a caller "+
				"cannot tell an empty response from one that is gone", r["url"])
		}
	}
}

// With nothing alive to have received them, earlier requests are absent,
// buffered is 0, and the envelope SAYS so — it must not pass an empty list off
// as a page that made no requests. This is the --no-daemon situation.
func TestNetWithoutRetainedHistoryDoesNotFabricateIt(t *testing.T) {
	b := liveChrome(t)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Open creates and navigates the tab at the BROWSER level, so nothing is
	// attached to it while it loads.
	opened, err := b.Open(ctx, page)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id, _ := opened["id"].(string)
	if id == "" {
		t.Fatalf("Open returned no id: %v", opened)
	}
	t.Cleanup(func() { _, _ = b.CloseTabs(context.Background(), []string{id}) })
	// Let the page finish loading BEFORE the first attach, so the test is about
	// retention and not about winning a race with the load event.
	time.Sleep(2 * time.Second)

	res, err := b.Net(ctx, id, NetOpts{Limit: 100})
	if err != nil {
		t.Fatalf("Net: %v", err)
	}
	m := res.(map[string]any)
	if got := m["buffered"]; got != 0 {
		t.Errorf("buffered = %v, want 0 — there was no retained history to report", got)
	}
	if note, _ := m["note"].(string); note == "" {
		t.Error("no note: an empty request list with no explanation reads as 'the page made no requests', " +
			"which is a lie the caller cannot detect")
	}
	if hasURL(netRequests(t, res), "style.css") {
		t.Error("a request made before anything was listening was reported anyway")
	}
}

// --follow delivers a request once it COMPLETES — once, not once per CDP event —
// and returns cleanly (not as an error) when the window closes.
func TestNetStreamDeliversCompletedRequestsOnce(t *testing.T) {
	b := liveChrome(t)
	_, page := netFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	id := netLiveTab(ctx, t, b, page)
	got := make(chan map[string]any, 64)
	streamCtx, streamCancel := context.WithTimeout(ctx, 60*time.Second)
	defer streamCancel()
	done := make(chan error, 1)
	go func() {
		done <- b.NetStream(streamCtx, id, NetOpts{URL: "/api/"}, func(v any) error {
			reqs := v.(map[string]any)["requests"].([]any)
			select {
			case got <- reqs[0].(map[string]any):
			default:
			}
			return nil
		})
	}()

	// Establish that the subscription is LIVE before making the request under
	// test, by clicking a decoy endpoint until one comes back. Sleeping for a
	// fixed interval and hoping is the same race as reading a buffer straight
	// after an action — it just fails one layer up, as a stream that delivered
	// nothing.
	live := false
	for deadline := time.Now().Add(20 * time.Second); !live && time.Now().Before(deadline); {
		if _, err := b.Pointer(ctx, id, "#boom", PointerOpts{Action: PointerClick}); err != nil {
			t.Fatalf("Click: %v", err)
		}
		live = nextStreamed(got, "/api/boom", time.Second) != nil
	}
	if !live {
		t.Fatal("the stream never delivered anything, so it was not listening")
	}

	// Now the request under test happens exactly once, with the stream known to
	// be running — so "delivered once" is a claim about the stream and not about
	// how many clicks landed.
	if _, err := b.Pointer(ctx, id, "#ok", PointerOpts{Action: PointerClick}); err != nil {
		t.Fatalf("Click: %v", err)
	}
	r := nextStreamed(got, "/api/ok", 15*time.Second)
	if r == nil {
		t.Fatal("the stream delivered nothing for a request made while it was running")
	}
	if s, _ := r["status"].(int64); s != 200 {
		t.Errorf("streamed status = %v, want 200 — a follow reports outcomes, so the record must be complete", r["status"])
	}
	if r["pending"] != false {
		t.Errorf("streamed a pending record: %v", r)
	}
	// The same request must not be announced again as its remaining events land.
	if dup := nextStreamed(got, "/api/ok", 2*time.Second); dup != nil {
		t.Errorf("the same request was streamed twice: %v", dup)
	}

	streamCancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("NetStream = %v; the window closing is how a follow ends, not a failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("NetStream did not return after its context was cancelled")
	}
}
