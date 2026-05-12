// Package store is the SQLite-backed persistence layer for shows, seats,
// reservations and tickets. All times are stored as unix seconds for
// portability with command-line tooling (sqlite3, jq) and decoded back to
// time.Time at the Go boundary.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

const schema = `
CREATE TABLE IF NOT EXISTS shows (
    id          INTEGER PRIMARY KEY,
    title       TEXT NOT NULL,
    venue       TEXT NOT NULL,
    starts_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS seats (
    id              INTEGER PRIMARY KEY,
    show_id         INTEGER NOT NULL REFERENCES shows(id),
    row             INTEGER NOT NULL,
    col             INTEGER NOT NULL,
    price_kopecks   INTEGER NOT NULL,
    UNIQUE(show_id, row, col)
);

CREATE TABLE IF NOT EXISTS reservations (
    id              INTEGER PRIMARY KEY,
    seat_id         INTEGER NOT NULL REFERENCES seats(id),
    tg_user_id      INTEGER NOT NULL,
    tg_chat_id      INTEGER NOT NULL,
    code            TEXT NOT NULL UNIQUE,
    created_at      INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL,
    confirmed_at    INTEGER,
    cancelled_at    INTEGER,
    buyer_name      TEXT NOT NULL DEFAULT '',
    reminded_at     INTEGER
);
CREATE INDEX IF NOT EXISTS idx_res_code ON reservations(code);
CREATE INDEX IF NOT EXISTS idx_res_seat ON reservations(seat_id);
CREATE INDEX IF NOT EXISTS idx_res_user ON reservations(tg_user_id);

CREATE TABLE IF NOT EXISTS tickets (
    id              INTEGER PRIMARY KEY,
    reservation_id  INTEGER NOT NULL UNIQUE REFERENCES reservations(id),
    qr_payload      TEXT NOT NULL UNIQUE,
    issued_at       INTEGER NOT NULL,
    used_at         INTEGER
);
CREATE INDEX IF NOT EXISTS idx_tkt_qr ON tickets(qr_payload);
`

// Lightweight on-open migrations for columns we added after v1 shipped.
// SQLite quietly rejects duplicate ADD COLUMN, which is fine here.
var migrations = []string{
	`ALTER TABLE reservations ADD COLUMN cancelled_at INTEGER`,
	`ALTER TABLE reservations ADD COLUMN buyer_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE reservations ADD COLUMN reminded_at INTEGER`,
}

// Errors.
var (
	ErrSeatTaken      = errors.New("seat is already reserved or sold")
	ErrSeatNotFound   = errors.New("seat does not exist for this show")
	ErrCodeNotFound   = errors.New("reservation code not found")
	ErrAlreadyPaid    = errors.New("reservation already confirmed")
	ErrAlreadyClosed  = errors.New("reservation already closed")
	ErrNotYourBooking = errors.New("reservation belongs to another user")
	ErrTicketNotFound = errors.New("ticket not found")
	ErrTicketUsed     = errors.New("ticket already used")
)

