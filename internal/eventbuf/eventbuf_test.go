package eventbuf

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// line is a synthetic console-shaped entry: enough structure to exercise level,
// regex and time filtering without any CDP types. The buffer never inspects it.
type line struct {
	Level string
	Text  string
	At    time.Time
}

// record is a synthetic network-shaped entry, for the correlation half of the
// API that RFC-0003 consumes: several events fold into one entry by request id.
type record struct {
	ID     string
	Method string
	Status int
	Done   bool
}

func levelKeep(levels ...string) func(line) bool {
	want := map[string]bool{}
	for _, l := range levels {
		want[l] = true
	}
	return func(l line) bool { return want[l.Level] }
}

func grepKeep(t *testing.T, expr string) func(line) bool {
	t.Helper()
	re, err := regexp.Compile(expr)
	if err != nil {
		t.Fatalf("regexp %q: %v", expr, err)
	}
	return func(l line) bool { return re.MatchString(l.Text) }
}

func texts(ls []line) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.Text
	}
	return out
}

// VS-3: level filtering keeps exactly the requested levels.
func TestQueryFiltersByLevel(t *testing.T) {
	t.Parallel()
	b := New[line](100)
	b.Add(line{Level: "log", Text: "chatter"})
	b.Add(line{Level: "warn", Text: "careful"})
	b.Add(line{Level: "error", Text: "boom"})

	res := b.Query(Query[line]{Keep: levelKeep("warn", "error")})
	if res.Count != 2 {
		t.Fatalf("count = %d, want 2 (%v)", res.Count, texts(res.Entries))
	}
	if res.Buffered != 3 {
		t.Errorf("buffered = %d, want 3 — the filter must not shrink the buffer", res.Buffered)
	}
	for _, e := range res.Entries {
		if e.Level == "log" {
			t.Errorf("a log entry survived a --level warn/error filter: %+v", e)
		}
	}
}

// VS-4: a regex filter is applied where the buffer lives, so `count` is small
// while `buffered` still reports everything retained.
func TestQueryFiltersByRegexServerSide(t *testing.T) {
	t.Parallel()
	b := New[line](1000)
	for i := range 50 {
		b.Add(line{Level: "log", Text: fmt.Sprintf("[Noise] tick %d", i)})
	}
	b.Add(line{Level: "log", Text: "[App] ready"})
	b.Add(line{Level: "error", Text: "[App] failed"})

	res := b.Query(Query[line]{Keep: grepKeep(t, `\[App\]`)})
	if res.Count != 2 {
		t.Errorf("count = %d, want 2 — the filter must run before the result is built", res.Count)
	}
	if res.Buffered != 52 {
		t.Errorf("buffered = %d, want 52 — buffered reports retention, not the filtered view", res.Buffered)
	}
	if got := texts(res.Entries); len(got) != 2 || got[0] != "[App] ready" || got[1] != "[App] failed" {
		t.Errorf("entries = %v, want the two [App] lines in order", got)
	}
}

// VS-6: the ring evicts the oldest entries, counts them, and keeps the newest.
func TestRingEvictsOldestAndReportsDropped(t *testing.T) {
	t.Parallel()
	b := New[line](10)
	for i := range 25 {
		b.Add(line{Level: "log", Text: fmt.Sprintf("m%02d", i)})
	}

	res := b.Query(Query[line]{Limit: 100})
	if res.Count != 10 {
		t.Fatalf("count = %d, want 10 (the ring size)", res.Count)
	}
	if res.Dropped != 15 {
		t.Errorf("dropped = %d, want 15 — eviction must be counted, or a caller cannot tell it read too late", res.Dropped)
	}
	if res.Truncated {
		t.Error("truncated = true, but --limit 100 did not cut a 10-entry result")
	}
	got := texts(res.Entries)
	want := []string{"m15", "m16", "m17", "m18", "m19", "m20", "m21", "m22", "m23", "m24"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("retained = %v, want the newest ten %v", got, want)
	}
}

