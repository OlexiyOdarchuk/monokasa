package public_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OlexiyOdarchuk/monokasa/internal/public"
	"github.com/OlexiyOdarchuk/monokasa/internal/store"
	"github.com/OlexiyOdarchuk/monokasa/internal/token"
)

type harness struct {
	t   *testing.T
	st  *store.Store
	srv *httptest.Server
}

func setup(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tix.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mux := http.NewServeMux()
	public.NewHandler(public.Config{
		Store:    st,
		Coder:    token.NewCoder("test-secret-with-enough-bytes-for-roundtrip"),
		JarLink:  "https://send.monobank.ua/jar/abc123",
		Hold:     15 * time.Minute,
		MinPrice: 100,
	}).Register(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{t: t, st: st, srv: srv}
}

func (h *harness) seedShow(t *testing.T, title string) (int64, []store.Seat) {
	t.Helper()
	id, err := h.st.CreateShow(context.Background(), store.Show{
		Title: title, Venue: "Hall", StartsAt: time.Now().Add(24 * time.Hour),
	}, 1, 2, 25000)
	if err != nil {
		t.Fatalf("create show: %v", err)
	}
	seats, _ := h.st.Seats(context.Background(), id)
	return id, seats
}

// --- GET /api/public/shows/{slug} ---

func TestGetShowBySlug(t *testing.T) {
	h := setup(t)
	_, seats := h.seedShow(t, "Public Event")

	resp, err := http.Get(h.srv.URL + "/api/public/shows/public-event")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Title string `json:"title"`
		Seats []struct {
			ID    int64
			Taken bool
		} `json:"seats"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Title != "Public Event" {
		t.Errorf("title = %q", body.Title)
	}
	if len(body.Seats) != len(seats) {
		t.Errorf("seats = %d, want %d", len(body.Seats), len(seats))
	}
	for _, s := range body.Seats {
		if s.Taken {
			t.Errorf("fresh seat %d marked taken", s.ID)
		}
	}
}

func TestGetShowUnknownSlugIs404(t *testing.T) {
	h := setup(t)
	resp, err := http.Get(h.srv.URL + "/api/public/shows/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// --- POST /api/public/reservations ---

func (h *harness) createReservation(t *testing.T, body any) *http.Response {
	t.Helper()
	buf, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/public/reservations", bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestCreateReservationHappyPath(t *testing.T) {
	h := setup(t)
	_, seats := h.seedShow(t, "Concert")

	resp := h.createReservation(t, map[string]any{
		"slug":        "concert",
		"seat_id":     seats[0].ID,
		"buyer_name":  "Web Buyer",
		"buyer_email": "buyer@example.com",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var body struct {
		Code   string `json:"code"`
		PayURL string `json:"pay_url"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if len(body.Code) != 8 {
		t.Errorf("code length = %d, want 8", len(body.Code))
	}
	if !strings.Contains(body.PayURL, "?a=250") || !strings.Contains(body.PayURL, "t="+body.Code) {
		t.Errorf("pay_url missing amount/code: %q", body.PayURL)
	}
}

func TestCreateReservationRejectsBadEmail(t *testing.T) {
	h := setup(t)
	_, seats := h.seedShow(t, "Concert")
	resp := h.createReservation(t, map[string]any{
		"slug":        "concert",
		"seat_id":     seats[0].ID,
		"buyer_name":  "Web Buyer",
		"buyer_email": "not-an-email",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad email: status = %d, want 400", resp.StatusCode)
	}
}

func TestCreateReservationRejectsShortName(t *testing.T) {
	h := setup(t)
	_, seats := h.seedShow(t, "Concert")
	resp := h.createReservation(t, map[string]any{
		"slug": "concert", "seat_id": seats[0].ID,
		"buyer_name": "X", "buyer_email": "buyer@example.com",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("short name: status = %d, want 400", resp.StatusCode)
	}
}

func TestCreateReservationSeatFromOtherShowIs404(t *testing.T) {
	h := setup(t)
	_, seatsA := h.seedShow(t, "Event A")
	_, _ = h.seedShow(t, "Event B")

	// Try to reserve event-A's seat under event-b's slug.
	resp := h.createReservation(t, map[string]any{
		"slug": "event-b", "seat_id": seatsA[0].ID,
		"buyer_name": "X Y", "buyer_email": "x@y.com",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-show: status = %d, want 404", resp.StatusCode)
	}
}

func TestCreateReservationTakenSeatIs409(t *testing.T) {
	h := setup(t)
	_, seats := h.seedShow(t, "Concert")

	// First reservation succeeds.
	resp1 := h.createReservation(t, map[string]any{
		"slug": "concert", "seat_id": seats[0].ID,
		"buyer_name": "First Buyer", "buyer_email": "first@example.com",
	})
	resp1.Body.Close()

	// Second on same seat fails.
	resp2 := h.createReservation(t, map[string]any{
		"slug": "concert", "seat_id": seats[0].ID,
		"buyer_name": "Second Buyer", "buyer_email": "second@example.com",
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("double-book: status = %d, want 409", resp2.StatusCode)
	}
}

func TestCreateReservationLowercasesEmail(t *testing.T) {
	h := setup(t)
	_, seats := h.seedShow(t, "Concert")
	resp := h.createReservation(t, map[string]any{
		"slug": "concert", "seat_id": seats[0].ID,
		"buyer_name": "Mixed Case", "buyer_email": "User@EXAMPLE.COM",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		BuyerEmail string `json:"buyer_email"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	// Full lowercase: case-sensitive local-parts technically exist per
	// RFC but no real provider honours them, and our /my login flow
	// needs case-insensitive lookups to work.
	if body.BuyerEmail != "user@example.com" {
		t.Errorf("email = %q, want user@example.com", body.BuyerEmail)
	}
}
