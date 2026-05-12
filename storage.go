package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
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
    confirmed_at    INTEGER
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

var (
	ErrSeatTaken      = errors.New("seat is already reserved or sold")
	ErrSeatNotFound   = errors.New("seat does not exist for this show")
	ErrCodeNotFound   = errors.New("reservation code not found")
	ErrAlreadyPaid    = errors.New("reservation already confirmed")
	ErrTicketNotFound = errors.New("ticket not found")
	ErrTicketUsed     = errors.New("ticket already used")
)

type Show struct {
	ID       int64
	Title    string
	Venue    string
	StartsAt time.Time
}

type Seat struct {
	ID            int64
	ShowID        int64
	Row, Col      int
	PriceKopecks  int64
}

type Reservation struct {
	ID          int64
	SeatID      int64
	TGUserID    int64
	TGChatID    int64
	Code        string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	ConfirmedAt *time.Time
}

type Ticket struct {
	ID            int64
	ReservationID int64
	QRPayload     string
	IssuedAt      time.Time
	UsedAt        *time.Time
}

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SeedIfEmpty inserts a show + grid of seats when the DB has no shows.
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

func (s *Store) Seats(ctx context.Context, showID int64) ([]Seat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, show_id, row, col, price_kopecks FROM seats WHERE show_id = ? ORDER BY row, col`, showID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Seat
	for rows.Next() {
		var s Seat
		if err := rows.Scan(&s.ID, &s.ShowID, &s.Row, &s.Col, &s.PriceKopecks); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (s *Store) SeatStatus(ctx context.Context, showID int64) (map[int64]string, error) {
	// "free" | "held" | "sold"
	out := make(map[int64]string)
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM seats WHERE show_id = ?`, showID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		out[id] = "free"
	}
	rows.Close()

	now := time.Now().Unix()
	r2, err := s.db.QueryContext(ctx, `
		SELECT seat_id, confirmed_at, expires_at FROM reservations
		WHERE seat_id IN (SELECT id FROM seats WHERE show_id = ?)`, showID)
	if err != nil {
		return nil, err
	}
	defer r2.Close()
	for r2.Next() {
		var seatID, exp int64
		var conf sql.NullInt64
		if err := r2.Scan(&seatID, &conf, &exp); err != nil {
			return nil, err
		}
		switch {
		case conf.Valid:
			out[seatID] = "sold"
		case exp > now && out[seatID] != "sold":
			out[seatID] = "held"
		}
	}
	return out, nil
}

// FindFreeSeat resolves "row, col" → seat_id ensuring it is free.
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

	// any active reservation/sold?
	now := time.Now().Unix()
	var taken int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reservations
		WHERE seat_id=? AND (confirmed_at IS NOT NULL OR expires_at > ?)`,
		seat.ID, now).Scan(&taken); err != nil {
		return seat, err
	}
	if taken > 0 {
		return seat, ErrSeatTaken
	}
	return seat, nil
}

// Reserve atomically holds a seat for the user with a unique code.
func (s *Store) Reserve(ctx context.Context, seat Seat, tgUserID, tgChatID int64, code string, hold time.Duration) (Reservation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Reservation{}, err
	}
	defer tx.Rollback()

	now := time.Now()
	var taken int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reservations
		WHERE seat_id=? AND (confirmed_at IS NOT NULL OR expires_at > ?)`,
		seat.ID, now.Unix()).Scan(&taken); err != nil {
		return Reservation{}, err
	}
	if taken > 0 {
		return Reservation{}, ErrSeatTaken
	}

	expires := now.Add(hold)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO reservations(seat_id, tg_user_id, tg_chat_id, code, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		seat.ID, tgUserID, tgChatID, code, now.Unix(), expires.Unix())
	if err != nil {
		return Reservation{}, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return Reservation{}, err
	}
	return Reservation{
		ID: id, SeatID: seat.ID, TGUserID: tgUserID, TGChatID: tgChatID,
		Code: code, CreatedAt: now, ExpiresAt: expires,
	}, nil
}

func (s *Store) FindReservationByCode(ctx context.Context, code string) (Reservation, Seat, error) {
	var r Reservation
	var seat Seat
	var conf sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT r.id, r.seat_id, r.tg_user_id, r.tg_chat_id, r.code, r.created_at, r.expires_at, r.confirmed_at,
		       s.id, s.show_id, s.row, s.col, s.price_kopecks
		FROM reservations r JOIN seats s ON s.id = r.seat_id
		WHERE r.code = ?`, code).Scan(
		&r.ID, &r.SeatID, &r.TGUserID, &r.TGChatID, &r.Code, scanTime(&r.CreatedAt), scanTime(&r.ExpiresAt), &conf,
		&seat.ID, &seat.ShowID, &seat.Row, &seat.Col, &seat.PriceKopecks)
	if errors.Is(err, sql.ErrNoRows) {
		return r, seat, ErrCodeNotFound
	}
	if err != nil {
		return r, seat, err
	}
	if conf.Valid {
		t := time.Unix(conf.Int64, 0)
		r.ConfirmedAt = &t
	}
	return r, seat, nil
}

// Confirm marks the reservation as paid and creates the linked ticket.
func (s *Store) Confirm(ctx context.Context, reservationID int64, qrPayload string) (Ticket, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Ticket{}, err
	}
	defer tx.Rollback()

	var conf sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT confirmed_at FROM reservations WHERE id=?`, reservationID).Scan(&conf); err != nil {
		return Ticket{}, err
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

// findReservationByTicket returns the reservation + seat behind a ticket id;
// used to render a nicer scanner UI ("Ряд 3 · місце 4") after UseTicket.
func (s *Store) findReservationByTicket(ctx context.Context, ticketID int64) (Reservation, Seat, error) {
	var r Reservation
	var seat Seat
	var conf sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT r.id, r.seat_id, r.tg_user_id, r.tg_chat_id, r.code, r.created_at, r.expires_at, r.confirmed_at,
		       s.id, s.show_id, s.row, s.col, s.price_kopecks
		FROM tickets t
		JOIN reservations r ON r.id = t.reservation_id
		JOIN seats s ON s.id = r.seat_id
		WHERE t.id = ?`, ticketID).Scan(
		&r.ID, &r.SeatID, &r.TGUserID, &r.TGChatID, &r.Code, scanTime(&r.CreatedAt), scanTime(&r.ExpiresAt), &conf,
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

// scanTime is a Scan target that converts unix int seconds into time.Time.
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