func TestLimitKeepsTheMostRecentAndFlagsTruncation(t *testing.T) {
	t.Parallel()
	b := New[line](100)
	for i := range 10 {
		b.Add(line{Level: "log", Text: fmt.Sprintf("m%d", i)})
	}

	res := b.Query(Query[line]{Limit: 3})
	if res.Count != 3 || !res.Truncated {
		t.Fatalf("count/truncated = %d/%v, want 3/true", res.Count, res.Truncated)
	}
	if got := texts(res.Entries); strings.Join(got, ",") != "m7,m8,m9" {
		t.Errorf("entries = %v, want the three most recent", got)
	}
	if res.Buffered != 10 {
		t.Errorf("buffered = %d, want 10", res.Buffered)
	}
}

func TestSinceFiltersOnEntryTime(t *testing.T) {
	t.Parallel()
	now := time.Now()
	b := New[line](100)
	b.Add(line{Text: "old", At: now.Add(-5 * time.Minute)})
	b.Add(line{Text: "recent", At: now.Add(-10 * time.Second)})

	cutoff := now.Add(-30 * time.Second)
	res := b.Query(Query[line]{Keep: func(l line) bool { return l.At.After(cutoff) }})
	if res.Count != 1 || res.Entries[0].Text != "recent" {
		t.Errorf("entries = %v, want only the entry newer than the cutoff", texts(res.Entries))
	}
}

func TestClearEmptiesAndResetsDropped(t *testing.T) {
	t.Parallel()
	b := New[line](5)
	for i := range 12 { // 7 evictions
		b.Add(line{Text: fmt.Sprintf("m%d", i)})
	}
	if d := b.Dropped(); d != 7 {
		t.Fatalf("dropped before clear = %d, want 7", d)
	}

	// Reading with Clear returns the matches AND empties the buffer, so the
	// next read is scoped to what happens after this call.
	res := b.Query(Query[line]{Clear: true})
	if res.Count != 5 || res.Dropped != 7 {
		t.Errorf("the clearing read still reports its own window: count=%d dropped=%d, want 5/7", res.Count, res.Dropped)
	}
	after := b.Query(Query[line]{})
	if after.Count != 0 || after.Buffered != 0 {
		t.Errorf("buffer not empty after Clear: %+v", after)
	}
	if after.Dropped != 0 {
		t.Errorf("dropped = %d after Clear, want 0 — a fresh window must not inherit a stale 'you read too late' signal", after.Dropped)
	}

	b.Add(line{Text: "after"})
	if res := b.Query(Query[line]{}); res.Count != 1 || res.Entries[0].Text != "after" {
		t.Errorf("post-clear read = %v, want only the entry added after the clear", texts(res.Entries))
	}
}

func TestZeroCapacityRetainsNothingButCountsIt(t *testing.T) {
	t.Parallel()
	b := New[line](0)
	b.Add(line{Text: "x"})
	b.Add(line{Text: "y"})
	res := b.Query(Query[line]{})
	if res.Count != 0 || res.Buffered != 0 {
		t.Errorf("a zero-capacity buffer retained %d entries", res.Buffered)
	}
	if res.Dropped != 2 {
		t.Errorf("dropped = %d, want 2 — retaining nothing is still reported, not silent", res.Dropped)
	}
}

