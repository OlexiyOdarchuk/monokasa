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
    id           INTEGER PRIMARY KEY,
    title        TEXT NOT NULL,
    venue        TEXT NOT NULL,
    starts_at    INTEGER NOT NULL,
    created_at   INTEGER NOT NULL DEFAULT 0,
    archived_at  INTEGER
);

CREATE TABLE IF NOT EXISTS seats (
    id              INTEGER PRIMARY KEY,
    show_id         INTEGER NOT NULL REFERENCES shows(id),
    row             INTEGER NOT NULL,
    col             INTEGER NOT NULL,
    x               REAL NOT NULL DEFAULT 0,
    y               REAL NOT NULL DEFAULT 0,
    label           TEXT NOT NULL DEFAULT '',
    category        TEXT NOT NULL DEFAULT '',
    price_kopecks   INTEGER NOT NULL,
    sellable        INTEGER NOT NULL DEFAULT 1,
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
// SQLite quietly rejects duplicate ADD COLUMN, which is fine here — the
// CREATE above already includes these columns for fresh databases.
//
// The trailing UPDATE backfills x,y for legacy seats that pre-date the
// visual editor. It's idempotent: after the first run x or y is non-zero,
// so the WHERE clause skips them on subsequent boots.
var migrations = []string{
	`ALTER TABLE reservations ADD COLUMN cancelled_at INTEGER`,
	`ALTER TABLE reservations ADD COLUMN buyer_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE reservations ADD COLUMN reminded_at INTEGER`,
	`ALTER TABLE shows ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE shows ADD COLUMN archived_at INTEGER`,
	`ALTER TABLE seats ADD COLUMN x REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE seats ADD COLUMN y REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE seats ADD COLUMN label TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE seats ADD COLUMN category TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE seats ADD COLUMN sellable INTEGER NOT NULL DEFAULT 1`,
	`UPDATE seats SET x = (col - 1) * 100.0 + 50.0, y = (row - 1) * 100.0 + 50.0 WHERE x = 0 AND y = 0`,
}

// Errors.
var (
	ErrSeatTaken           = errors.New("seat is already reserved or sold")
	ErrSeatNotFound        = errors.New("seat does not exist for this show")
	ErrSeatNotSellable     = errors.New("seat is not sellable")
	ErrSeatExists          = errors.New("seat row/col already taken in this show")
	ErrCodeNotFound        = errors.New("reservation code not found")
	ErrAlreadyPaid         = errors.New("reservation already confirmed")
	ErrAlreadyClosed       = errors.New("reservation already closed")
	ErrNotYourBooking      = errors.New("reservation belongs to another user")
	ErrTicketNotFound      = errors.New("ticket not found")
	ErrTicketUsed          = errors.New("ticket already used")
	ErrShowNotFound        = errors.New("show not found")
	ErrNoActiveShow        = errors.New("no active show")
	ErrSeatHasReservations = errors.New("seat has reservations and cannot be removed")
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

// Ping verifies the database is reachable. Used by the /health endpoint
// so a stuck or evicted SQLite file fails readiness instead of silently
// serving stale data.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// --- shows ---

const showCols = `id, title, venue, starts_at, created_at, archived_at`

func scanShow(row interface{ Scan(...any) error }, sh *Show) error {
	var startsAt, createdAt int64
	var archivedAt sql.NullInt64
	if err := row.Scan(&sh.ID, &sh.Title, &sh.Venue, &startsAt, &createdAt, &archivedAt); err != nil {
		return err
	}
	sh.StartsAt = time.Unix(startsAt, 0)
	if createdAt > 0 {
		sh.CreatedAt = time.Unix(createdAt, 0)
	}
	if archivedAt.Valid {
		t := time.Unix(archivedAt.Int64, 0)
		sh.ArchivedAt = &t
	}
	return nil
}

// CreateShow inserts a new show with rows×cols seats at the given price.
// Seats are auto-laid-out on a default grid so the visual editor has
// something to start from.
func (s *Store) CreateShow(ctx context.Context, show Show, rows, cols int, priceKopecks int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	if show.CreatedAt.IsZero() {
		show.CreatedAt = time.Unix(now, 0)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO shows(title, venue, starts_at, created_at) VALUES (?, ?, ?, ?)`,
		show.Title, show.Venue, show.StartsAt.Unix(), show.CreatedAt.Unix())
	if err != nil {
		return 0, err
	}
	showID, _ := res.LastInsertId()

	for r := 1; r <= rows; r++ {
		for c := 1; c <= cols; c++ {
			x := float64(c-1)*100.0 + 50.0
			y := float64(r-1)*100.0 + 50.0
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO seats(show_id, row, col, x, y, price_kopecks, sellable)
				 VALUES (?, ?, ?, ?, ?, ?, 1)`,
				showID, r, c, x, y, priceKopecks); err != nil {
				return 0, err
			}
		}
	}
	return showID, tx.Commit()
}

