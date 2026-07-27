// Package eventbuf provides the bounded, concurrency-safe ring buffers that
// retain the CDP events a held connection receives, per target.
//
// It is deliberately PURE: no CDP types, no context, no I/O. It knows nothing
// about console lines or network records — it is generic over the entry type so
// one implementation, and one test suite, serves every event-backed verb
// (RFC-0002's `console` and RFC-0003's `net`).
//
// Callers own the entry shape and the filter predicate. This package owns the
// two things that make retention safe on a day-long session:
//
//   - the bounds — a ring of at most N entries per target, plus a total cap
//     across targets, so an idle daemon cannot grow without limit;
//   - the accounting — a `dropped` counter that survives into the result
//     envelope, so a caller can tell that it read too late instead of silently
//     seeing a truncated history.
//
// Two shapes of event are supported. Add appends an independent entry (a
// console line). Upsert folds a stream of correlated events into ONE entry by a
// caller-chosen key (a network request's start, response, and completion arrive
// as separate CDP events but are one record).
package eventbuf

import (
	"sync"
	"unicode/utf8"
)

// Query selects entries from a buffer. It is applied SERVER-SIDE — where the
// buffer lives, before the envelope is marshalled — so a chatty page cannot
// flood a caller's context.
type Query[T any] struct {
	// Keep reports whether an entry matches. A nil Keep keeps everything.
	// Compose level / regex / since / status predicates here: the buffer stays
	// free of any knowledge of what an entry means.
	Keep func(T) bool

	// Limit keeps only the most recent Limit matches (0 = every match).
	Limit int

	// Clear empties the buffer after the query is answered, so the next read is
	// scoped to whatever the caller does next.
	Clear bool
}

// Result is a query's answer plus the accounting a caller needs to trust it.
type Result[T any] struct {
	Entries   []T  // the matches, oldest first, at most Limit of them
	Count     int  // len(Entries)
	Buffered  int  // entries the buffer held when the query ran
	Dropped   int  // entries evicted by the bound since the buffer was last cleared
	Truncated bool // Limit cut the match list
}

// Buffer is a bounded ring of entries for ONE target, safe for concurrent use.
//
// The zero value is not usable; construct with New.
type Buffer[T any] struct {
	mu   sync.Mutex
	max  int
	ring []T      // len == max; the entry with sequence n lives at n%max
	keys []string // correlation key per slot ("" when the entry is unkeyed)

	// Live entries are the sequence numbers in [start, next). Absolute
	// sequences (rather than slice offsets) keep the correlation index valid
	// across wraparound: a key whose sequence has fallen below start is, by
	// construction, an entry that has already been evicted.
	start int64
	next  int64

	dropped int
	index   map[string]int64

	subs   map[int]func(T)
	nextID int
}

// New returns a buffer holding at most max entries. A max of 0 (or less) is
// legal and holds nothing: every Add is counted as immediately dropped, which
// is the honest reading of "retain zero entries" rather than a silent no-op.
func New[T any](max int) *Buffer[T] {
	if max < 0 {
		max = 0
	}
	return &Buffer[T]{
		max:   max,
		ring:  make([]T, max),
		keys:  make([]string, max),
		index: map[string]int64{},
		subs:  map[int]func(T){},
	}
}

func (b *Buffer[T]) slot(seq int64) int { return int(seq % int64(b.max)) }

// Add appends an independent entry, evicting the oldest when the ring is full.
func (b *Buffer[T]) Add(e T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pushLocked("", e)
}

// Upsert folds a correlated event into a single entry.
//
// If key still names a live entry, mutate is called with it (existed = true)
// and the result replaces it in place — no eviction, no dropped++, and the
// entry keeps its original position in the ring, so a long-running request does
// not jump to the front of the history. Otherwise mutate is called with the
// zero value (existed = false) and the result is appended.
//
// Out-of-order events are therefore tolerated by construction: whichever event
// arrives first creates the entry, and the rest merge into it.
func (b *Buffer[T]) Upsert(key string, mutate func(cur T, existed bool) T) T {
	b.mu.Lock()
	defer b.mu.Unlock()
	if key != "" && b.max > 0 {
		if seq, ok := b.index[key]; ok && seq >= b.start && seq < b.next {
			e := mutate(b.ring[b.slot(seq)], true)
			b.ring[b.slot(seq)] = e
			b.notifyLocked(e)
			return e
		}
	}
	var zero T
	e := mutate(zero, false)
	b.pushLocked(key, e)
	return e
}

