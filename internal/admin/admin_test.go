package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OlexiyOdarchuk/monokasa/internal/admin"
	"github.com/OlexiyOdarchuk/monokasa/internal/auth"
	"github.com/OlexiyOdarchuk/monokasa/internal/store"
)

// harness stands up the same wiring main.go uses (auth + admin), with a
// fresh on-disk SQLite per test. Returns the server URL and a cookie
// jar already containing a valid admin session — every helper below
// drives requests through it.
type harness struct {
	t      *testing.T
	st     *store.Store
	srv    *httptest.Server
	cookie *http.Cookie
}

func setup(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tix.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Seed one admin user and a session straight in the store so we don't
	// have to drive the login form for every test.
	hash, _ := auth.HashPassword("p@ss")
	u, err := st.CreateUser(context.Background(), "admin@x.com", "Admin", hash)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	tok, _ := auth.NewToken()
	if _, err := st.CreateSession(context.Background(), u.ID, tok, auth.SessionTTL); err != nil {
		t.Fatalf("create session: %v", err)
	}

	mux := http.NewServeMux()
	adminMux := http.NewServeMux()
	admin.NewHandler(st).Register(adminMux)

	authHandler := auth.NewHandler(st, false)
	mux.Handle("/api/admin/", authHandler.RequireAuth(adminMux))
	authHandler.Register(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &harness{
		t: t, st: st, srv: srv,
		cookie: &http.Cookie{Name: auth.SessionCookie, Value: tok},
	}
}

// do drives a request through the test server with auth cookie + JSON
// content-type already set.
func (h *harness) do(method, path string, body any) *http.Response {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(h.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func (h *harness) decodeJSON(resp *http.Response, dst any) {
	h.t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		h.t.Fatalf("decode: %v", err)
	}
}

// --- /me ---

func TestMeRequiresAuth(t *testing.T) {
	h := setup(t)
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/admin/me", nil)
	// No cookie + Accept: application/json → middleware should 401 instead
	// of redirecting to /admin/login (the redirect path is reserved for
	// browser navigation, distinguished by Accept: text/html).
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-cookie status = %d, want 401", resp.StatusCode)
	}
}

func TestMeReturnsCurrentUser(t *testing.T) {
	h := setup(t)
	resp := h.do(http.MethodGet, "/api/admin/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Email string `json:"email"`
		Name  string `json:"name"`
		ID    int64  `json:"id"`
	}
	h.decodeJSON(resp, &body)
	if body.Email != "admin@x.com" || body.ID == 0 {
		t.Errorf("body = %+v, want admin@x.com with non-zero id", body)
	}
}

// --- shows ---

func TestCreateAndListShows(t *testing.T) {
	h := setup(t)

	resp := h.do(http.MethodPost, "/api/admin/shows", map[string]any{
		"title":         "Test Event",
		"venue":         "Stage A",
		"starts_at":     time.Date(2026, 6, 1, 19, 0, 0, 0, time.UTC),
		"rows":          2,
		"cols":          3,
		"price_kopecks": 25000,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	}
	h.decodeJSON(resp, &created)
	if created.ID == 0 || created.Title != "Test Event" {
		t.Errorf("created = %+v", created)
	}

	resp2 := h.do(http.MethodGet, "/api/admin/shows", nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp2.StatusCode)
	}
	var shows []struct{ ID int64 }
	h.decodeJSON(resp2, &shows)
	if len(shows) != 1 || shows[0].ID != created.ID {
		t.Errorf("shows = %+v, want one with id %d", shows, created.ID)
	}
}

func TestCreateShowRejectsBadInput(t *testing.T) {
	h := setup(t)
	resp := h.do(http.MethodPost, "/api/admin/shows", map[string]any{
		"title": "", "rows": 0, "cols": 0, "price_kopecks": 0,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGetShowIncludesStats(t *testing.T) {
	h := setup(t)
	id := mustCreateShow(t, h, 1, 2)
	resp := h.do(http.MethodGet, fmt.Sprintf("/api/admin/shows/%d", id), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		ID    int64 `json:"id"`
		Stats *struct {
			Total int `json:"total"`
		} `json:"stats"`
	}
	h.decodeJSON(resp, &body)
	if body.Stats == nil || body.Stats.Total != 2 {
		t.Errorf("body.Stats = %+v, want total=2", body.Stats)
	}
}

func TestUpdateShowPatchesIndividualFields(t *testing.T) {
	h := setup(t)
	id := mustCreateShow(t, h, 1, 1)

	newTitle := "Renamed"
	resp := h.do(http.MethodPatch, fmt.Sprintf("/api/admin/shows/%d", id),
		map[string]any{"title": newTitle})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Title string `json:"title"`
		Venue string `json:"venue"`
	}
	h.decodeJSON(resp, &body)
	if body.Title != newTitle {
		t.Errorf("title = %q, want %q", body.Title, newTitle)
	}
	if body.Venue == "" {
		// venue wasn't in the patch, but the original "Venue 1x1" should remain
		t.Errorf("venue should not have been cleared by partial patch")
	}
}

func TestArchiveShow(t *testing.T) {
	h := setup(t)
	id := mustCreateShow(t, h, 1, 1)
	resp := h.do(http.MethodPost, fmt.Sprintf("/api/admin/shows/%d/archive", id), nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("archive: status = %d, want 204", resp.StatusCode)
	}
	// Second archive on the same show is now a 404 (ArchiveShow refuses
	// to re-archive — store returns ErrShowNotFound, handler maps to 404).
	resp2 := h.do(http.MethodPost, fmt.Sprintf("/api/admin/shows/%d/archive", id), nil)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("re-archive: status = %d, want 404", resp2.StatusCode)
	}
}

// --- seats ---

func TestSeatsListAddBatchRemove(t *testing.T) {
	h := setup(t)
	showID := mustCreateShow(t, h, 1, 2)

	// list
	resp := h.do(http.MethodGet, fmt.Sprintf("/api/admin/shows/%d/seats", showID), nil)
	var seats []struct {
		ID       int64
		Row      int
		Col      int
		Sellable bool
	}
	h.decodeJSON(resp, &seats)
	if len(seats) != 2 {
		t.Fatalf("seed seats = %d, want 2", len(seats))
	}

	// add
	resp2 := h.do(http.MethodPost, fmt.Sprintf("/api/admin/shows/%d/seats", showID), map[string]any{
		"row": 99, "col": 99, "x": 500, "y": 500,
		"price_kopecks": 50000, "sellable": true, "category": "vip",
	})
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("add seat status = %d, want 201", resp2.StatusCode)
	}
	var addedSeat struct {
		ID int64 `json:"id"`
	}
	h.decodeJSON(resp2, &addedSeat)

	// batch update — mark first seat non-sellable, change x
	newX := 123.0
	notSellable := false
	resp3 := h.do(http.MethodPatch, "/api/admin/seats", []map[string]any{
		{"id": seats[0].ID, "x": newX, "sellable": notSellable},
	})
	if resp3.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp3.Body)
		t.Fatalf("batch update status = %d, body=%s", resp3.StatusCode, body)
	}

	// confirm via list
	resp4 := h.do(http.MethodGet, fmt.Sprintf("/api/admin/shows/%d/seats", showID), nil)
	var after []struct {
		ID       int64
		X        float64
		Sellable bool
	}
	h.decodeJSON(resp4, &after)
	var foundPatched bool
	for _, s := range after {
		if s.ID == seats[0].ID {
			if s.X != newX || s.Sellable {
				t.Errorf("patched seat = %+v, want X=%v Sellable=false", s, newX)
			}
			foundPatched = true
		}
	}
	if !foundPatched {
		t.Errorf("patched seat not found in list")
	}

	// remove freshly-added seat (no reservations) — should 204
	resp5 := h.do(http.MethodDelete, fmt.Sprintf("/api/admin/seats/%d", addedSeat.ID), nil)
	if resp5.StatusCode != http.StatusNoContent {
		t.Errorf("remove status = %d, want 204", resp5.StatusCode)
	}
}

