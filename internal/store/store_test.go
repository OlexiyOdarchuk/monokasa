package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tix.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedShow(t *testing.T, s *Store) int64 {
	t.Helper()
	id, err := s.SeedIfEmpty(context.Background(), Show{
		Title: "Test", Venue: "Home", StartsAt: time.Now().Add(24 * time.Hour),
	}, 2, 3, 25000)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return id
}

func TestSeedIfEmptyIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	show := Show{Title: "A", Venue: "B", StartsAt: time.Now()}
	a, err := s.SeedIfEmpty(ctx, show, 2, 3, 100)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.SeedIfEmpty(ctx, show, 5, 5, 999) // different params should be ignored
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("seed not idempotent: %d vs %d", a, b)
	}
	seats, err := s.Seats(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(seats) != 6 {
		t.Fatalf("got %d seats, want 6", len(seats))
	}
}

func TestReserveTwiceFails(t *testing.T) {
	s := newTestStore(t)
	showID := seedShow(t, s)
	ctx := context.Background()
	seat, err := s.FindFreeSeat(ctx, showID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, seat, 1, 100, "Anna", "code0001", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err = s.Reserve(ctx, seat, 2, 200, "Bob", "code0002", 5*time.Minute)
	if !errors.Is(err, ErrSeatTaken) {
		t.Fatalf("got %v, want ErrSeatTaken", err)
	}
}

func TestCancelFreesSeat(t *testing.T) {
	s := newTestStore(t)
	showID := seedShow(t, s)
	ctx := context.Background()
	seat, _ := s.FindFreeSeat(ctx, showID, 1, 1)
	r, err := s.Reserve(ctx, seat, 1, 100, "A", "code0001", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CancelReservation(ctx, r.Code, 1); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := s.FindFreeSeat(ctx, showID, 1, 1); err != nil {
		t.Fatalf("seat should be free after cancel, got %v", err)
	}
}

func TestCancelByOtherUserFails(t *testing.T) {
	s := newTestStore(t)
	showID := seedShow(t, s)
	ctx := context.Background()
	seat, _ := s.FindFreeSeat(ctx, showID, 1, 1)
	r, _ := s.Reserve(ctx, seat, 1, 100, "A", "code0001", 5*time.Minute)
	if _, _, err := s.CancelReservation(ctx, r.Code, 999); !errors.Is(err, ErrNotYourBooking) {
		t.Fatalf("got %v, want ErrNotYourBooking", err)
	}
}

func TestConfirmTwiceFails(t *testing.T) {
	s := newTestStore(t)
	showID := seedShow(t, s)
	ctx := context.Background()
	seat, _ := s.FindFreeSeat(ctx, showID, 1, 1)
	r, _ := s.Reserve(ctx, seat, 1, 100, "A", "code0001", 5*time.Minute)
	if _, err := s.Confirm(ctx, r.ID, "qr-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(ctx, r.ID, "qr-2"); !errors.Is(err, ErrAlreadyPaid) {
		t.Fatalf("got %v, want ErrAlreadyPaid", err)
	}
}

func TestUseTicketTwiceFails(t *testing.T) {
	s := newTestStore(t)
	showID := seedShow(t, s)
	ctx := context.Background()
	seat, _ := s.FindFreeSeat(ctx, showID, 1, 1)
	r, _ := s.Reserve(ctx, seat, 1, 100, "A", "code0001", 5*time.Minute)
	if _, err := s.Confirm(ctx, r.ID, "qr-xyz"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseTicket(ctx, "qr-xyz"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseTicket(ctx, "qr-xyz"); !errors.Is(err, ErrTicketUsed) {
		t.Fatalf("got %v, want ErrTicketUsed", err)
	}
}

func TestSeatStatusesTransition(t *testing.T) {
	s := newTestStore(t)
	showID := seedShow(t, s)
	ctx := context.Background()
	seat, _ := s.FindFreeSeat(ctx, showID, 1, 1)

	st, _ := s.SeatStatuses(ctx, showID)
	if st[seat.ID] != SeatFree {
		t.Fatalf("before reserve: got %s, want SeatFree", st[seat.ID])
	}

	r, _ := s.Reserve(ctx, seat, 1, 100, "A", "code0001", 5*time.Minute)
	st, _ = s.SeatStatuses(ctx, showID)
	if st[seat.ID] != SeatHeld {
		t.Fatalf("after reserve: got %s, want SeatHeld", st[seat.ID])
	}

	if _, err := s.Confirm(ctx, r.ID, "qr"); err != nil {
		t.Fatal(err)
	}
	st, _ = s.SeatStatuses(ctx, showID)
	if st[seat.ID] != SeatSold {
		t.Fatalf("after confirm: got %s, want SeatSold", st[seat.ID])
	}
}

func TestStats(t *testing.T) {
	s := newTestStore(t)
	showID := seedShow(t, s) // 2x3 = 6 seats @ 25000
	ctx := context.Background()

	seat1, _ := s.FindFreeSeat(ctx, showID, 1, 1)
	r1, _ := s.Reserve(ctx, seat1, 1, 100, "A", "code0001", 5*time.Minute)
	if _, err := s.Confirm(ctx, r1.ID, "qr1"); err != nil {
		t.Fatal(err)
	}

	seat2, _ := s.FindFreeSeat(ctx, showID, 1, 2)
	if _, err := s.Reserve(ctx, seat2, 2, 200, "B", "code0002", 5*time.Minute); err != nil {
		t.Fatal(err)
	}

	st, err := s.Stats(ctx, showID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 6 {
		t.Errorf("Total=%d want 6", st.Total)
	}
	if st.Sold != 1 {
		t.Errorf("Sold=%d want 1", st.Sold)
	}
	if st.Held != 1 {
		t.Errorf("Held=%d want 1", st.Held)
	}
	if st.Free != 4 {
		t.Errorf("Free=%d want 4", st.Free)
	}
	if st.RevenueKopecks != 25000 {
		t.Errorf("Revenue=%d want 25000", st.RevenueKopecks)
	}
}

func TestMyReservationsExcludesCancelled(t *testing.T) {
	s := newTestStore(t)
	showID := seedShow(t, s)
	ctx := context.Background()

	seat1, _ := s.FindFreeSeat(ctx, showID, 1, 1)
	if _, err := s.Reserve(ctx, seat1, 42, 100, "A", "code0001", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	seat2, _ := s.FindFreeSeat(ctx, showID, 1, 2)
	r2, _ := s.Reserve(ctx, seat2, 42, 100, "A", "code0002", 5*time.Minute)
	if _, _, err := s.CancelReservation(ctx, r2.Code, 42); err != nil {
		t.Fatal(err)
	}

	items, err := s.MyReservations(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1 (cancelled should be hidden)", len(items))
	}
}

func TestFindFreeSeatNotFound(t *testing.T) {
	s := newTestStore(t)
	showID := seedShow(t, s)
	ctx := context.Background()
	if _, err := s.FindFreeSeat(ctx, showID, 99, 99); !errors.Is(err, ErrSeatNotFound) {
		t.Fatalf("got %v, want ErrSeatNotFound", err)
	}
}

func TestSweepExpiredHolds(t *testing.T) {
	s := newTestStore(t)
	showID := seedShow(t, s)
	ctx := context.Background()

	// Expired and unpaid → swept.
	seat1, _ := s.FindFreeSeat(ctx, showID, 1, 1)
	if _, err := s.Reserve(ctx, seat1, 1, 100, "A", "code0001", -1*time.Minute); err != nil {
		t.Fatal(err)
	}
	// Live hold → untouched.
	seat2, _ := s.FindFreeSeat(ctx, showID, 1, 2)
	if _, err := s.Reserve(ctx, seat2, 2, 200, "B", "code0002", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	// Expired but paid → untouched.
	seat3, _ := s.FindFreeSeat(ctx, showID, 1, 3)
	r3, _ := s.Reserve(ctx, seat3, 3, 300, "C", "code0003", -1*time.Minute)
	if _, err := s.Confirm(ctx, r3.ID, "qr3"); err != nil {
		t.Fatal(err)
	}

	n, err := s.SweepExpiredHolds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("swept %d rows, want 1", n)
	}
	// Re-running is a no-op.
	if n, _ := s.SweepExpiredHolds(ctx); n != 0 {
		t.Fatalf("re-run swept %d rows, want 0", n)
	}
	// Seat1 is now free again.
	if _, err := s.FindFreeSeat(ctx, showID, 1, 1); err != nil {
		t.Fatalf("seat1 should be free after sweep, got %v", err)
	}
}

func TestRemindFlow(t *testing.T) {
	s := newTestStore(t)
	showID := seedShow(t, s)
	ctx := context.Background()
	seat, _ := s.FindFreeSeat(ctx, showID, 1, 1)
	r, _ := s.Reserve(ctx, seat, 1, 100, "A", "code0001", 5*time.Minute)
	if _, err := s.Confirm(ctx, r.ID, "qr"); err != nil {
		t.Fatal(err)
	}

	items, err := s.ConfirmedNotYetReminded(ctx, showID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d, want 1", len(items))
	}
	if err := s.MarkReminded(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	items, err = s.ConfirmedNotYetReminded(ctx, showID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("after MarkReminded got %d, want 0", len(items))
	}
}