// pushLocked appends an entry, evicting the oldest first when the ring is full.
// Subscribers are notified even when the entry is not retained: a live tail and
// the retained history are separate concerns.
func (b *Buffer[T]) pushLocked(key string, e T) {
	defer b.notifyLocked(e)
	if b.max == 0 {
		b.dropped++
		return
	}
	if b.next-b.start == int64(b.max) {
		b.evictOldestLocked()
	}
	s := b.slot(b.next)
	b.ring[s] = e
	b.keys[s] = key
	if key != "" {
		b.index[key] = b.next
	}
	b.next++
}

// evictOldestLocked drops the oldest live entry and counts it. It reports
// whether there was anything to drop.
func (b *Buffer[T]) evictOldestLocked() bool {
	if b.next == b.start {
		return false
	}
	s := b.slot(b.start)
	if k := b.keys[s]; k != "" {
		// Only delete the index entry if it still points at THIS slot; a newer
		// entry may have reused the key.
		if seq, ok := b.index[k]; ok && seq == b.start {
			delete(b.index, k)
		}
		b.keys[s] = ""
	}
	var zero T
	b.ring[s] = zero
	b.start++
	b.dropped++
	return true
}

// evictOldest drops the oldest live entry, for the Set's total-cap backstop.
func (b *Buffer[T]) evictOldest() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.evictOldestLocked()
}

// Len returns how many entries the buffer currently holds.
func (b *Buffer[T]) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int(b.next - b.start)
}

// Dropped returns how many entries the bound has evicted since the buffer was
// created or last cleared.
func (b *Buffer[T]) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Clear empties the buffer and resets the dropped counter.
//
// Resetting `dropped` is deliberate: an explicit clear starts a new observation
// window, and carrying an eviction count from before it would report "you read
// too late" about messages the caller threw away on purpose.
func (b *Buffer[T]) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clearLocked()
}

func (b *Buffer[T]) clearLocked() {
	var zero T
	for i := range b.ring {
		b.ring[i] = zero
		b.keys[i] = ""
	}
	b.index = map[string]int64{}
	b.start = b.next
	b.dropped = 0
}

// Query returns the entries matching q, oldest first, plus the accounting.
func (b *Buffer[T]) Query(q Query[T]) Result[T] {
	b.mu.Lock()
	defer b.mu.Unlock()
	res := Result[T]{Buffered: int(b.next - b.start), Dropped: b.dropped}
	match := make([]T, 0, res.Buffered)
	for seq := b.start; seq < b.next; seq++ {
		e := b.ring[b.slot(seq)]
		if q.Keep == nil || q.Keep(e) {
			match = append(match, e)
		}
	}
	if q.Limit > 0 && len(match) > q.Limit {
		match = match[len(match)-q.Limit:]
		res.Truncated = true
	}
	res.Entries = match
	res.Count = len(match)
	if q.Clear {
		b.clearLocked()
	}
	return res
}

