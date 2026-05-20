package realtime

import (
	"sync"
	"testing"
	"time"
)

func TestSubscribeReceivesPublish(t *testing.T) {
	h := New()
	ch, unsub := h.Subscribe(7)
	defer unsub()

	h.Publish(7, Event{Type: "seat_status", SeatID: 42, Status: SeatHeld})

	select {
	case ev := <-ch:
		if ev.SeatID != 42 || ev.Status != SeatHeld {
			t.Fatalf("got %+v", ev)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("event not received")
	}
}

func TestPublishToDifferentShowIgnored(t *testing.T) {
	h := New()
	ch, unsub := h.Subscribe(7)
	defer unsub()

	h.Publish(99, Event{Type: "seat_status", SeatID: 1, Status: SeatHeld})

	select {
	case ev := <-ch:
		t.Fatalf("got unexpected event %+v", ev)
	case <-time.After(30 * time.Millisecond):
		// expected — no event for show 7
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	h := New()
	ch, unsub := h.Subscribe(7)
	unsub()

	h.Publish(7, Event{Type: "seat_status", SeatID: 1, Status: SeatHeld})

	// ch should be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("event delivered after unsubscribe")
		}
	case <-time.After(30 * time.Millisecond):
		t.Fatal("channel not closed after unsubscribe")
	}
}

func TestUnsubscribeIdempotent(t *testing.T) {
	h := New()
	_, unsub := h.Subscribe(7)
	unsub()
	unsub() // must not panic / double-close
}

func TestSlowSubscriberDoesNotBlockHub(t *testing.T) {
	h := New()
	// One slow subscriber, one fast.
	slow, unsubSlow := h.Subscribe(7)
	defer unsubSlow()
	fast, unsubFast := h.Subscribe(7)
	defer unsubFast()

	// Fill slow's buffer + one extra so the next Publish must drop.
	for i := range subBuffer + 5 {
		h.Publish(7, Event{Type: "seat_status", SeatID: int64(i), Status: SeatHeld})
	}

	// Fast subscriber should still see some events (the recent ones).
	drained := 0
	timeout := time.After(50 * time.Millisecond)
	for {
		select {
		case <-fast:
			drained++
		case <-timeout:
			if drained == 0 {
				t.Fatal("fast subscriber got nothing — hub blocked on slow client")
			}
			_ = slow // referenced
			return
		}
	}
}

func TestNilHubPublishIsSafe(t *testing.T) {
	var h *Hub
	h.Publish(1, Event{}) // must not panic
	if h.Subscribers() != 0 {
		t.Fatal("nil hub should report 0 subscribers")
	}
}

func TestConcurrentSubscribePublish(t *testing.T) {
	// Sanity check under race detector. No assertions on count — just
	// that nothing panics or deadlocks.
	h := New()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			ch, unsub := h.Subscribe(int64(seed % 3))
			defer unsub()
			go func() {
				for range ch {
				}
			}()
			for j := range 50 {
				h.Publish(int64(j%3), Event{Type: "seat_status", SeatID: int64(j), Status: SeatHeld})
			}
		}(i)
	}
	wg.Wait()
}