// SeedIfEmpty creates a default show only when the database has none.
// Returns the id of the existing or freshly-seeded show.
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
	return s.CreateShow(ctx, show, rows, cols, priceKopecks)
}

// LoadShow returns the full Show by id.
func (s *Store) LoadShow(ctx context.Context, id int64) (Show, error) {
	var sh Show
	err := scanShow(
		s.db.QueryRowContext(ctx, `SELECT `+showCols+` FROM shows WHERE id = ?`, id),
		&sh)
	if errors.Is(err, sql.ErrNoRows) {
		return sh, ErrShowNotFound
	}
	return sh, err
}

// ListShows returns all shows, archived or not, newest start first.
func (s *Store) ListShows(ctx context.Context) ([]Show, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+showCols+` FROM shows ORDER BY starts_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Show
	for rows.Next() {
		var sh Show
		if err := scanShow(rows, &sh); err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// ActiveShow returns the soonest upcoming non-archived show. Falls back to
// the most recent non-archived show whose start time has already passed —
// useful right after a show ends, while the operator hasn't archived it
// yet. Returns ErrNoActiveShow when nothing fits.
func (s *Store) ActiveShow(ctx context.Context) (Show, error) {
	now := time.Now().Unix()
	var sh Show
	err := scanShow(s.db.QueryRowContext(ctx, `
		SELECT `+showCols+` FROM shows
		WHERE archived_at IS NULL AND starts_at >= ?
		ORDER BY starts_at ASC LIMIT 1`, now), &sh)
	if err == nil {
		return sh, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return sh, err
	}
	err = scanShow(s.db.QueryRowContext(ctx, `
		SELECT `+showCols+` FROM shows
		WHERE archived_at IS NULL
		ORDER BY starts_at DESC LIMIT 1`), &sh)
	if errors.Is(err, sql.ErrNoRows) {
		return sh, ErrNoActiveShow
	}
	return sh, err
}

// UpdateShow rewrites the editable fields of an existing show. ID, created_at
// and archived_at are not touched.
func (s *Store) UpdateShow(ctx context.Context, sh Show) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE shows SET title=?, venue=?, starts_at=? WHERE id=?`,
		sh.Title, sh.Venue, sh.StartsAt.Unix(), sh.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrShowNotFound
	}
	return nil
}