// Store wraps *sql.DB with typed methods. All methods take a context.
type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	for _, m := range migrations {
		_, _ = db.Exec(m) // duplicate-column errors are expected and ignored
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SeedIfEmpty inserts a single show plus a rows×cols seat grid when the
// database is freshly initialised. Returns the show id either way.
func (s *Store) SeedIfEmpty(ctx context.Context, show Show, rows, cols int, priceKopecks int64) (int64, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM shows`).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		var id int64
		err := s.db.QueryRowContext(ctx, `SELECT id FROM shows ORDER BY id LIMIT 1`).Scan(&id)
		return id, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO shows(title, venue, starts_at) VALUES (?, ?, ?)`,
		show.Title, show.Venue, show.StartsAt.Unix())
	if err != nil {
		return 0, err
	}
	showID, _ := res.LastInsertId()

	for r := 1; r <= rows; r++ {
		for c := 1; c <= cols; c++ {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO seats(show_id, row, col, price_kopecks) VALUES (?, ?, ?, ?)`,
				showID, r, c, priceKopecks); err != nil {
				return 0, err
			}
		}
	}
	return showID, tx.Commit()
}

// LoadShow returns the full Show by id.
func (s *Store) LoadShow(ctx context.Context, id int64) (Show, error) {
	var sh Show
	var startsAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, venue, starts_at FROM shows WHERE id = ?`, id).
		Scan(&sh.ID, &sh.Title, &sh.Venue, &startsAt)
	if err != nil {
		return sh, err
	}
	sh.StartsAt = time.Unix(startsAt, 0)
	return sh, nil
}

func (s *Store) Seats(ctx context.Context, showID int64) ([]Seat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, show_id, row, col, price_kopecks FROM seats WHERE show_id = ? ORDER BY row, col`, showID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Seat
	for rows.Next() {
		var seat Seat
		if err := rows.Scan(&seat.ID, &seat.ShowID, &seat.Row, &seat.Col, &seat.PriceKopecks); err != nil {
			return nil, err
		}
		out = append(out, seat)
	}
	return out, rows.Err()
}

// SeatStatuses returns the current state of every seat in the show.
func (s *Store) SeatStatuses(ctx context.Context, showID int64) (map[int64]SeatStatus, error) {
	out := make(map[int64]SeatStatus)
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM seats WHERE show_id = ?`, showID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		out[id] = SeatFree
	}
	rows.Close()

	now := time.Now().Unix()
	r2, err := s.db.QueryContext(ctx, `
		SELECT seat_id, confirmed_at, expires_at, cancelled_at FROM reservations
		WHERE seat_id IN (SELECT id FROM seats WHERE show_id = ?)`, showID)
	if err != nil {
		return nil, err
	}
	defer r2.Close()
	for r2.Next() {
		var seatID, exp int64
		var conf, cancelled sql.NullInt64
		if err := r2.Scan(&seatID, &conf, &exp, &cancelled); err != nil {
			return nil, err
		}
		if cancelled.Valid {
			continue // cancelled reservation does not occupy the seat
		}
		switch {
		case conf.Valid:
			out[seatID] = SeatSold
		case exp > now && out[seatID] != SeatSold:
			out[seatID] = SeatHeld
		}
	}
	return out, nil
}

// FindFreeSeat resolves a row/col to a seat id and errors if it's not free.
func (s *Store) FindFreeSeat(ctx context.Context, showID int64, row, col int) (Seat, error) {
	var seat Seat
	err := s.db.QueryRowContext(ctx,
		`SELECT id, show_id, row, col, price_kopecks FROM seats WHERE show_id=? AND row=? AND col=?`,
		showID, row, col).Scan(&seat.ID, &seat.ShowID, &seat.Row, &seat.Col, &seat.PriceKopecks)
	if errors.Is(err, sql.ErrNoRows) {
		return seat, ErrSeatNotFound
	}
	if err != nil {
		return seat, err
	}

	now := time.Now().Unix()
	var taken int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reservations
		WHERE seat_id=? AND cancelled_at IS NULL
		  AND (confirmed_at IS NOT NULL OR expires_at > ?)`,
		seat.ID, now).Scan(&taken); err != nil {
		return seat, err
	}
	if taken > 0 {
		return seat, ErrSeatTaken
	}
	return seat, nil
}

// Reserve atomically claims a seat for the user.
func (s *Store) Reserve(
	ctx context.Context, seat Seat, tgUserID, tgChatID int64,
	buyerName, code string, hold time.Duration,
) (Reservation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Reservation{}, err
	}
	defer tx.Rollback()

	now := time.Now()
	var taken int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reservations
		WHERE seat_id=? AND cancelled_at IS NULL
		  AND (confirmed_at IS NOT NULL OR expires_at > ?)`,
		seat.ID, now.Unix()).Scan(&taken); err != nil {
		return Reservation{}, err
	}
	if taken > 0 {
		return Reservation{}, ErrSeatTaken
	}

	expires := now.Add(hold)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO reservations(seat_id, tg_user_id, tg_chat_id, buyer_name, code, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		seat.ID, tgUserID, tgChatID, buyerName, code, now.Unix(), expires.Unix())
	if err != nil {
		return Reservation{}, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return Reservation{}, err
	}
	return Reservation{
		ID: id, SeatID: seat.ID, TGUserID: tgUserID, TGChatID: tgChatID,
		BuyerName: buyerName, Code: code, CreatedAt: now, ExpiresAt: expires,
	}, nil
}