// Upsert is the half RFC-0003 needs: four CDP events, one record.
func TestUpsertCorrelatesEventsIntoOneEntry(t *testing.T) {
	t.Parallel()
	b := New[record](100)
	b.Upsert("req-1", func(r record, existed bool) record {
		if existed {
			t.Error("first event for req-1 must not report an existing entry")
		}
		r.ID, r.Method = "req-1", "POST"
		return r
	})
	b.Upsert("req-2", func(r record, _ bool) record { r.ID, r.Method = "req-2", "GET"; return r })
	b.Upsert("req-1", func(r record, existed bool) record {
		if !existed {
			t.Error("the response event for req-1 created a second entry instead of merging")
		}
		r.Status = 200
		return r
	})
	b.Upsert("req-1", func(r record, _ bool) record { r.Done = true; return r })

	res := b.Query(Query[record]{})
	if res.Count != 2 {
		t.Fatalf("count = %d, want 2 correlated records", res.Count)
	}
	got := res.Entries[0]
	if got.ID != "req-1" || got.Method != "POST" || got.Status != 200 || !got.Done {
		t.Errorf("req-1 = %+v, want the merged POST/200/done record", got)
	}
	if res.Entries[1].ID != "req-2" {
		t.Errorf("correlation must not reorder: entries = %+v", res.Entries)
	}
	if res.Dropped != 0 {
		t.Errorf("dropped = %d; merging into an existing entry is not an eviction", res.Dropped)
	}
}

// Events for one record can arrive in any order; whichever lands first creates
// the entry and the rest merge into it.
func TestUpsertToleratesOutOfOrderEvents(t *testing.T) {
	t.Parallel()
	b := New[record](100)
	b.Upsert("req-9", func(r record, _ bool) record { r.Status = 500; return r }) // response first
	b.Upsert("req-9", func(r record, existed bool) record {                       // start second
		if !existed {
			t.Error("the late start event created a duplicate record")
		}
		r.ID, r.Method = "req-9", "GET"
		return r
	})
	res := b.Query(Query[record]{})
	if res.Count != 1 || res.Entries[0].Status != 500 || res.Entries[0].Method != "GET" {
		t.Errorf("entries = %+v, want one record carrying both events", res.Entries)
	}
}

// An evicted key must not resurrect: a later event for it starts a new entry.
func TestUpsertKeyIsForgottenAfterEviction(t *testing.T) {
	t.Parallel()
	b := New[record](2)
	b.Upsert("a", func(r record, _ bool) record { r.ID = "a"; return r })
	b.Upsert("b", func(r record, _ bool) record { r.ID = "b"; return r })
	b.Upsert("c", func(r record, _ bool) record { r.ID = "c"; return r }) // evicts a
	b.Upsert("a", func(r record, existed bool) record {
		if existed {
			t.Error("an evicted key still resolved to a live entry")
		}
		r.ID = "a-again"
		return r
	})
	res := b.Query(Query[record]{})
	if res.Count != 2 {
		t.Fatalf("count = %d, want 2", res.Count)
	}
	if res.Entries[0].ID != "c" || res.Entries[1].ID != "a-again" {
		t.Errorf("entries = %+v, want [c, a-again]", res.Entries)
	}
}

func TestSetIsolatesTargets(t *testing.T) {
	t.Parallel()
	s := NewSet[line](10, 0)
	s.Add("tab-1", line{Text: "from one"})
	s.Add("tab-2", line{Text: "from two"})

	one := s.Query("tab-1", Query[line]{})
	if one.Count != 1 || one.Entries[0].Text != "from one" {
		t.Errorf("tab-1 = %v, want only its own entry", texts(one.Entries))
	}
	two := s.Query("tab-2", Query[line]{})
	if two.Count != 1 || two.Entries[0].Text != "from two" {
		t.Errorf("tab-2 = %v, want only its own entry", texts(two.Entries))
	}
	if got := s.Total(); got != 2 {
		t.Errorf("total = %d, want 2", got)
	}
}