// Subscribe registers fn to receive every entry added or updated from now on,
// and returns the function that unregisters it.
//
// fn runs while the buffer's lock is held, which is what keeps a subscriber's
// view in the same order the events arrived. It must therefore NEVER block and
// NEVER call back into this buffer or its Set — hand the entry to a buffered
// channel and do the work elsewhere.
func (b *Buffer[T]) Subscribe(fn func(T)) (stop func()) {
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = fn
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

func (b *Buffer[T]) notifyLocked(e T) {
	for _, fn := range b.subs {
		fn(e)
	}
}

// Set is the per-target collection of buffers, with a total cap across targets
// as the backstop.
//
// Per-target rings alone bound each tab, but a session that touches many tabs
// would still grow linearly in tabs; the total cap makes the worst case a
// constant. When it is exceeded, the LARGEST buffer gives up its oldest entry —
// so the chatty tab pays, not the quiet one that happened to be added last.
//
// The zero value is not usable; construct with NewSet.
type Set[T any] struct {
	mu        sync.Mutex
	perTarget int
	total     int
	bufs      map[string]*Buffer[T]
}

// NewSet returns a Set whose per-target rings hold perTarget entries, capped at
// total entries across all targets (total <= 0 disables the total cap).
func NewSet[T any](perTarget, total int) *Set[T] {
	return &Set[T]{perTarget: perTarget, total: total, bufs: map[string]*Buffer[T]{}}
}

// Buffer returns the buffer for a target, creating it on first use.
func (s *Set[T]) Buffer(targetID string) *Buffer[T] {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bufs[targetID]
	if !ok {
		b = New[T](s.perTarget)
		s.bufs[targetID] = b
	}
	return b
}

// Add appends an entry for a target and enforces the total cap.
func (s *Set[T]) Add(targetID string, e T) {
	s.Buffer(targetID).Add(e)
	s.enforceTotal()
}

// Upsert folds a correlated event into a target's entry for key (see
// Buffer.Upsert) and enforces the total cap.
func (s *Set[T]) Upsert(targetID, key string, mutate func(cur T, existed bool) T) T {
	e := s.Buffer(targetID).Upsert(key, mutate)
	s.enforceTotal()
	return e
}

// Query answers a query against one target's buffer.
func (s *Set[T]) Query(targetID string, q Query[T]) Result[T] {
	return s.Buffer(targetID).Query(q)
}

// Stat returns a target's live counts without materializing its entries — for
// the per-message accounting a streaming (`--follow`) read carries.
func (s *Set[T]) Stat(targetID string) (buffered, dropped int) {
	b := s.Buffer(targetID)
	return b.Len(), b.Dropped()
}

// Clear empties one target's buffer.
func (s *Set[T]) Clear(targetID string) { s.Buffer(targetID).Clear() }

// Subscribe registers fn for one target's entries (see Buffer.Subscribe).
func (s *Set[T]) Subscribe(targetID string, fn func(T)) (stop func()) {
	return s.Buffer(targetID).Subscribe(fn)
}

// Forget discards a target's buffer entirely — for a tab that has closed, whose
// entries can never be read again.
func (s *Set[T]) Forget(targetID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bufs, targetID)
}

// Targets lists the targets this set holds a buffer for.
func (s *Set[T]) Targets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.bufs))
	for id := range s.bufs {
		out = append(out, id)
	}
	return out
}

// Total returns how many entries are held across every target.
func (s *Set[T]) Total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.bufs {
		n += b.Len()
	}
	return n
}

// enforceTotal evicts from the largest buffer until the set is within its total
// cap. Ties break on the lowest target id, so eviction is deterministic and a
// test can assert on it.
func (s *Set[T]) enforceTotal() {
	if s.total <= 0 {
		return
	}
	for {
		s.mu.Lock()
		sum, biggest, bn := 0, "", -1
		for id, b := range s.bufs {
			n := b.Len()
			sum += n
			if n > bn || (n == bn && id < biggest) {
				biggest, bn = id, n
			}
		}
		victim := s.bufs[biggest]
		s.mu.Unlock()
		if sum <= s.total || victim == nil || bn <= 0 {
			return
		}
		if !victim.evictOldest() {
			return
		}
	}
}

// TruncateText caps s at max bytes, reporting whether it was cut.
//
// It is here rather than in a caller because every event-backed verb needs the
// same bound (console message text, a network body) and the same subtlety: the
// cut lands on a rune boundary, so a truncated entry is still valid UTF-8 and
// still marshals into the envelope. max <= 0 means no cap.
func TruncateText(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut, true
}
