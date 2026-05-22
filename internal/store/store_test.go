package store

import (
	"context"
	"errors"
	"fmt"
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
	if _, err := s.Reserve(ctx, seat, 1, 100, "Anna", "", "code0001", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err = s.Reserve(ctx, seat, 2, 200, "Bob", "", "code0002", 5*time.Minute)
	if !errors.Is(err, ErrSeatTaken) {
		t.Fatalf("got %v, want ErrSeatTaken", err)
	}
}

func TestCancelFreesSeat(t *testing.T) {
	s := newTestStore(t)
	showID := seedShow(t, s)
	ctx := context.Background()
	seat, _ := s.FindFreeSeat(ctx, showID, 1, 1)
	r, err := s.Reserve(ctx, seat, 1, 100, "A", "", "code0001", 5*time.Minute)
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
	r, _ := s.Reserve(ctx, seat, 1, 100, "A", "", "code0001", 5*time.Minute)
	if _, _, err := s.CancelReservation(ctx, r.Code, 999); !errors.Is(err, ErrNotYourBooking) {
		t.Fatalf("got %v, want ErrNotYourBooking", err)
	}
}

func TestConfirmTwiceFails(t *testing.T) {
	s := newTestStore(t)
	showID := seedShow(t, s)
	ctx := context.Background()
	seat, _ := s.FindFreeSeat(ctx, showID, 1, 1)
	r, _ := s.Reserve(ctx, seat, 1, 100, "A", "", "code0001", 5*time.Minute)
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
	r, _ := s.Reserve(ctx, seat, 1, 100, "A", "", "code0001", 5*time.Minute)
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

	r, _ := s.Reserve(ctx, seat, 1, 100, "A", "", "code0001", 5*time.Minute)
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
	r1, _ := s.Reserve(ctx, seat1, 1, 100, "A", "", "code0001", 5*time.Minute)
	if _, err := s.Confirm(ctx, r1.ID, "qr1"); err != nil {
		t.Fatal(err)
	}

	seat2, _ := s.FindFreeSeat(ctx, showID, 1, 2)
	if _, err := s.Reserve(ctx, seat2, 2, 200, "B", "", "code0002", 5*time.Minute); err != nil {
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
	if _, err := s.Reserve(ctx, seat1, 42, 100, "A", "", "code0001", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	seat2, _ := s.FindFreeSeat(ctx, showID, 1, 2)
	r2, _ := s.Reserve(ctx, seat2, 42, 100, "A", "", "code0002", 5*time.Minute)
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
	if _, err := s.Reserve(ctx, seat1, 1, 100, "A", "", "code0001", -1*time.Minute); err != nil {
		t.Fatal(err)
	}
	// Live hold → untouched.
	seat2, _ := s.FindFreeSeat(ctx, showID, 1, 2)
	if _, err := s.Reserve(ctx, seat2, 2, 200, "B", "", "code0002", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	// Expired but paid → untouched.
	seat3, _ := s.FindFreeSeat(ctx, showID, 1, 3)
	r3, _ := s.Reserve(ctx, seat3, 3, 300, "C", "", "code0003", -1*time.Minute)
	if _, err := s.Confirm(ctx, r3.ID, "qr3"); err != nil {
		t.Fatal(err)
	}

	freed, err := s.SweepExpiredHolds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(freed) != 1 {
		t.Fatalf("swept %d rows, want 1", len(freed))
	}
	// Re-running is a no-op.
	if freed, _ := s.SweepExpiredHolds(ctx); len(freed) != 0 {
		t.Fatalf("re-run swept %d rows, want 0", len(freed))
	}
	// Seat1 is now free again.
	if _, err := s.FindFreeSeat(ctx, showID, 1, 1); err != nil {
		t.Fatalf("seat1 should be free after sweep, got %v", err)
	}
}

func TestCreateShowAndListShows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t1 := time.Date(2026, 6, 1, 19, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 1, 19, 0, 0, 0, time.UTC)
	a, err := s.CreateShow(ctx, Show{Title: "A", Venue: "V1", StartsAt: t1}, 2, 2, 10000)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateShow(ctx, Show{Title: "B", Venue: "V2", StartsAt: t2}, 1, 1, 20000)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("CreateShow should mint distinct ids, got %d twice", a)
	}
	shows, err := s.ListShows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(shows) != 2 {
		t.Fatalf("ListShows = %d, want 2", len(shows))
	}
	// Newest start first.
	if shows[0].ID != b || shows[1].ID != a {
		t.Errorf("ListShows order = [%d, %d], want [%d, %d]", shows[0].ID, shows[1].ID, b, a)
	}
	// CreatedAt populated from server clock.
	if shows[0].CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want non-zero")
	}
}

func TestActiveShowPicksSoonestUpcoming(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	near := time.Now().Add(24 * time.Hour)
	far := time.Now().Add(7 * 24 * time.Hour)
	_, _ = s.CreateShow(ctx, Show{Title: "Far", StartsAt: far}, 1, 1, 100)
	near1, _ := s.CreateShow(ctx, Show{Title: "Near", StartsAt: near}, 1, 1, 100)

	active, err := s.ActiveShow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != near1 {
		t.Fatalf("ActiveShow = %d, want %d (soonest)", active.ID, near1)
	}
}

func TestActiveShowFallbackToRecentPast(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	past := time.Now().Add(-2 * time.Hour)
	id, _ := s.CreateShow(ctx, Show{Title: "Just ended", StartsAt: past}, 1, 1, 100)

	active, err := s.ActiveShow(ctx)
	if err != nil {
		t.Fatalf("expected fallback to recent past, got %v", err)
	}
	if active.ID != id {
		t.Fatalf("ActiveShow = %d, want %d", active.ID, id)
	}
}

func TestActiveShowNothingReturnsError(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.ActiveShow(context.Background()); !errors.Is(err, ErrNoActiveShow) {
		t.Fatalf("got %v, want ErrNoActiveShow", err)
	}
}

func TestArchiveShowRemovesFromActive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateShow(ctx, Show{Title: "X", StartsAt: time.Now().Add(24 * time.Hour)}, 1, 1, 100)
	if err := s.ArchiveShow(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActiveShow(ctx); !errors.Is(err, ErrNoActiveShow) {
		t.Fatalf("after archive: got %v, want ErrNoActiveShow", err)
	}
	// Double-archive is a no-op (no row affected).
	if err := s.ArchiveShow(ctx, id); !errors.Is(err, ErrShowNotFound) {
		t.Fatalf("re-archive: got %v, want ErrShowNotFound", err)
	}
}

func TestUpdateShowFields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateShow(ctx, Show{Title: "Old", Venue: "Old", StartsAt: time.Now()}, 1, 1, 100)
	newStart := time.Date(2027, 1, 1, 20, 0, 0, 0, time.UTC)
	if err := s.UpdateShow(ctx, Show{ID: id, Title: "New", Venue: "New venue", StartsAt: newStart}); err != nil {
		t.Fatal(err)
	}
	sh, _ := s.LoadShow(ctx, id)
	if sh.Title != "New" || sh.Venue != "New venue" || !sh.StartsAt.Equal(newStart) {
		t.Errorf("UpdateShow did not persist: %+v", sh)
	}
}

func TestAddSeatAndConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "A", StartsAt: time.Now()}, 2, 2, 100)
	// New row/col is fine.
	added, err := s.AddSeat(ctx, NewSeat{
		ShowID: showID, Row: 99, Col: 99, X: 500, Y: 500,
		Label: "VIP-A", Category: "vip", PriceKopecks: 99900, Sellable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if added.ID == 0 {
		t.Error("AddSeat returned zero id")
	}
	// Existing row/col collides on UNIQUE constraint.
	if _, err := s.AddSeat(ctx, NewSeat{ShowID: showID, Row: 1, Col: 1, PriceKopecks: 100, Sellable: true}); !errors.Is(err, ErrSeatExists) {
		t.Fatalf("collision: got %v, want ErrSeatExists", err)
	}
}

func TestUpdateSeatsBatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "A", StartsAt: time.Now()}, 1, 2, 10000)
	seats, _ := s.Seats(ctx, showID)
	if len(seats) != 2 {
		t.Fatalf("seed seats = %d, want 2", len(seats))
	}
	newX := 777.0
	newPrice := int64(50000)
	notSellable := false
	newLabel := "балкон-1"
	patches := []SeatPatch{
		{ID: seats[0].ID, X: &newX, PriceKopecks: &newPrice, Label: &newLabel},
		{ID: seats[1].ID, Sellable: &notSellable},
	}
	if err := s.UpdateSeats(ctx, patches); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Seats(ctx, showID)
	if got[0].X != newX || got[0].PriceKopecks != newPrice || got[0].Label != newLabel {
		t.Errorf("seat[0] not updated: %+v", got[0])
	}
	if got[1].Sellable {
		t.Errorf("seat[1].Sellable should be false")
	}
}

func TestReserveNonSellableRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "A", StartsAt: time.Now()}, 1, 1, 100)
	seats, _ := s.Seats(ctx, showID)
	notSellable := false
	if err := s.UpdateSeats(ctx, []SeatPatch{{ID: seats[0].ID, Sellable: &notSellable}}); err != nil {
		t.Fatal(err)
	}
	// FindFreeSeat refuses it.
	if _, err := s.FindFreeSeat(ctx, showID, 1, 1); !errors.Is(err, ErrSeatNotSellable) {
		t.Fatalf("FindFreeSeat on non-sellable: got %v, want ErrSeatNotSellable", err)
	}
	// And so does Reserve directly (caller bypasses FindFreeSeat).
	stale := seats[0]
	stale.Sellable = true // pretend caller has stale data
	if _, err := s.Reserve(ctx, stale, 1, 100, "X", "", "code0001", time.Minute); !errors.Is(err, ErrSeatNotSellable) {
		t.Fatalf("Reserve on non-sellable: got %v, want ErrSeatNotSellable", err)
	}
}

func TestRemoveSeatRequiresClean(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "A", StartsAt: time.Now()}, 1, 2, 100)
	seats, _ := s.Seats(ctx, showID)

	// Untouched seat — removable.
	if err := s.RemoveSeat(ctx, seats[0].ID); err != nil {
		t.Fatalf("RemoveSeat clean: %v", err)
	}
	// Seat with even a cancelled reservation — not removable (history matters).
	r, _ := s.Reserve(ctx, seats[1], 1, 100, "A", "", "code0001", time.Minute)
	if _, _, err := s.CancelReservation(ctx, r.Code, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveSeat(ctx, seats[1].ID); !errors.Is(err, ErrSeatHasReservations) {
		t.Fatalf("RemoveSeat with cancelled res: got %v, want ErrSeatHasReservations", err)
	}
}

func TestStatsExcludesNonSellableFromTotal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "A", StartsAt: time.Now()}, 1, 4, 100) // 4 seats
	seats, _ := s.Seats(ctx, showID)
	notSellable := false
	// Mark one as aisle.
	if err := s.UpdateSeats(ctx, []SeatPatch{{ID: seats[0].ID, Sellable: &notSellable}}); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats(ctx, showID)
	if st.Total != 3 {
		t.Errorf("Stats.Total = %d, want 3 (one seat is non-sellable)", st.Total)
	}
}

func TestUsersCreateAndLookup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "admin@example.com", "Admin", "hash:abc")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID == 0 || u.CreatedAt.IsZero() {
		t.Fatalf("CreateUser returned incomplete: %+v", u)
	}
	byEmail, err := s.FindUserByEmail(ctx, "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if byEmail.ID != u.ID {
		t.Errorf("FindUserByEmail.ID = %d, want %d", byEmail.ID, u.ID)
	}
	byID, err := s.FindUserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if byID.Email != "admin@example.com" {
		t.Errorf("FindUserByID.Email = %q, want admin@example.com", byID.Email)
	}
}

func TestUsersDuplicateEmailRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "a@b.com", "A", "h1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "a@b.com", "B", "h2"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate email: got %v, want ErrEmailTaken", err)
	}
}

func TestUsersNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.FindUserByEmail(ctx, "missing@x.com"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("FindUserByEmail missing: got %v, want ErrUserNotFound", err)
	}
	if _, err := s.FindUserByID(ctx, 999); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("FindUserByID missing: got %v, want ErrUserNotFound", err)
	}
}