// The total cap is the backstop that keeps a many-tab session from growing
// linearly in tabs; the chattiest buffer is the one that gives entries up.
func TestSetTotalCapEvictsFromTheLargestBuffer(t *testing.T) {
	t.Parallel()
	s := NewSet[line](100, 10)
	for i := range 9 {
		s.Add("chatty", line{Text: fmt.Sprintf("c%d", i)})
	}
	s.Add("quiet", line{Text: "q0"})
	if got := s.Total(); got != 10 {
		t.Fatalf("total = %d, want 10 (at the cap, nothing evicted yet)", got)
	}

	s.Add("chatty", line{Text: "c9"}) // pushes the set over the cap

	if got := s.Total(); got != 10 {
		t.Errorf("total = %d, want 10 — the cap must hold", got)
	}
	quiet := s.Query("quiet", Query[line]{})
	if quiet.Count != 1 || quiet.Dropped != 0 {
		t.Errorf("the quiet target lost an entry to the chatty one: %+v", quiet)
	}
	chatty := s.Query("chatty", Query[line]{})
	if chatty.Dropped != 1 || chatty.Entries[0].Text != "c1" {
		t.Errorf("chatty = count %d dropped %d first %q, want its oldest evicted", chatty.Count, chatty.Dropped, chatty.Entries[0].Text)
	}
}

func TestForgetDiscardsATargetsBuffer(t *testing.T) {
	t.Parallel()
	s := NewSet[line](10, 0)
	s.Add("gone", line{Text: "x"})
	s.Forget("gone")
	if got := s.Total(); got != 0 {
		t.Errorf("total = %d after Forget, want 0", got)
	}
	if ts := s.Targets(); len(ts) != 0 {
		t.Errorf("targets = %v after Forget, want none", ts)
	}
}

func TestSubscribeSeesEntriesInOrderAndStops(t *testing.T) {
	t.Parallel()
	b := New[line](10)
	got := make(chan string, 10)
	stop := b.Subscribe(func(l line) { got <- l.Text })

	b.Add(line{Text: "one"})
	b.Add(line{Text: "two"})
	stop()
	b.Add(line{Text: "three"})

	if a, bb := <-got, <-got; a != "one" || bb != "two" {
		t.Errorf("subscriber saw %q,%q, want one,two in order", a, bb)
	}
	select {
	case extra := <-got:
		t.Errorf("subscriber saw %q after stop()", extra)
	default:
	}
}

func TestStatReportsLiveCounts(t *testing.T) {
	t.Parallel()
	s := NewSet[line](3, 0)
	for i := range 5 {
		s.Add("t", line{Text: fmt.Sprintf("m%d", i)})
	}
	buffered, dropped := s.Stat("t")
	if buffered != 3 || dropped != 2 {
		t.Errorf("Stat = %d/%d, want 3 buffered / 2 dropped", buffered, dropped)
	}
}

func TestTruncateText(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in        string
		max       int
		want      string
		truncated bool
	}{
		"under the cap":      {"hello", 8, "hello", false},
		"exactly at the cap": {"hello", 5, "hello", false},
		"over the cap":       {"hello world", 5, "hello", true},
		"no cap":             {"hello world", 0, "hello world", false},
		"negative cap":       {"hello world", -1, "hello world", false},
		// The cut must not split a multi-byte rune, or the entry stops being
		// valid UTF-8 and the envelope it lands in stops being marshallable.
		"mid-rune cut": {"aé", 2, "a", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, cut := TruncateText(c.in, c.max)
			if got != c.want || cut != c.truncated {
				t.Errorf("TruncateText(%q, %d) = %q,%v; want %q,%v", c.in, c.max, got, cut, c.want, c.truncated)
			}
		})
	}
}

// The buffer is written from CDP event goroutines and read from the RPC
// dispatch, so concurrent Add/Upsert/Query must be safe (run under -race).
func TestConcurrentUseIsSafe(t *testing.T) {
	t.Parallel()
	s := NewSet[line](50, 200)
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				s.Add(fmt.Sprintf("tab-%d", w%2), line{Text: fmt.Sprintf("w%d-%d", w, i)})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 200 {
			s.Query("tab-0", Query[line]{Limit: 10})
			s.Stat("tab-1")
			s.Total()
		}
	}()
	wg.Wait()

	if got := s.Total(); got > 200 {
		t.Errorf("total = %d, want the cap (200) to have held under concurrency", got)
	}
}
