package encode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// NetEntry is one request as the `net` envelope reports it. The JSON tags ARE
// the envelope keys (RFC-0003, RFC-0017): the CLI fills it from the rows `Net`
// returns by a JSON round trip, which also normalises the int64/float64 split
// between the in-process and daemon paths.
type NetEntry struct {
	ID           string  `json:"id"`
	Method       string  `json:"method"`
	URL          string  `json:"url"`
	Type         string  `json:"type"`
	Status       *int64  `json:"status"` // nil: no status (pending or network-level failure)
	StatusText   string  `json:"status_text"`
	StartedAt    string  `json:"started_at"` // RFC 3339 UTC, ms; "" when the row carries none
	StartedMs    int64   `json:"started_ms"`
	DurationMs   *int64  `json:"duration_ms"` // nil: not finished
	RequestSize  int64   `json:"request_size"`
	ResponseSize int64   `json:"response_size"`
	FromCache    bool    `json:"from_cache"`
	Failed       bool    `json:"failed"`
	Error        *string `json:"error"`
	Pending      bool    `json:"pending"`

	RequestHeaders         map[string]string `json:"request_headers"`
	ResponseHeaders        map[string]string `json:"response_headers"`
	RequestBody            *string           `json:"request_body"`
	RequestBodyUnavailable bool              `json:"request_body_unavailable"`
	ResponseBody           *string           `json:"response_body"`
	BodyUnavailable        bool              `json:"body_unavailable"`
	BodyTruncated          bool              `json:"body_truncated"`
}

// DecodeNetEntries turns the `requests` value of a net result into typed rows.
// It is a JSON round trip (marshal, then unmarshal into []NetEntry) rather
// than a type switch, because that is what also normalises the int64/float64
// split between an in-process call and one that crossed the daemon's RPC.
func DecodeNetEntries(rows any) ([]NetEntry, error) {
	b, err := json.Marshal(rows)
	if err != nil {
		return nil, fmt.Errorf("net rows are not marshalable: %w", err)
	}
	var entries []NetEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("net rows are not a JSON array of request objects: %w", err)
	}
	return entries, nil
}

// HAROpts parameterises an export. Version is the CLI build version for
// log.creator; Now is the export instant, used only for a row with no start.
type HAROpts struct {
	Version string
	Now     time.Time
}

// HARTimeLayout is the RFC 3339 UTC, millisecond-precision layout both
// `started_at` (RFC-0017) and this encoder's fallback use, so a fallback
// timestamp is indistinguishable in shape from one filled from a real row.
const HARTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// harNVP is one name/value pair, the shape HAR uses for headers, cookies and
// query-string parameters.
type harNVP struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type harCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type harPostData struct {
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
}

type harContent struct {
	Size     int64   `json:"size"`
	MimeType string  `json:"mimeType"`
	Text     *string `json:"text,omitempty"`
}

type harRequest struct {
	Method      string       `json:"method"`
	URL         string       `json:"url"`
	HTTPVersion string       `json:"httpVersion"`
	Headers     []harNVP     `json:"headers"`
	QueryString []harNVP     `json:"queryString"`
	Cookies     []harNVP     `json:"cookies"`
	HeadersSize int64        `json:"headersSize"`
	BodySize    int64        `json:"bodySize"`
	PostData    *harPostData `json:"postData,omitempty"`
}

type harResponse struct {
	Status      int64      `json:"status"`
	StatusText  string     `json:"statusText"`
	HTTPVersion string     `json:"httpVersion"`
	Headers     []harNVP   `json:"headers"`
	Cookies     []harNVP   `json:"cookies"`
	Content     harContent `json:"content"`
	RedirectURL string     `json:"redirectURL"`
	HeadersSize int64      `json:"headersSize"`
	BodySize    int64      `json:"bodySize"`
}

// harCache is `{}` unless the request was served from cache, in which case it
// carries the one extension field this record can honestly report — Chrome's
// own "disk"/"memory" distinction is not something a retained record can tell.
type harCache struct {
	FromCache bool `json:"_fromCache,omitempty"`
}

// harTimings has only two knowns (start and end); everything this record
// cannot see is -1, and the whole duration is attributed to `wait` so `time`
// equals the sum of the non-(-1) timings, as the spec requires.
type harTimings struct {
	Blocked float64 `json:"blocked"`
	DNS     float64 `json:"dns"`
	Connect float64 `json:"connect"`
	SSL     float64 `json:"ssl"`
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
}

type harEntry struct {
	StartedDateTime string      `json:"startedDateTime"`
	Time            int64       `json:"time"`
	Request         harRequest  `json:"request"`
	Response        harResponse `json:"response"`
	Cache           harCache    `json:"cache"`
	Timings         harTimings  `json:"timings"`

	// Extensions (leading underscore), which the spec allows and viewers
	// ignore. Chrome's own names are reused where Chrome has one.
	ResourceType           string  `json:"_resourceType"`
	ID                     string  `json:"_id"`
	StartedMs              int64   `json:"_startedMs"`
	Pending                bool    `json:"_pending,omitempty"`
	Failed                 bool    `json:"_failed,omitempty"`
	Error                  *string `json:"_error,omitempty"`
	BodyUnavailable        bool    `json:"_bodyUnavailable,omitempty"`
	BodyTruncated          bool    `json:"_bodyTruncated,omitempty"`
	RequestBodyUnavailable bool    `json:"_requestBodyUnavailable,omitempty"`
	StartUnknown           bool    `json:"_startUnknown,omitempty"`
}

type harLog struct {
	Version string     `json:"version"`
	Creator harCreator `json:"creator"`
	Entries []harEntry `json:"entries"`
}

type harDocument struct {
	Log harLog `json:"log"`
}

