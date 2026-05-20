// Package realtime is the in-process pub/sub hub for "seat state
// changed" events. Producers (public reserve, pay confirm, admin
// cancel) call Publish; SSE handlers Subscribe per-show and stream
// events to connected buyers so the seat map updates without polling.
//
// The hub is keyed by show ID (not slug) so a slug rename doesn't
// orphan live subscribers. Drops events on slow subscribers rather
// than blocking the producer — losing a frame is fine, stalling the
// admin cancel handler is not.
package realtime

import (
	"sync"
)

// SeatStatus mirrors store.SeatStatus but lives here as a string so the
// realtime package keeps zero internal deps.
type SeatStatus string

const (
	SeatFree SeatStatus = "free"
	SeatHeld SeatStatus = "held"
	SeatSold SeatStatus = "sold"
)

// Event is the wire shape pushed to SSE subscribers. JSON-encoded in
// the handler; the struct is the single source of truth for field names.
type Event struct {
	Type   string     `json:"type"`    // always "seat_status" for now
	SeatID int64      `json:"seat_id"`
	Status SeatStatus `json:"status"`
}

// Hub is a goroutine-safe fan-out. Zero value is NOT usable — use New.
type Hub struct {
	mu   sync.RWMutex
	subs map[int64]map[chan Event]struct{} // showID → set of subscriber channels
}

// New returns a ready-to-use Hub with no subscribers.
func New() *Hub {
	return &Hub{subs: make(map[int64]map[chan Event]struct{})}
}

// subBuffer is how many events one subscriber can fall behind before we
// start dropping. 16 is enough for a quick burst (e.g. admin cancels
// the whole order = N events fired back-to-back) without ballooning
// memory if a client tab is frozen.
const subBuffer = 16

// Subscribe registers a new listener for the given show. Returns the
// receive channel and an unsubscribe func; the func is idempotent and
// MUST be called (defer is fine) to avoid leaking the subscription.
func (h *Hub) Subscribe(showID int64) (<-chan Event, func()) {
	ch := make(chan Event, subBuffer)
	h.mu.Lock()
	if h.subs[showID] == nil {
		h.subs[showID] = make(map[chan Event]struct{})
	}
	h.subs[showID][ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			// Close the channel INSIDE the write lock so no concurrent
			// Publish (holding the read lock) can still be looking at
			// it. Race detector caught the obvious "delete + Unlock +
			// close" variant — a Publisher mid-iterate would send into
			// the just-closed channel.
			h.mu.Lock()
			delete(h.subs[showID], ch)
			if len(h.subs[showID]) == 0 {
				delete(h.subs, showID)
			}
			close(ch)
			h.mu.Unlock()
		})
	}
	return ch, unsub
}

// Publish fans an event out to every subscriber of showID. Slow
// subscribers (full buffer) silently drop the event instead of blocking
// the producer — the SSE client will rebuild from the next REST refresh.
//
// Safe to call on a nil Hub (no-ops) so wiring code can pass nil during
// tests without sprinkling guards everywhere.
//
// Sends happen under the read lock — non-blocking (select + default)
// so it can't stall other Publishers. The lock guarantees no concurrent
// unsubscribe can close any of the channels we're about to write to.
func (h *Hub) Publish(showID int64, ev Event) {
	if h == nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs[showID] {
		select {
		case ch <- ev:
		default:
			// Buffer full — drop.
		}
	}
}

// Subscribers returns the number of live subscribers across all shows.
// Intended for /debug/vars-style introspection.
func (h *Hub) Subscribers() int {
	if h == nil {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, set := range h.subs {
		n += len(set)
	}
	return n
}