func TestAddSeatDuplicateConflict(t *testing.T) {
	h := setup(t)
	showID := mustCreateShow(t, h, 1, 1)
	// Try to add a seat at the same row/col as the seed.
	resp := h.do(http.MethodPost, fmt.Sprintf("/api/admin/shows/%d/seats", showID), map[string]any{
		"row": 1, "col": 1, "price_kopecks": 100, "sellable": true,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// --- guests / reservations ---

func TestListGuestsIncludesAllStates(t *testing.T) {
	h := setup(t)
	showID := mustCreateShow(t, h, 1, 3)
	seats := mustGetSeats(t, h, showID)

	// 1 paid, 1 held, 1 cancelled
	r1, _ := h.st.Reserve(context.Background(), seats[0], 1, 100, "Paid Buyer", "", "code0001", 5*time.Minute)
	_, _ = h.st.Confirm(context.Background(), r1.ID, "qr1")
	_, _ = h.st.Reserve(context.Background(), seats[1], 2, 200, "Held Buyer", "", "code0002", 5*time.Minute)
	r3, _ := h.st.Reserve(context.Background(), seats[2], 3, 300, "Cancelled Buyer", "", "code0003", 5*time.Minute)
	_, _, _ = h.st.CancelReservation(context.Background(), r3.Code, 3)

	resp := h.do(http.MethodGet, fmt.Sprintf("/api/admin/shows/%d/guests", showID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var guests []struct {
		Reservation struct {
			Status    string `json:"status"`
			BuyerName string `json:"buyer_name"`
		} `json:"reservation"`
	}
	h.decodeJSON(resp, &guests)
	if len(guests) != 3 {
		t.Fatalf("guests = %d, want 3", len(guests))
	}
	statuses := map[string]bool{}
	for _, g := range guests {
		statuses[g.Reservation.Status] = true
	}
	for _, want := range []string{"paid", "held", "cancelled"} {
		if !statuses[want] {
			t.Errorf("missing status %q in guests: %+v", want, statuses)
		}
	}
}

func TestCancelReservation(t *testing.T) {
	h := setup(t)
	showID := mustCreateShow(t, h, 1, 1)
	seats := mustGetSeats(t, h, showID)
	r, _ := h.st.Reserve(context.Background(), seats[0], 1, 100, "X", "", "code0001", 5*time.Minute)
	_, _ = h.st.Confirm(context.Background(), r.ID, "qr1")

	resp := h.do(http.MethodPost, fmt.Sprintf("/api/admin/reservations/%d/cancel", r.ID), nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}
	var body struct {
		Reservation struct {
			Status string `json:"status"`
		} `json:"reservation"`
	}
	h.decodeJSON(resp, &body)
	if body.Reservation.Status != "cancelled" {
		t.Errorf("status after cancel = %q, want cancelled", body.Reservation.Status)
	}

	// Double-cancel → 409.
	resp2 := h.do(http.MethodPost, fmt.Sprintf("/api/admin/reservations/%d/cancel", r.ID), nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("re-cancel status = %d, want 409", resp2.StatusCode)
	}
}

func TestExportGuestsCSV(t *testing.T) {
	h := setup(t)
	showID := mustCreateShow(t, h, 1, 1)
	seats := mustGetSeats(t, h, showID)
	r, _ := h.st.Reserve(context.Background(), seats[0], 1, 100, "Тест Імʼя", "", "code0001", 5*time.Minute)
	_, _ = h.st.Confirm(context.Background(), r.ID, "qr1")

	resp := h.do(http.MethodGet, fmt.Sprintf("/api/admin/shows/%d/guests.csv", showID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv*", got)
	}
	if !strings.HasPrefix(bodyStr, "\ufeff") {
		t.Errorf("CSV missing UTF-8 BOM")
	}
	if !strings.Contains(bodyStr, "code;buyer_name;row") {
		t.Errorf("CSV header missing or wrong separator: %q", bodyStr)
	}
	if !strings.Contains(bodyStr, "code0001") || !strings.Contains(bodyStr, "Тест Імʼя") {
		t.Errorf("CSV missing seeded row data:\n%s", bodyStr)
	}
}

// --- helpers ---

func mustCreateShow(t *testing.T, h *harness, rows, cols int) int64 {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/admin/shows", map[string]any{
		"title":         fmt.Sprintf("Venue %dx%d", rows, cols),
		"venue":         fmt.Sprintf("Venue %dx%d", rows, cols),
		"starts_at":     time.Now().Add(24 * time.Hour),
		"rows":          rows,
		"cols":          cols,
		"price_kopecks": 25000,
	})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create show: %d %s", resp.StatusCode, body)
	}
	var body struct {
		ID int64 `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	return body.ID
}

func mustGetSeats(t *testing.T, h *harness, showID int64) []store.Seat {
	t.Helper()
	seats, err := h.st.Seats(context.Background(), showID)
	if err != nil {
		t.Fatal(err)
	}
	return seats
}

func TestAuditLogRecordsAdminActions(t *testing.T) {
	h := setup(t)

	// Trigger one of each kind of mutation.
	resp := h.do(http.MethodPost, "/api/admin/shows", map[string]any{
		"title":         "Audit Show",
		"venue":         "X",
		"starts_at":     time.Date(2026, 6, 1, 19, 0, 0, 0, time.UTC),
		"rows":          1,
		"cols":          2,
		"price_kopecks": 10000,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	var created struct{ ID int64 }
	h.decodeJSON(resp, &created)

	resp = h.do(http.MethodPatch, fmt.Sprintf("/api/admin/shows/%d", created.ID),
		map[string]any{"venue": "Y"})
	resp.Body.Close()

	resp = h.do(http.MethodPost, fmt.Sprintf("/api/admin/shows/%d/archive", created.ID), nil)
	resp.Body.Close()

	// Now read the audit feed back.
	resp = h.do(http.MethodGet, "/api/admin/audit", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit list: %d", resp.StatusCode)
	}
	var entries []struct {
		ActorEmail string `json:"actor_email"`
		Action     string `json:"action"`
		Target     string `json:"target"`
	}
	h.decodeJSON(resp, &entries)
	want := []string{"show.archive", "show.update", "show.create"} // newest first
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}
	for i, w := range want {
		if entries[i].Action != w {
			t.Errorf("entries[%d].Action = %q, want %q", i, entries[i].Action, w)
		}
		if entries[i].ActorEmail != "admin@x.com" {
			t.Errorf("entries[%d].ActorEmail = %q", i, entries[i].ActorEmail)
		}
		if !strings.HasPrefix(entries[i].Target, "show:") {
			t.Errorf("entries[%d].Target = %q, want show:*", i, entries[i].Target)
		}
	}
}