func TestCountUsers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if n, _ := s.CountUsers(ctx); n != 0 {
		t.Errorf("CountUsers fresh = %d, want 0", n)
	}
	_, _ = s.CreateUser(ctx, "a@b.com", "A", "h")
	_, _ = s.CreateUser(ctx, "c@d.com", "C", "h")
	if n, _ := s.CountUsers(ctx); n != 2 {
		t.Errorf("CountUsers after 2 inserts = %d, want 2", n)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "admin@x.com", "Admin", "h")

	sess, err := s.CreateSession(ctx, u.ID, "tok-fresh", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ExpiresAt.Before(time.Now()) {
		t.Errorf("fresh session already expired: %v", sess.ExpiresAt)
	}

	got, gotUser, err := s.FindSession(ctx, "tok-fresh")
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != u.ID || gotUser.Email != "admin@x.com" {
		t.Errorf("FindSession got user %+v, want %s", gotUser, "admin@x.com")
	}

	if err := s.DeleteSession(ctx, "tok-fresh"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FindSession(ctx, "tok-fresh"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("after delete: got %v, want ErrSessionNotFound", err)
	}
	// Idempotent — second delete is fine.
	if err := s.DeleteSession(ctx, "tok-fresh"); err != nil {
		t.Errorf("re-delete: got %v, want nil", err)
	}
}

func TestSessionExpired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "admin@x.com", "Admin", "h")

	// TTL in the past → row stored, but FindSession reports expired.
	if _, err := s.CreateSession(ctx, u.ID, "tok-expired", -time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FindSession(ctx, "tok-expired"); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expired session: got %v, want ErrSessionExpired", err)
	}
}

func TestSweepExpiredSessions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "admin@x.com", "Admin", "h")
	_, _ = s.CreateSession(ctx, u.ID, "live", time.Hour)
	_, _ = s.CreateSession(ctx, u.ID, "dead-1", -time.Minute)
	_, _ = s.CreateSession(ctx, u.ID, "dead-2", -time.Hour)

	n, err := s.SweepExpiredSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("swept %d, want 2", n)
	}
	// Live session still resolvable.
	if _, _, err := s.FindSession(ctx, "live"); err != nil {
		t.Errorf("live session after sweep: %v", err)
	}
	// Dead sessions truly gone (not just expired).
	if _, _, err := s.FindSession(ctx, "dead-1"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("dead session after sweep: got %v, want ErrSessionNotFound", err)
	}
}