// ArchiveShow soft-deletes a show: it stops showing up as active but its
// data — including issued tickets — stays for the audit trail.
func (s *Store) ArchiveShow(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE shows SET archived_at=? WHERE id=? AND archived_at IS NULL`,
		time.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrShowNotFound
	}
	return nil
}

// --- seats ---

const seatCols = `id, show_id, row, col, x, y, label, category, price_kopecks, sellable`

func scanSeat(row interface{ Scan(...any) error }, s *Seat) error {
	var sellable int
	if err := row.Scan(
		&s.ID, &s.ShowID, &s.Row, &s.Col, &s.X, &s.Y,
		&s.Label, &s.Category, &s.PriceKopecks, &sellable,
	); err != nil {
		return err
	}
	s.Sellable = sellable != 0
	return nil
}

func (s *Store) Seats(ctx context.Context, showID int64) ([]Seat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+seatCols+` FROM seats WHERE show_id = ? ORDER BY row, col`, showID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Seat
	for rows.Next() {
		var seat Seat
		if err := scanSeat(rows, &seat); err != nil {
			return nil, err
		}
		out = append(out, seat)
	}
	return out, rows.Err()
}

// SeatStatuses returns the current state of every seat in the show.
// Non-sellable seats are still reported (as free) so the UI can render
// them — callers that distinguish should read Seat.Sellable separately.
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

// FindFreeSeat resolves a row/col to a seat id and errors if it's not free
// or not sellable.
func (s *Store) FindFreeSeat(ctx context.Context, showID int64, row, col int) (Seat, error) {
	var seat Seat
	err := scanSeat(s.db.QueryRowContext(ctx,
		`SELECT `+seatCols+` FROM seats WHERE show_id=? AND row=? AND col=?`,
		showID, row, col), &seat)
	if errors.Is(err, sql.ErrNoRows) {
		return seat, ErrSeatNotFound
	}
	if err != nil {
		return seat, err
	}
	if !seat.Sellable {
		return seat, ErrSeatNotSellable
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

// Reserve atomically claims a seat for the user. Refuses non-sellable seats.
func (s *Store) Reserve(
	ctx context.Context, seat Seat, tgUserID, tgChatID int64,
	buyerName, code string, hold time.Duration,
) (Reservation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Reservation{}, err
	}
	defer tx.Rollback()

	// Re-read sellable inside the tx so a caller that passed a stale Seat
	// (e.g., layout changed between FindFreeSeat and Reserve) can't bypass
	// the check.
	var sellable int
	if err := tx.QueryRowContext(ctx,
		`SELECT sellable FROM seats WHERE id=?`, seat.ID).Scan(&sellable); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Reservation{}, ErrSeatNotFound
		}
		return Reservation{}, err
	}
	if sellable == 0 {
		return Reservation{}, ErrSeatNotSellable
	}

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

// AddSeat inserts a new seat into an existing show.
func (s *Store) AddSeat(ctx context.Context, n NewSeat) (Seat, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO seats(show_id, row, col, x, y, label, category, price_kopecks, sellable)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ShowID, n.Row, n.Col, n.X, n.Y, n.Label, n.Category, n.PriceKopecks,
		boolToInt(n.Sellable))
	if err != nil {
		// UNIQUE(show_id, row, col) violation
		return Seat{}, fmt.Errorf("%w: %v", ErrSeatExists, err)
	}
	id, _ := res.LastInsertId()
	return Seat{
		ID: id, ShowID: n.ShowID, Row: n.Row, Col: n.Col, X: n.X, Y: n.Y,
		Label: n.Label, Category: n.Category, PriceKopecks: n.PriceKopecks,
		Sellable: n.Sellable,
	}, nil
}

// UpdateSeats applies a batch of partial updates from the layout editor.
// All-or-nothing: either every patch lands or none do.
func (s *Store) UpdateSeats(ctx context.Context, patches []SeatPatch) error {
	if len(patches) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, p := range patches {
		set, args := patchSQL(p)
		if set == "" {
			continue
		}
		args = append(args, p.ID)
		res, err := tx.ExecContext(ctx, `UPDATE seats SET `+set+` WHERE id = ?`, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrSeatNotFound
		}
	}
	return tx.Commit()
}

func patchSQL(p SeatPatch) (string, []any) {
	var cols []string
	var args []any
	if p.X != nil {
		cols = append(cols, "x = ?")
		args = append(args, *p.X)
	}
	if p.Y != nil {
		cols = append(cols, "y = ?")
		args = append(args, *p.Y)
	}
	if p.Label != nil {
		cols = append(cols, "label = ?")
		args = append(args, *p.Label)
	}
	if p.Category != nil {
		cols = append(cols, "category = ?")
		args = append(args, *p.Category)
	}
	if p.PriceKopecks != nil {
		cols = append(cols, "price_kopecks = ?")
		args = append(args, *p.PriceKopecks)
	}
	if p.Sellable != nil {
		cols = append(cols, "sellable = ?")
		args = append(args, boolToInt(*p.Sellable))
	}
	if len(cols) == 0 {
		return "", nil
	}
	return join(cols, ", "), args
}

// RemoveSeat deletes a seat that has never been reserved. Cancelled or
// confirmed reservations both block removal — preserve history over
// editor convenience.
func (s *Store) RemoveSeat(ctx context.Context, seatID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM reservations WHERE seat_id = ?`, seatID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrSeatHasReservations
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM seats WHERE id = ?`, seatID)
	if err != nil {
		return err
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return ErrSeatNotFound
	}
	return tx.Commit()
}

// --- reservations & tickets ---

const resCols = `r.id, r.seat_id, r.tg_user_id, r.tg_chat_id, r.buyer_name, r.code,
	r.created_at, r.expires_at, r.confirmed_at`

// scanReservationWithSeat scans the union of resCols + cancelled_at + seatCols
// (with the `s.` prefix). The order must match the SELECT below.
func scanReservationWithSeat(row interface{ Scan(...any) error }, r *Reservation, seat *Seat) error {
	var conf, cancelled sql.NullInt64
	var sellable int
	if err := row.Scan(
		&r.ID, &r.SeatID, &r.TGUserID, &r.TGChatID, &r.BuyerName, &r.Code,
		scanTime(&r.CreatedAt), scanTime(&r.ExpiresAt), &conf, &cancelled,
		&seat.ID, &seat.ShowID, &seat.Row, &seat.Col, &seat.X, &seat.Y,
		&seat.Label, &seat.Category, &seat.PriceKopecks, &sellable,
	); err != nil {
		return err
	}
	if conf.Valid {
		t := time.Unix(conf.Int64, 0)
		r.ConfirmedAt = &t
	}
	if cancelled.Valid {
		return ErrAlreadyClosed
	}
	seat.Sellable = sellable != 0
	return nil
}

const reservationJoinSeat = resCols + `, r.cancelled_at, ` +
	`s.id, s.show_id, s.row, s.col, s.x, s.y, s.label, s.category, s.price_kopecks, s.sellable`

// FindReservationByCode finds an active reservation. Cancelled rows return
// ErrAlreadyClosed.
func (s *Store) FindReservationByCode(ctx context.Context, code string) (Reservation, Seat, error) {
	var r Reservation
	var seat Seat
	err := scanReservationWithSeat(s.db.QueryRowContext(ctx, `
		SELECT `+reservationJoinSeat+`
		FROM reservations r JOIN seats s ON s.id = r.seat_id
		WHERE r.code = ?`, code), &r, &seat)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return r, seat, ErrCodeNotFound
	case errors.Is(err, ErrAlreadyClosed):
		return r, seat, ErrAlreadyClosed
	}
	return r, seat, err
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
	err := scanReservationWithSeat(s.db.QueryRowContext(ctx, `
		SELECT `+reservationJoinSeat+`
		FROM tickets t
		JOIN reservations r ON r.id = t.reservation_id
		JOIN seats s ON s.id = r.seat_id
		WHERE t.id = ?`, ticketID), &r, &seat)
	if errors.Is(err, ErrAlreadyClosed) {
		// A scanned ticket whose reservation got cancelled in parallel —
		// vanishingly rare, but don't fail the scan over it. Caller gets
		// the row data; the cancelled state isn't surfaced here.
		return r, seat, nil
	}
	return r, seat, err
}

// MyReservations returns the user's bookings, newest first.
func (s *Store) MyReservations(ctx context.Context, tgUserID int64) ([]MyItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+resCols+`,
		       s.id, s.show_id, s.row, s.col, s.x, s.y, s.label, s.category, s.price_kopecks, s.sellable
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
		var sellable int
		if err := rows.Scan(
			&r.ID, &r.SeatID, &r.TGUserID, &r.TGChatID, &r.BuyerName, &r.Code,
			scanTime(&r.CreatedAt), scanTime(&r.ExpiresAt), &conf,
			&seat.ID, &seat.ShowID, &seat.Row, &seat.Col, &seat.X, &seat.Y,
			&seat.Label, &seat.Category, &seat.PriceKopecks, &sellable,
		); err != nil {
			return nil, err
		}
		if conf.Valid {
			t := time.Unix(conf.Int64, 0)
			r.ConfirmedAt = &t
		}
		seat.Sellable = sellable != 0
		out = append(out, MyItem{Reservation: r, Seat: seat})
	}
	return out, rows.Err()
}