// HAR renders the entries as an HTTP Archive 1.2 document: `json.Encoder` with
// two-space indent and HTML escaping OFF, so a redacted value ("<redacted>")
// survives byte-for-byte instead of becoming <redacted> — a reader
// grepping the file for the marker has to be able to find it.
func HAR(entries []NetEntry, opts HAROpts) ([]byte, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	built := make([]harEntry, 0, len(entries))
	for _, e := range entries {
		built = append(built, harBuildEntry(e, now))
	}
	// Ascending by startedDateTime, independent of buffer order, as the spec
	// recommends. The fixed-width UTC layout sorts lexicographically exactly
	// as it sorts chronologically, so a plain string sort is correct here.
	sort.SliceStable(built, func(i, j int) bool {
		return built[i].StartedDateTime < built[j].StartedDateTime
	})
	doc := harDocument{Log: harLog{
		Version: "1.2",
		Creator: harCreator{Name: "chrome-cdp", Version: opts.Version},
		Entries: built,
	}}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode HAR: %w", err)
	}
	return buf.Bytes(), nil
}

// harBuildEntry maps one NetEntry onto a HAR entry, per RFC-0017's mapping
// table and its worked per-state examples (ok / non-2xx / network failure /
// pending / cached).
func harBuildEntry(e NetEntry, now time.Time) harEntry {
	started, startUnknown := e.StartedAt, false
	if started == "" {
		started, startUnknown = now.UTC().Format(HARTimeLayout), true
	}

	var timeMs int64
	if e.DurationMs != nil {
		timeMs = *e.DurationMs
	}

	var status int64
	if e.Status != nil {
		status = *e.Status
	}

	// response.bodySize is -1 (unknown) for anything with no status: a
	// network-level failure or a request still pending never received bytes
	// to size.
	bodySize := int64(-1)
	if e.Status != nil {
		bodySize = e.ResponseSize
	}

	strippedURL := harStripFragment(e.URL)

	var postData *harPostData
	if e.RequestBody != nil {
		postData = &harPostData{
			MimeType: harHeaderValue(e.RequestHeaders, "content-type"),
			Text:     *e.RequestBody,
		}
	}

	return harEntry{
		StartedDateTime: started,
		Time:            timeMs,
		Request: harRequest{
			Method:      e.Method,
			URL:         strippedURL,
			HTTPVersion: "",
			Headers:     harHeaderList(e.RequestHeaders),
			QueryString: harQueryString(strippedURL),
			Cookies:     []harNVP{},
			HeadersSize: -1,
			BodySize:    e.RequestSize,
			PostData:    postData,
		},
		Response: harResponse{
			Status:      status,
			StatusText:  e.StatusText,
			HTTPVersion: "",
			Headers:     harHeaderList(e.ResponseHeaders),
			Cookies:     []harNVP{},
			Content: harContent{
				Size:     e.ResponseSize,
				MimeType: harHeaderValue(e.ResponseHeaders, "content-type"),
				Text:     e.ResponseBody,
			},
			RedirectURL: harHeaderValue(e.ResponseHeaders, "location"),
			HeadersSize: -1,
			BodySize:    bodySize,
		},
		Cache: harCache{FromCache: e.FromCache},
		Timings: harTimings{
			Blocked: -1, DNS: -1, Connect: -1, SSL: -1,
			Send: 0, Wait: float64(timeMs), Receive: 0,
		},
		ResourceType:           e.Type,
		ID:                     e.ID,
		StartedMs:              e.StartedMs,
		Pending:                e.Pending,
		Failed:                 e.Failed,
		Error:                  e.Error,
		BodyUnavailable:        e.BodyUnavailable,
		BodyTruncated:          e.BodyTruncated,
		RequestBodyUnavailable: e.RequestBodyUnavailable,
		StartUnknown:           startUnknown,
	}
}

// harStripFragment removes any #fragment from a URL, as the spec's `url`
// field requires and as `queryString` must already have removed before it is
// parsed.
func harStripFragment(u string) string {
	if i := strings.IndexByte(u, '#'); i >= 0 {
		return u[:i]
	}
	return u
}

// harHeaderValue does a case-insensitive header lookup, since HTTP header
// names are case-insensitive and the retained map preserves whatever case
// Chrome reported.
func harHeaderValue(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// harHeaderList renders a header map as HAR's [{name, value}] shape, sorted by
// name so the output is deterministic across runs.
func harHeaderList(h map[string]string) []harNVP {
	out := make([]harNVP, 0, len(h))
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		out = append(out, harNVP{Name: k, Value: h[k]})
	}
	return out
}

// harQueryString parses the query of a URL (which must already have its
// fragment stripped) in URL order: split on &, then on the first =, then
// url.QueryUnescape each half, keeping the raw text on an unescape error. A
// part with no = is a name with an empty value.
func harQueryString(rawURL string) []harNVP {
	_, query, hasQuery := strings.Cut(rawURL, "?")
	out := []harNVP{}
	if !hasQuery || query == "" {
		return out
	}
	for _, part := range strings.Split(query, "&") {
		name, value, hasEq := strings.Cut(part, "=")
		out = append(out, harNVP{Name: harUnescape(name), Value: harUnescapeIf(hasEq, value)})
	}
	return out
}

// harUnescape percent-decodes s, keeping the raw text on a malformed escape
// rather than dropping the parameter.
func harUnescape(s string) string {
	if u, err := url.QueryUnescape(s); err == nil {
		return u
	}
	return s
}

// harUnescapeIf unescapes value only when the part actually had an "=" —
// a bare flag parameter ("flag" with no "=") reports an empty value, not an
// unescaped empty string masquerading as one.
func harUnescapeIf(hasEq bool, value string) string {
	if !hasEq {
		return ""
	}
	return harUnescape(value)
}