func TestCreateShowAutogeneratesSlug(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, err := s.CreateShow(ctx, Show{Title: "Hello World", StartsAt: time.Now()}, 1, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	sh, _ := s.LoadShow(ctx, id)
	if sh.Slug != "hello-world" {
		t.Errorf("Slug = %q, want hello-world", sh.Slug)
	}
}

func TestCreateShowSlugCollisionAppendsSuffix(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, _ := s.CreateShow(ctx, Show{Title: "Show", StartsAt: time.Now()}, 1, 1, 100)
	b, _ := s.CreateShow(ctx, Show{Title: "Show", StartsAt: time.Now()}, 1, 1, 100)
	shA, _ := s.LoadShow(ctx, a)
	shB, _ := s.LoadShow(ctx, b)
	if shA.Slug != "show" {
		t.Errorf("first slug = %q, want show", shA.Slug)
	}
	if shB.Slug != "show-2" {
		t.Errorf("second slug = %q, want show-2", shB.Slug)
	}
}

func TestCreateShowEmptySlugFromCyrillicTitle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Slugify drops non-ASCII → empty base → uniqueSlugTx falls back to "show".
	id, err := s.CreateShow(ctx, Show{Title: "Незвідана Зоря", StartsAt: time.Now()}, 1, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	sh, _ := s.LoadShow(ctx, id)
	if sh.Slug != "show" {
		t.Errorf("Slug = %q, want fallback show", sh.Slug)
	}
}

func TestLoadShowBySlug(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateShow(ctx, Show{Title: "My Event", StartsAt: time.Now()}, 1, 1, 100)
	sh, err := s.LoadShowBySlug(ctx, "my-event")
	if err != nil {
		t.Fatal(err)
	}
	if sh.ID != id {
		t.Errorf("ID = %d, want %d", sh.ID, id)
	}
}

func TestLoadShowBySlugHidesArchived(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateShow(ctx, Show{Title: "Old Event", StartsAt: time.Now()}, 1, 1, 100)
	if err := s.ArchiveShow(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadShowBySlug(ctx, "old-event"); !errors.Is(err, ErrShowNotFound) {
		t.Errorf("archived show via slug: got %v, want ErrShowNotFound", err)
	}
}

func TestReserveStoresBuyerEmail(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 1, 100)
	seats, _ := s.Seats(ctx, showID)
	r, err := s.Reserve(ctx, seats[0], 0, 0, "Web Buyer", "buyer@example.com", "codeweb01", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if r.BuyerEmail != "buyer@example.com" {
		t.Errorf("BuyerEmail = %q, want buyer@example.com", r.BuyerEmail)
	}
	// Round-trip via FindReservationByCode too.
	got, _, err := s.FindReservationByCode(ctx, r.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.BuyerEmail != "buyer@example.com" {
		t.Errorf("after roundtrip email = %q", got.BuyerEmail)
	}
}

func TestSlugify(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Hello World", "hello-world"},
		{"  multi   spaces  ", "multi-spaces"},
		{"Punct! Show?", "punct-show"},
		{"Tabs\tand\nnewlines", "tabs-and-newlines"},
		{"Незвідана Зоря", ""}, // Cyrillic stripped — caller adds fallback
		{"123 abc!", "123-abc"},
		{"ALL-CAPS", "all-caps"},
	}
	for _, tt := range tests {
		if got := Slugify(tt.in); got != tt.want {
			t.Errorf("Slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestListReservationsIncludesCancelled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 3, 100)
	seats, _ := s.Seats(ctx, showID)

	// 1 paid, 1 held, 1 cancelled.
	r1, _ := s.Reserve(ctx, seats[0], 1, 100, "A", "", "code0001", 5*time.Minute)
	if _, err := s.Confirm(ctx, r1.ID, "qr1"); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Reserve(ctx, seats[1], 2, 200, "B", "", "code0002", 5*time.Minute)
	r3, _ := s.Reserve(ctx, seats[2], 3, 300, "C", "", "code0003", 5*time.Minute)
	if _, _, err := s.CancelReservation(ctx, r3.Code, 3); err != nil {
		t.Fatal(err)
	}

	items, err := s.ListReservations(ctx, showID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("ListReservations = %d, want 3 (paid + held + cancelled)", len(items))
	}
	// Verify CancelledAt populated for the cancelled one.
	var cancelledSeen bool
	for _, it := range items {
		if it.Reservation.CancelledAt != nil {
			cancelledSeen = true
		}
	}
	if !cancelledSeen {
		t.Error("no reservation with CancelledAt set, want one")
	}
}

func TestAdminCancelReservationWorksOnPaid(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 1, 100)
	seats, _ := s.Seats(ctx, showID)
	r, _ := s.Reserve(ctx, seats[0], 1, 100, "A", "", "code0001", 5*time.Minute)
	if _, err := s.Confirm(ctx, r.ID, "qr1"); err != nil {
		t.Fatal(err)
	}

	got, _, _, err := s.AdminCancelReservation(ctx, r.ID)
	if err != nil {
		t.Fatalf("AdminCancelReservation on paid: %v", err)
	}
	if got.CancelledAt == nil {
		t.Error("CancelledAt should be set after admin cancel")
	}
	// Seat is now free.
	if _, err := s.FindFreeSeat(ctx, showID, 1, 1); err != nil {
		t.Errorf("seat should be free after admin cancel, got %v", err)
	}
}

func TestAdminCancelReservationDoubleFails(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 1, 100)
	seats, _ := s.Seats(ctx, showID)
	r, _ := s.Reserve(ctx, seats[0], 1, 100, "A", "", "code0001", 5*time.Minute)

	if _, _, _, err := s.AdminCancelReservation(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.AdminCancelReservation(ctx, r.ID); !errors.Is(err, ErrAlreadyClosed) {
		t.Errorf("re-cancel: got %v, want ErrAlreadyClosed", err)
	}
}

func TestAdminCancelReservationNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, _, _, err := s.AdminCancelReservation(context.Background(), 9999); !errors.Is(err, ErrCodeNotFound) {
		t.Errorf("got %v, want ErrCodeNotFound", err)
	}
}

func TestAdminCancelHeldMultiSeatCascades(t *testing.T) {
	// Held (unpaid) multi-seat order: cancelling one row dies the whole
	// basket. Buyer would otherwise be mid-payment with a moving total.
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 3, 100)
	seats, _ := s.Seats(ctx, showID)
	_, reservations, err := s.CreateOrder(ctx, seats, 0, 0, "Buyer", "b@x.com", nil, "casc1234", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	_, _, freed, err := s.AdminCancelReservation(ctx, reservations[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(freed) != 3 {
		t.Errorf("freed = %d seats, want 3 (cascade)", len(freed))
	}
	// All peers should be cancelled too.
	for _, r := range reservations[1:] {
		got, _, err := s.FindReservationByCode(ctx, r.Code)
		if !errors.Is(err, ErrAlreadyClosed) {
			t.Errorf("peer %q: status %+v err %v, want cancelled", r.Code, got, err)
		}
	}
}

func TestAdminCancelConfirmedMultiSeatPerSeat(t *testing.T) {
	// Confirmed multi-seat order: cancel one, leave the rest valid.
	// "One of my five guests can't come" use case.
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 3, 100)
	seats, _ := s.Seats(ctx, showID)
	order, reservations, err := s.CreateOrder(ctx, seats, 0, 0, "Buyer", "b@x.com", nil, "conf1234", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	qrs := map[int64]string{}
	for _, r := range reservations {
		qrs[r.ID] = "qr-" + r.Code
	}
	if _, err := s.ConfirmOrder(ctx, order.ID, qrs); err != nil {
		t.Fatal(err)
	}

	_, _, freed, err := s.AdminCancelReservation(ctx, reservations[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(freed) != 1 {
		t.Errorf("freed = %d seats, want 1 (per-seat, no cascade)", len(freed))
	}

	// Cancelled row is cancelled.
	if _, _, err := s.FindReservationByCode(ctx, reservations[1].Code); !errors.Is(err, ErrAlreadyClosed) {
		t.Errorf("targeted row: err %v, want ErrAlreadyClosed", err)
	}
	// Peers stay confirmed and uncancelled.
	for _, idx := range []int{0, 2} {
		got, _, err := s.FindReservationByCode(ctx, reservations[idx].Code)
		if err != nil {
			t.Errorf("peer %d: err %v", idx, err)
			continue
		}
		if got.ConfirmedAt == nil {
			t.Errorf("peer %d should stay confirmed", idx)
		}
		if got.CancelledAt != nil {
			t.Errorf("peer %d should NOT be cancelled", idx)
		}
	}
}

func TestCreateOrderMultiSeat(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 3, 250_00)
	seats, _ := s.Seats(ctx, showID)

	order, reservations, err := s.CreateOrder(
		ctx, seats, 0, 0, "Buyer", "buyer@x.com", nil, "abcd1234", 5*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if order.ID == 0 || order.Code != "abcd1234" {
		t.Errorf("order = %+v", order)
	}
	if order.TotalKopecks != 250_00*3 {
		t.Errorf("total = %d, want 75000", order.TotalKopecks)
	}
	if len(reservations) != 3 {
		t.Fatalf("reservations = %d, want 3", len(reservations))
	}
	for i, r := range reservations {
		want := fmt.Sprintf("abcd1234.%d", i+1)
		if r.Code != want {
			t.Errorf("reservation[%d].Code = %q, want %q", i, r.Code, want)
		}
	}
}

func TestCreateOrderAttendeeNames(t *testing.T) {
	// Each row should land the matching attendee_name; empty strings
	// inside the slice store as "" so the render-time fallback applies.
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 3, 100)
	seats, _ := s.Seats(ctx, showID)

	attendees := []string{"Анна", "", "Богдан"}
	_, reservations, err := s.CreateOrder(
		ctx, seats, 0, 0, "Buyer", "b@x.com", attendees, "att12345", time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Анна", "", "Богдан"}
	for i, r := range reservations {
		if r.AttendeeName != want[i] {
			t.Errorf("reservations[%d].AttendeeName = %q, want %q",
				i, r.AttendeeName, want[i])
		}
	}

	// Round-trip: FindOrderByCode should re-hydrate AttendeeName from disk.
	_, items, err := s.FindOrderByCode(ctx, "att12345")
	if err != nil {
		t.Fatal(err)
	}
	for i, it := range items {
		if it.Reservation.AttendeeName != want[i] {
			t.Errorf("items[%d].Reservation.AttendeeName = %q, want %q",
				i, it.Reservation.AttendeeName, want[i])
		}
	}
}

func TestCreateOrderAttendeeLenMismatch(t *testing.T) {
	// Wrong-length slice is a programmer error — refuse loudly so a
	// silently truncated/padded list never reaches the DB.
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 2, 100)
	seats, _ := s.Seats(ctx, showID)

	_, _, err := s.CreateOrder(
		ctx, seats, 0, 0, "Buyer", "b@x.com",
		[]string{"only one"}, "len12345", time.Minute,
	)
	if err == nil {
		t.Fatal("expected error for len mismatch")
	}
}

func TestCreateOrderSingleSeatKeepsBareCode(t *testing.T) {
	// When the order has a single reservation, no ".1" suffix — keeps
	// the legacy form so old display code stays correct.
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 1, 100)
	seats, _ := s.Seats(ctx, showID)

	_, reservations, err := s.CreateOrder(ctx, seats, 0, 0, "X", "x@y.com", nil, "solo1234", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reservations) != 1 || reservations[0].Code != "solo1234" {
		t.Errorf("single-seat code = %q, want %q", reservations[0].Code, "solo1234")
	}
}

func TestCreateOrderRefusesTakenSeat(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 2, 100)
	seats, _ := s.Seats(ctx, showID)
	// Pre-book seats[0] via single-seat Reserve, then try to include
	// it in a multi-seat order — should fail atomically (no order row
	// created).
	if _, err := s.Reserve(ctx, seats[0], 1, 100, "A", "", "first123", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.CreateOrder(ctx, seats, 0, 0, "B", "b@x.com", nil, "multi456", time.Minute)
	if !errors.Is(err, ErrSeatTaken) {
		t.Fatalf("got %v, want ErrSeatTaken", err)
	}
	// Make sure the order didn't sneak in.
	if _, _, err := s.FindOrderByCode(ctx, "multi456"); !errors.Is(err, ErrCodeNotFound) {
		t.Errorf("partial order row leaked: %v", err)
	}
}

func TestFindOrderByCodeAndConfirmOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 2, 100)
	seats, _ := s.Seats(ctx, showID)

	_, reservations, err := s.CreateOrder(ctx, seats, 0, 0, "Buyer", "b@x.com", nil, "find1234", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	order, items, err := s.FindOrderByCode(ctx, "find1234")
	if err != nil {
		t.Fatal(err)
	}
	if order.Code != "find1234" || len(items) != 2 {
		t.Errorf("find: order=%+v items=%d", order, len(items))
	}

	// Confirm: QR payload per reservation id; ticket per reservation.
	qrs := map[int64]string{
		reservations[0].ID: "qr-1",
		reservations[1].ID: "qr-2",
	}
	tickets, err := s.ConfirmOrder(ctx, order.ID, qrs)
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 2 {
		t.Errorf("tickets = %d, want 2", len(tickets))
	}
	// Re-confirm should fail.
	if _, err := s.ConfirmOrder(ctx, order.ID, qrs); !errors.Is(err, ErrAlreadyPaid) {
		t.Errorf("re-confirm: got %v, want ErrAlreadyPaid", err)
	}
}

func TestLegacyReservationMigratedToOrder(t *testing.T) {
	// Simulate a legacy database: a single-seat reservation created via
	// the old Reserve method. After Open's migrations run, an orders
	// row must exist for it with the same code, and reservation must
	// be linked.
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 1, 250_00)
	seats, _ := s.Seats(ctx, showID)
	_, err := s.Reserve(ctx, seats[0], 0, 0, "Legacy", "", "legacy12", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Pre-existing rows already migrated by Open's idempotent backfill,
	// but the Reserve we just did doesn't go through it — verify the
	// new-flow path (FindOrderByCode) still works for it. We backfill
	// manually here, mirroring what Open() does on next start.
	if _, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO orders (code, buyer_name, buyer_email, tg_user_id, tg_chat_id, total_kopecks, created_at, expires_at, confirmed_at, cancelled_at, reminded_at)
		SELECT r.code, r.buyer_name, r.buyer_email, r.tg_user_id, r.tg_chat_id,
		       (SELECT price_kopecks FROM seats WHERE id = r.seat_id),
		       r.created_at, r.expires_at, r.confirmed_at, r.cancelled_at, r.reminded_at
		FROM reservations r WHERE r.order_id IS NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE reservations SET order_id = (SELECT id FROM orders WHERE code = reservations.code) WHERE order_id IS NULL`); err != nil {
		t.Fatal(err)
	}

	order, items, err := s.FindOrderByCode(ctx, "legacy12")
	if err != nil {
		t.Fatalf("find legacy order: %v", err)
	}
	if order.TotalKopecks != 250_00 {
		t.Errorf("legacy total = %d, want 25000", order.TotalKopecks)
	}
	if len(items) != 1 {
		t.Errorf("legacy items = %d, want 1", len(items))
	}
}

func TestLinkReservationToTGChat(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 1, 100)
	seats, _ := s.Seats(ctx, showID)
	// Simulate a web-buyer reservation (TGChatID=0, BuyerEmail set).
	r, err := s.Reserve(ctx, seats[0], 0, 0, "Web", "web@example.com", "codelink1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Link to a TG chat.
	linked, _, err := s.LinkOrderToTGChat(ctx, r.Code, 12345, 67890)
	if err != nil {
		t.Fatal(err)
	}
	if linked.TGUserID != 12345 || linked.TGChatID != 67890 {
		t.Errorf("link did not persist: %+v", linked)
	}
	// Same user re-link is fine (chat id may have changed).
	relinked, _, err := s.LinkOrderToTGChat(ctx, r.Code, 12345, 88)
	if err != nil {
		t.Fatal(err)
	}
	if relinked.TGChatID != 88 {
		t.Errorf("same-user relink TGChatID = %d, want 88", relinked.TGChatID)
	}
	// Different user re-link is REFUSED — prevents code-leak takeover.
	if _, _, err := s.LinkOrderToTGChat(ctx, r.Code, 99, 100); !errors.Is(err, ErrNotYourBooking) {
		t.Errorf("different-user relink: got %v, want ErrNotYourBooking", err)
	}
}

func TestLinkReservationCancelledFails(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	showID, _ := s.CreateShow(ctx, Show{Title: "T", StartsAt: time.Now()}, 1, 1, 100)
	seats, _ := s.Seats(ctx, showID)
	r, _ := s.Reserve(ctx, seats[0], 0, 0, "Web", "web@x.com", "codelink2", time.Minute)
	// Force-cancel as admin.
	if _, _, _, err := s.AdminCancelReservation(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LinkOrderToTGChat(ctx, r.Code, 1, 2); !errors.Is(err, ErrAlreadyClosed) {
		t.Errorf("link cancelled: got %v, want ErrAlreadyClosed", err)
	}
}

func TestLinkReservationNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.LinkOrderToTGChat(context.Background(), "nosuchcd", 1, 2); !errors.Is(err, ErrCodeNotFound) {
		t.Errorf("got %v, want ErrCodeNotFound", err)
	}
}

func TestRemindFlow(t *testing.T) {
	s := newTestStore(t)
	showID := seedShow(t, s)
	ctx := context.Background()
	seat, _ := s.FindFreeSeat(ctx, showID, 1, 1)
	r, _ := s.Reserve(ctx, seat, 1, 100, "A", "", "code0001", 5*time.Minute)
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