// FindReservationByCode finds an active reservation. Cancelled rows are
// invisible.
func (s *Store) FindReservationByCode(ctx context.Context, code string) (Reservation, Seat, error) {
	var r Reservation
	var seat Seat
	var conf sql.NullInt64
	var cancelled sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT r.id, r.seat_id, r.tg_user_id, r.tg_chat_id, r.buyer_name, r.code,
		       r.created_at, r.expires_at, r.confirmed_at, r.cancelled_at,
		       s.id, s.show_id, s.row, s.col, s.price_kopecks
		FROM reservations r JOIN seats s ON s.id = r.seat_id
		WHERE r.code = ?`, code).Scan(
		&r.ID, &r.SeatID, &r.TGUserID, &r.TGChatID, &r.BuyerName, &r.Code,
		scanTime(&r.CreatedAt), scanTime(&r.ExpiresAt), &conf, &cancelled,
		&seat.ID, &seat.ShowID, &seat.Row, &seat.Col, &seat.PriceKopecks)
	if errors.Is(err, sql.ErrNoRows) {
		return r, seat, ErrCodeNotFound
	}
	if err != nil {
		return r, seat, err
	}
	if cancelled.Valid {
		return r, seat, ErrAlreadyClosed
	}
	if conf.Valid {
		t := time.Unix(conf.Int64, 0)
		r.ConfirmedAt = &t
	}
	return r, seat, nil
}

// CancelReservation soft-deletes a reservation if it belongs to the user
// and is not yet confirmed.
func (s *Store) CancelReservation(ctx context.Context, code string, tgUserID int64) (Reservation, Seat, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Reservation{}, Seat{}, err
	}
	defer tx.Rollback()

	r, seat, err := s.FindReservationByCode(ctx, code)
	if err != nil {
		return r, seat, err
	}
	if r.TGUserID != tgUserID {
		return r, seat, ErrNotYourBooking
	}
	if r.ConfirmedAt != nil {
		return r, seat, ErrAlreadyPaid
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE reservations SET cancelled_at=? WHERE id=?`, time.Now().Unix(), r.ID); err != nil {
		return r, seat, err
	}
	return r, seat, tx.Commit()
}

// Confirm marks a reservation paid and issues its ticket.
func (s *Store) Confirm(ctx context.Context, reservationID int64, qrPayload string) (Ticket, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Ticket{}, err
	}
	defer tx.Rollback()

	var conf, cancelled sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT confirmed_at, cancelled_at FROM reservations WHERE id=?`, reservationID).
		Scan(&conf, &cancelled); err != nil {
		return Ticket{}, err
	}
	if cancelled.Valid {
		return Ticket{}, ErrAlreadyClosed
	}
	if conf.Valid {
		return Ticket{}, ErrAlreadyPaid
	}

	now := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE reservations SET confirmed_at=? WHERE id=?`,
		now.Unix(), reservationID); err != nil {
		return Ticket{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO tickets(reservation_id, qr_payload, issued_at) VALUES (?, ?, ?)`,
		reservationID, qrPayload, now.Unix())
	if err != nil {
		return Ticket{}, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return Ticket{}, err
	}
	return Ticket{ID: id, ReservationID: reservationID, QRPayload: qrPayload, IssuedAt: now}, nil
}

// UseTicket marks the ticket as used; returns ErrTicketUsed with UsedAt
// populated if it had already been scanned.
func (s *Store) UseTicket(ctx context.Context, qrPayload string) (Ticket, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Ticket{}, err
	}
	defer tx.Rollback()

	var t Ticket
	var used sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT id, reservation_id, qr_payload, issued_at, used_at FROM tickets WHERE qr_payload=?`,
		qrPayload).Scan(&t.ID, &t.ReservationID, &t.QRPayload, scanTime(&t.IssuedAt), &used)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrTicketNotFound
	}
	if err != nil {
		return t, err
	}
	if used.Valid {
		ut := time.Unix(used.Int64, 0)
		t.UsedAt = &ut
		return t, ErrTicketUsed
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE tickets SET used_at=? WHERE id=?`, now.Unix(), t.ID); err != nil {
		return t, err
	}
	if err := tx.Commit(); err != nil {
		return t, err
	}
	t.UsedAt = &now
	return t, nil
}

// FindReservationByTicket joins back from a ticket id to the seat/buyer info
// — used by the scanner UI to display "Олексій · ряд 3 місце 7".
func (s *Store) FindReservationByTicket(ctx context.Context, ticketID int64) (Reservation, Seat, error) {
	var r Reservation
	var seat Seat
	var conf, cancelled sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT r.id, r.seat_id, r.tg_user_id, r.tg_chat_id, r.buyer_name, r.code,
		       r.created_at, r.expires_at, r.confirmed_at, r.cancelled_at,
		       s.id, s.show_id, s.row, s.col, s.price_kopecks
		FROM tickets t
		JOIN reservations r ON r.id = t.reservation_id
		JOIN seats s ON s.id = r.seat_id
		WHERE t.id = ?`, ticketID).Scan(
		&r.ID, &r.SeatID, &r.TGUserID, &r.TGChatID, &r.BuyerName, &r.Code,
		scanTime(&r.CreatedAt), scanTime(&r.ExpiresAt), &conf, &cancelled,
		&seat.ID, &seat.ShowID, &seat.Row, &seat.Col, &seat.PriceKopecks)
	if err != nil {
		return r, seat, err
	}
	if conf.Valid {
		t := time.Unix(conf.Int64, 0)
		r.ConfirmedAt = &t
	}
	return r, seat, nil
}