// Stats returns admin-facing counters for the show. Only sellable seats
// count toward Total/Free — aisles shouldn't make the hall look emptier
// than it is.
func (s *Store) Stats(ctx context.Context, showID int64) (Stats, error) {
	var st Stats
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM seats WHERE show_id = ? AND sellable = 1`, showID).Scan(&st.Total); err != nil {
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
		SELECT `+resCols+`,
		       s.id, s.show_id, s.row, s.col, s.x, s.y, s.label, s.category, s.price_kopecks, s.sellable
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
		var sellable int
		if err := rows.Scan(
			&r.ID, &r.SeatID, &r.TGUserID, &r.TGChatID, &r.BuyerName, &r.Code,
			scanTime(&r.CreatedAt), scanTime(&r.ExpiresAt), &conf,
			&seat.ID, &seat.ShowID, &seat.Row, &seat.Col, &seat.X, &seat.Y,
			&seat.Label, &seat.Category, &seat.PriceKopecks, &sellable,
		); err != nil {
			return nil, err
		}
		if conf.Valid {
			t := time.Unix(conf.Int64, 0)
			r.ConfirmedAt = &t
		}
		seat.Sellable = sellable != 0
		out = append(out, MyItem{Reservation: r, Seat: seat})
	}
	return out, rows.Err()
}

// SweepExpiredHolds cancels every reservation whose HOLD has lapsed and
// which was never paid for. Returns the number of rows touched. Safe to
// run on any schedule — it's idempotent and only touches stale rows.
func (s *Store) SweepExpiredHolds(ctx context.Context) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE reservations
		SET cancelled_at = ?
		WHERE cancelled_at IS NULL
		  AND confirmed_at IS NULL
		  AND expires_at < ?`, now, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) MarkReminded(ctx context.Context, reservationID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE reservations SET reminded_at = ? WHERE id = ?`, time.Now().Unix(), reservationID)
	return err
}

// --- helpers ---

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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func join(parts []string, sep string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	n := len(sep) * (len(parts) - 1)
	for _, p := range parts {
		n += len(p)
	}
	b := make([]byte, 0, n)
	b = append(b, parts[0]...)
	for _, p := range parts[1:] {
		b = append(b, sep...)
		b = append(b, p...)
	}
	return string(b)
}