// MyReservations returns the user's bookings, newest first.
func (s *Store) MyReservations(ctx context.Context, tgUserID int64) ([]MyItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.seat_id, r.tg_user_id, r.tg_chat_id, r.buyer_name, r.code,
		       r.created_at, r.expires_at, r.confirmed_at,
		       s.id, s.show_id, s.row, s.col, s.price_kopecks
		FROM reservations r
		JOIN seats s ON s.id = r.seat_id
		WHERE r.tg_user_id = ? AND r.cancelled_at IS NULL
		ORDER BY r.created_at DESC
		LIMIT 50`, tgUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MyItem
	for rows.Next() {
		var r Reservation
		var seat Seat
		var conf sql.NullInt64
		if err := rows.Scan(
			&r.ID, &r.SeatID, &r.TGUserID, &r.TGChatID, &r.BuyerName, &r.Code,
			scanTime(&r.CreatedAt), scanTime(&r.ExpiresAt), &conf,
			&seat.ID, &seat.ShowID, &seat.Row, &seat.Col, &seat.PriceKopecks,
		); err != nil {
			return nil, err
		}
		if conf.Valid {
			t := time.Unix(conf.Int64, 0)
			r.ConfirmedAt = &t
		}
		out = append(out, MyItem{Reservation: r, Seat: seat})
	}
	return out, rows.Err()
}

// Stats returns admin-facing counters for the show.
func (s *Store) Stats(ctx context.Context, showID int64) (Stats, error) {
	var st Stats
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM seats WHERE show_id = ?`, showID).Scan(&st.Total); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(SUM(CASE WHEN r.confirmed_at IS NOT NULL THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN r.confirmed_at IS NULL AND r.cancelled_at IS NULL
		                     AND r.expires_at > ? THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN r.confirmed_at IS NOT NULL THEN s.price_kopecks ELSE 0 END), 0)
		FROM reservations r JOIN seats s ON s.id = r.seat_id
		WHERE s.show_id = ?`,
		time.Now().Unix(), showID).Scan(&st.Sold, &st.Held, &st.RevenueKopecks); err != nil {
		return st, err
	}
	st.Free = st.Total - st.Sold - st.Held
	return st, nil
}

// ConfirmedNotYetReminded returns paid reservations that have not yet been
// pinged with the "show is in an hour" reminder. Mark them with
// MarkReminded after sending.
func (s *Store) ConfirmedNotYetReminded(ctx context.Context, showID int64) ([]MyItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.seat_id, r.tg_user_id, r.tg_chat_id, r.buyer_name, r.code,
		       r.created_at, r.expires_at, r.confirmed_at,
		       s.id, s.show_id, s.row, s.col, s.price_kopecks
		FROM reservations r
		JOIN seats s ON s.id = r.seat_id
		WHERE s.show_id = ?
		  AND r.confirmed_at IS NOT NULL
		  AND r.cancelled_at IS NULL
		  AND r.reminded_at IS NULL`, showID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MyItem
	for rows.Next() {
		var r Reservation
		var seat Seat
		var conf sql.NullInt64
		if err := rows.Scan(
			&r.ID, &r.SeatID, &r.TGUserID, &r.TGChatID, &r.BuyerName, &r.Code,
			scanTime(&r.CreatedAt), scanTime(&r.ExpiresAt), &conf,
			&seat.ID, &seat.ShowID, &seat.Row, &seat.Col, &seat.PriceKopecks,
		); err != nil {
			return nil, err
		}
		if conf.Valid {
			t := time.Unix(conf.Int64, 0)
			r.ConfirmedAt = &t
		}
		out = append(out, MyItem{Reservation: r, Seat: seat})
	}
	return out, rows.Err()
}

func (s *Store) MarkReminded(ctx context.Context, reservationID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE reservations SET reminded_at = ? WHERE id = ?`, time.Now().Unix(), reservationID)
	return err
}

// scanTime is an sql.Scanner that converts unix seconds → time.Time.
type timeScanner struct{ t *time.Time }

func (ts timeScanner) Scan(src any) error {
	if src == nil {
		return nil
	}
	switch v := src.(type) {
	case int64:
		*ts.t = time.Unix(v, 0)
	default:
		return fmt.Errorf("timeScanner: unexpected %T", src)
	}
	return nil
}

func scanTime(t *time.Time) any { return timeScanner{t: t} }
