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
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

const schema = `
CREATE TABLE IF NOT EXISTS shows (
    id           INTEGER PRIMARY KEY,
    slug         TEXT NOT NULL DEFAULT '',
    title        TEXT NOT NULL,
    venue        TEXT NOT NULL,
    starts_at    INTEGER NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    poster_url   TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL DEFAULT 0,
    archived_at  INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_shows_slug ON shows(slug);

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
    buyer_email     TEXT NOT NULL DEFAULT '',
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

CREATE TABLE IF NOT EXISTS users (
    id              INTEGER PRIMARY KEY,
    email           TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    name            TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id              INTEGER PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES users(id),
    token           TEXT NOT NULL UNIQUE,
    created_at      INTEGER NOT NULL,
    expires_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sess_token ON sessions(token);
CREATE INDEX IF NOT EXISTS idx_sess_expires ON sessions(expires_at);
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
	`ALTER TABLE reservations ADD COLUMN buyer_email TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE shows ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE shows ADD COLUMN archived_at INTEGER`,
	`ALTER TABLE shows ADD COLUMN slug TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE seats ADD COLUMN x REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE seats ADD COLUMN y REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE seats ADD COLUMN label TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE seats ADD COLUMN category TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE seats ADD COLUMN sellable INTEGER NOT NULL DEFAULT 1`,
	`UPDATE seats SET x = (col - 1) * 100.0 + 50.0, y = (row - 1) * 100.0 + 50.0 WHERE x = 0 AND y = 0`,
	// Backfill slugs for any pre-slug shows. Idempotent: on a second run
	// every show already has a non-empty slug so the WHERE skips them.
	`UPDATE shows SET slug = 'show-' || id WHERE slug = ''`,
	// And finally apply the unique index after backfill — ALTER TABLE ADD
	// COLUMN can't include UNIQUE, so we declare it here. CREATE INDEX is
	// idempotent via IF NOT EXISTS.
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_shows_slug ON shows(slug)`,
	`ALTER TABLE shows ADD COLUMN description TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE shows ADD COLUMN poster_url TEXT NOT NULL DEFAULT ''`,
	// Orders: groups one or more reservations under one payment code.
	// Single-seat reservations created before this migration get one
	// row each so the new code path (look up Order by code) handles
	// every reservation uniformly.
	`CREATE TABLE IF NOT EXISTS orders (
		id              INTEGER PRIMARY KEY,
		code            TEXT NOT NULL UNIQUE,
		buyer_name      TEXT NOT NULL DEFAULT '',
		buyer_email     TEXT NOT NULL DEFAULT '',
		tg_user_id      INTEGER NOT NULL DEFAULT 0,
		tg_chat_id      INTEGER NOT NULL DEFAULT 0,
		total_kopecks   INTEGER NOT NULL DEFAULT 0,
		created_at      INTEGER NOT NULL,
		expires_at      INTEGER NOT NULL,
		confirmed_at    INTEGER,
		cancelled_at    INTEGER,
		reminded_at     INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_orders_code ON orders(code)`,
	`ALTER TABLE reservations ADD COLUMN order_id INTEGER`,
	`CREATE INDEX IF NOT EXISTS idx_res_order ON reservations(order_id)`,
	// Backfill: every existing reservation becomes a single-item order.
	// INSERT OR IGNORE skips reservations already migrated (code UNIQUE
	// conflict). UPDATE then attaches order_id to all still-loose rows.
	`INSERT OR IGNORE INTO orders (code, buyer_name, buyer_email, tg_user_id, tg_chat_id, total_kopecks, created_at, expires_at, confirmed_at, cancelled_at, reminded_at)
		SELECT r.code, r.buyer_name, r.buyer_email, r.tg_user_id, r.tg_chat_id,
		       (SELECT price_kopecks FROM seats WHERE id = r.seat_id),
		       r.created_at, r.expires_at, r.confirmed_at, r.cancelled_at, r.reminded_at
		FROM reservations r
		WHERE r.order_id IS NULL`,
	`UPDATE reservations SET order_id = (SELECT id FROM orders WHERE code = reservations.code) WHERE order_id IS NULL`,
	// Per-ticket attendee name. Empty means "use the order's buyer name on
	// the PDF" — the renderer falls back accordingly. Only the multi-seat
	// web flow lets buyers fill these; bot and single-seat leave it empty.
	`ALTER TABLE reservations ADD COLUMN attendee_name TEXT NOT NULL DEFAULT ''`,
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
	ErrUserNotFound        = errors.New("user not found")
	ErrEmailTaken          = errors.New("email already registered")
	ErrSessionNotFound     = errors.New("session not found")
	ErrSessionExpired      = errors.New("session expired")
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

const showCols = `id, slug, title, venue, starts_at, description, poster_url, created_at, archived_at`

func scanShow(row interface{ Scan(...any) error }, sh *Show) error {
	var startsAt, createdAt int64
	var archivedAt sql.NullInt64
	if err := row.Scan(&sh.ID, &sh.Slug, &sh.Title, &sh.Venue, &startsAt,
		&sh.Description, &sh.PosterURL, &createdAt, &archivedAt); err != nil {
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
// something to start from. Slug is auto-derived from Title when blank,
// with a numeric suffix appended if the resulting slug collides.
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
	if show.Slug == "" {
		slug, err := s.uniqueSlugTx(ctx, tx, Slugify(show.Title))
		if err != nil {
			return 0, err
		}
		show.Slug = slug
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO shows(slug, title, venue, starts_at, description, poster_url, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		show.Slug, show.Title, show.Venue, show.StartsAt.Unix(),
		show.Description, show.PosterURL, show.CreatedAt.Unix())
	if err != nil {
		if isUniqueErr(err) {
			return 0, fmt.Errorf("slug %q already exists", show.Slug)
		}
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

// LoadShowBySlug looks the show up by its URL-safe handle. Used by the
// public buyer flow where IDs leak too much (e.g., "we have 17 shows so
// far"). Returns ErrShowNotFound for misses and for archived shows so
// stale links can't be ressurected by mistake.
func (s *Store) LoadShowBySlug(ctx context.Context, slug string) (Show, error) {
	var sh Show
	err := scanShow(
		s.db.QueryRowContext(ctx, `SELECT `+showCols+` FROM shows WHERE slug = ?`, slug),
		&sh)
	if errors.Is(err, sql.ErrNoRows) {
		return sh, ErrShowNotFound
	}
	if err != nil {
		return sh, err
	}
	if sh.ArchivedAt != nil {
		return sh, ErrShowNotFound
	}
	return sh, nil
}

// uniqueSlugTx tries `base`, then `base-2`, `base-3`, … until it finds a
// slug that doesn't already exist within the transaction. The search is
// bounded so a runaway loop on a misconfigured slug pattern doesn't hang.
func (s *Store) uniqueSlugTx(ctx context.Context, tx *sql.Tx, base string) (string, error) {
	if base == "" {
		base = "show"
	}
	for i := range 1000 {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", base, i+1)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM shows WHERE slug = ?`, candidate).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find a unique slug after 1000 attempts from base %q", base)
}

// Slugify produces a URL-safe slug from a free-form title. Lowercases
// ASCII letters, drops anything that isn't a letter/digit, collapses
// separators into single hyphens. Non-ASCII letters (Ukrainian Cyrillic,
// emoji, etc.) are dropped — the resulting slug may be empty for
// titles that are entirely Cyrillic, in which case CreateShow falls back
// to "show-<id>" via uniqueSlugTx.
//
// Future: add transliteration for Cyrillic so "Незвідана Зоря" →
// "nezvidana-zorya". For now empty-and-suffix is acceptable.
func Slugify(s string) string {
	out := make([]rune, 0, len(s))
	prevHyphen := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
			prevHyphen = false
		default:
			if !prevHyphen && len(out) > 0 {
				out = append(out, '-')
				prevHyphen = true
			}
		}
	}
	// Trim trailing hyphen if any.
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	return string(out)
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

// UpdateShow rewrites the editable fields of an existing show. ID, slug,
// created_at and archived_at are not touched — slug is set once at create
// and changing it would break shared links.
func (s *Store) UpdateShow(ctx context.Context, sh Show) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE shows SET title=?, venue=?, starts_at=?, description=?, poster_url=? WHERE id=?`,
		sh.Title, sh.Venue, sh.StartsAt.Unix(), sh.Description, sh.PosterURL, sh.ID)
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

// Reserve is a single-seat shortcut around CreateOrder. Keeps a stable
// API for the bot/legacy public flow while ensuring every reservation
// still belongs to an order (pay processor only looks orders up by code).
func (s *Store) Reserve(
	ctx context.Context, seat Seat, tgUserID, tgChatID int64,
	buyerName, buyerEmail, code string, hold time.Duration,
) (Reservation, error) {
	_, reservations, err := s.CreateOrder(ctx, []Seat{seat}, tgUserID, tgChatID, buyerName, buyerEmail, nil, code, hold)
	if err != nil {
		return Reservation{}, err
	}
	return reservations[0], nil
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

const resCols = `r.id, r.seat_id, r.tg_user_id, r.tg_chat_id, r.buyer_name, r.buyer_email,
	r.attendee_name, r.code,
	r.created_at, r.expires_at, r.confirmed_at`

// scanReservationWithSeat scans the union of resCols + cancelled_at + seatCols
// (with the `s.` prefix). The order must match the SELECT below. Cancelled
// rows are populated normally — callers that care about active-only state
// (FindReservationByCode) check r.CancelledAt themselves.
func scanReservationWithSeat(row interface{ Scan(...any) error }, r *Reservation, seat *Seat) error {
	var conf, cancelled sql.NullInt64
	var sellable int
	if err := row.Scan(
		&r.ID, &r.SeatID, &r.TGUserID, &r.TGChatID, &r.BuyerName, &r.BuyerEmail,
		&r.AttendeeName, &r.Code,
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
		t := time.Unix(cancelled.Int64, 0)
		r.CancelledAt = &t
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
	if errors.Is(err, sql.ErrNoRows) {
		return r, seat, ErrCodeNotFound
	}
	if err != nil {
		return r, seat, err
	}
	if r.CancelledAt != nil {
		return r, seat, ErrAlreadyClosed
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
	err := scanReservationWithSeat(s.db.QueryRowContext(ctx, `
		SELECT `+reservationJoinSeat+`
		FROM tickets t
		JOIN reservations r ON r.id = t.reservation_id
		JOIN seats s ON s.id = r.seat_id
		WHERE t.id = ?`, ticketID), &r, &seat)
	// Cancelled-state is not raised here — a scanned ticket whose
	// reservation got cancelled in parallel is vanishingly rare, and
	// blocking entry over it isn't the right call. Caller gets the row.
	return r, seat, err
}

// ListReservations returns every reservation for a show — including
// cancelled rows, which are needed for admin audit. Newest first.
func (s *Store) ListReservations(ctx context.Context, showID int64) ([]MyItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+reservationJoinSeat+`
		FROM reservations r
		JOIN seats s ON s.id = r.seat_id
		WHERE s.show_id = ?
		ORDER BY r.created_at DESC`, showID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MyItem
	for rows.Next() {
		var r Reservation
		var seat Seat
		if err := scanReservationWithSeat(rows, &r, &seat); err != nil {
			return nil, err
		}
		out = append(out, MyItem{Reservation: r, Seat: seat})
	}
	return out, rows.Err()
}

// AdminCancelReservation force-cancels the order this reservation
// belongs to. Cancelling a single seat in a multi-seat order cascades to
// every peer — partial cancels would leave the order half-paid, which
// confuses both the buyer and the pay processor.
//
// Paid cancellations are permitted: monobank refunds happen out of band,
// the DB just frees the seats and marks the rows. Already-cancelled
// orders return ErrAlreadyClosed.
//
// The returned freed slice contains every seat that became free as a
// result of the cancellation (including the targeted seat). Callers
// broadcast SSE seat-status events from this list.
func (s *Store) AdminCancelReservation(ctx context.Context, reservationID int64) (Reservation, Seat, []Seat, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Reservation{}, Seat{}, nil, err
	}
	defer tx.Rollback()

	var r Reservation
	var seat Seat
	err = scanReservationWithSeat(tx.QueryRowContext(ctx, `
		SELECT `+reservationJoinSeat+`
		FROM reservations r JOIN seats s ON s.id = r.seat_id
		WHERE r.id = ?`, reservationID), &r, &seat)
	if errors.Is(err, sql.ErrNoRows) {
		return r, seat, nil, ErrCodeNotFound
	}
	if err != nil {
		return r, seat, nil, err
	}
	if r.CancelledAt != nil {
		return r, seat, nil, ErrAlreadyClosed
	}

	var orderID sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT order_id FROM reservations WHERE id = ?`, reservationID).Scan(&orderID); err != nil {
		return r, seat, nil, err
	}

	now := time.Now()
	freed := []Seat{seat}
	if orderID.Valid {
		// Cascade: cancel every reservation in the order and the order
		// itself. Idempotent guards (cancelled_at IS NULL) prevent
		// stamping the same row twice if some peer was already
		// individually cancelled.
		//
		// Collect the peer seats BEFORE the UPDATE so callers can
		// broadcast realtime events for the whole order. We only need
		// rows that are about to be cancelled (cancelled_at IS NULL)
		// to avoid re-firing on already-cancelled peers.
		rows, err := tx.QueryContext(ctx, `
			SELECT s.id, s.show_id, s.row, s.col, s.x, s.y,
			       s.label, s.category, s.price_kopecks, s.sellable
			FROM reservations r JOIN seats s ON s.id = r.seat_id
			WHERE r.order_id = ? AND r.cancelled_at IS NULL AND r.id != ?`,
			orderID.Int64, reservationID)
		if err != nil {
			return r, seat, nil, err
		}
		for rows.Next() {
			var s Seat
			var sellable int
			if err := rows.Scan(&s.ID, &s.ShowID, &s.Row, &s.Col, &s.X, &s.Y,
				&s.Label, &s.Category, &s.PriceKopecks, &sellable); err != nil {
				rows.Close()
				return r, seat, nil, err
			}
			s.Sellable = sellable != 0
			freed = append(freed, s)
		}
		rows.Close()

		if _, err := tx.ExecContext(ctx,
			`UPDATE reservations SET cancelled_at=? WHERE order_id=? AND cancelled_at IS NULL`,
			now.Unix(), orderID.Int64); err != nil {
			return r, seat, nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE orders SET cancelled_at=? WHERE id=? AND cancelled_at IS NULL`,
			now.Unix(), orderID.Int64); err != nil {
			return r, seat, nil, err
		}
	} else {
		// Legacy reservation pre-dating orders (shouldn't exist after
		// the backfill, but defend against partial state).
		if _, err := tx.ExecContext(ctx,
			`UPDATE reservations SET cancelled_at=? WHERE id=?`, now.Unix(), r.ID); err != nil {
			return r, seat, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return r, seat, nil, err
	}
	r.CancelledAt = &now
	return r, seat, freed, nil
}

// --- orders (multi-seat) ---

// CreateOrder atomically books N seats under one payment code. Each
// underlying reservation gets a derived sub-code "<orderCode>.<seq>" so
// the legacy UNIQUE(reservations.code) constraint stays satisfied,
// while the webhook still matches the bare 8-char base32 order code in
// the monobank comment.
//
// All seats must belong to the same show and be free + sellable. Any
// violation rolls back the entire transaction.
//
// attendeeNames is optional: pass nil (every ticket prints the buyer
// name) or a slice of len(seats) where each entry is the per-seat
// attendee. Empty strings inside the slice are treated the same as
// nil — that ticket falls back to buyerName at render time.
func (s *Store) CreateOrder(
	ctx context.Context, seats []Seat, tgUserID, tgChatID int64,
	buyerName, buyerEmail string, attendeeNames []string,
	code string, hold time.Duration,
) (Order, []Reservation, error) {
	if len(seats) == 0 {
		return Order{}, nil, errors.New("CreateOrder: empty seats")
	}
	if attendeeNames != nil && len(attendeeNames) != len(seats) {
		return Order{}, nil, fmt.Errorf(
			"CreateOrder: attendeeNames len %d != seats len %d",
			len(attendeeNames), len(seats))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Order{}, nil, err
	}
	defer tx.Rollback()

	now := time.Now()
	expires := now.Add(hold)

	var total int64
	for _, seat := range seats {
		// Re-verify each seat inside the tx: sellable, free, exists.
		var sellable int
		if err := tx.QueryRowContext(ctx,
			`SELECT sellable FROM seats WHERE id=?`, seat.ID).Scan(&sellable); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Order{}, nil, ErrSeatNotFound
			}
			return Order{}, nil, err
		}
		if sellable == 0 {
			return Order{}, nil, ErrSeatNotSellable
		}
		var taken int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM reservations
			WHERE seat_id=? AND cancelled_at IS NULL
			  AND (confirmed_at IS NOT NULL OR expires_at > ?)`,
			seat.ID, now.Unix()).Scan(&taken); err != nil {
			return Order{}, nil, err
		}
		if taken > 0 {
			return Order{}, nil, ErrSeatTaken
		}
		total += seat.PriceKopecks
	}

	orderRes, err := tx.ExecContext(ctx, `
		INSERT INTO orders(code, buyer_name, buyer_email, tg_user_id, tg_chat_id,
		                   total_kopecks, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		code, buyerName, buyerEmail, tgUserID, tgChatID, total, now.Unix(), expires.Unix())
	if err != nil {
		if isUniqueErr(err) {
			return Order{}, nil, fmt.Errorf("order code %q already exists", code)
		}
		return Order{}, nil, err
	}
	orderID, _ := orderRes.LastInsertId()

	reservations := make([]Reservation, 0, len(seats))
	for i, seat := range seats {
		subcode := code
		if len(seats) > 1 {
			subcode = fmt.Sprintf("%s.%d", code, i+1)
		}
		var attendee string
		if attendeeNames != nil {
			attendee = strings.TrimSpace(attendeeNames[i])
		}
		resRes, err := tx.ExecContext(ctx, `
			INSERT INTO reservations(order_id, seat_id, tg_user_id, tg_chat_id,
			                         buyer_name, buyer_email, attendee_name, code,
			                         created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			orderID, seat.ID, tgUserID, tgChatID, buyerName, buyerEmail,
			attendee, subcode, now.Unix(), expires.Unix())
		if err != nil {
			return Order{}, nil, err
		}
		resID, _ := resRes.LastInsertId()
		reservations = append(reservations, Reservation{
			ID: resID, SeatID: seat.ID, TGUserID: tgUserID, TGChatID: tgChatID,
			BuyerName: buyerName, BuyerEmail: buyerEmail, AttendeeName: attendee,
			Code:      subcode,
			CreatedAt: now, ExpiresAt: expires,
		})
	}
	if err := tx.Commit(); err != nil {
		return Order{}, nil, err
	}
	return Order{
		ID: orderID, Code: code,
		BuyerName: buyerName, BuyerEmail: buyerEmail,
		TGUserID: tgUserID, TGChatID: tgChatID,
		TotalKopecks: total,
		CreatedAt:    now, ExpiresAt: expires,
	}, reservations, nil
}

// OrderItem couples a reservation with its seat — what the pay processor
// needs to render one PDF per seat.
type OrderItem struct {
	Reservation Reservation
	Seat        Seat
}

// FindOrderByCode looks up an active order (not cancelled) plus its items.
// Returns ErrCodeNotFound for missing, ErrAlreadyClosed for cancelled.
func (s *Store) FindOrderByCode(ctx context.Context, code string) (Order, []OrderItem, error) {
	var o Order
	var conf, cancelled, reminded sql.NullInt64
	var createdAt, expiresAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, code, buyer_name, buyer_email, tg_user_id, tg_chat_id,
		       total_kopecks, created_at, expires_at, confirmed_at, cancelled_at, reminded_at
		FROM orders WHERE code = ?`, code).Scan(
		&o.ID, &o.Code, &o.BuyerName, &o.BuyerEmail, &o.TGUserID, &o.TGChatID,
		&o.TotalKopecks, &createdAt, &expiresAt, &conf, &cancelled, &reminded)
	if errors.Is(err, sql.ErrNoRows) {
		return o, nil, ErrCodeNotFound
	}
	if err != nil {
		return o, nil, err
	}
	o.CreatedAt = time.Unix(createdAt, 0)
	o.ExpiresAt = time.Unix(expiresAt, 0)
	if conf.Valid {
		t := time.Unix(conf.Int64, 0)
		o.ConfirmedAt = &t
	}
	if cancelled.Valid {
		t := time.Unix(cancelled.Int64, 0)
		o.CancelledAt = &t
		return o, nil, ErrAlreadyClosed
	}
	if reminded.Valid {
		t := time.Unix(reminded.Int64, 0)
		o.RemindedAt = &t
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+resCols+`, r.cancelled_at,
		       s.id, s.show_id, s.row, s.col, s.x, s.y, s.label, s.category, s.price_kopecks, s.sellable
		FROM reservations r JOIN seats s ON s.id = r.seat_id
		WHERE r.order_id = ?
		ORDER BY r.id`, o.ID)
	if err != nil {
		return o, nil, err
	}
	defer rows.Close()
	var items []OrderItem
	for rows.Next() {
		var r Reservation
		var seat Seat
		if err := scanReservationWithSeat(rows, &r, &seat); err != nil {
			return o, nil, err
		}
		items = append(items, OrderItem{Reservation: r, Seat: seat})
	}
	return o, items, nil
}

// ConfirmOrder marks the order paid, issues a Ticket per reservation
// with its QR payload, and stamps confirmed_at on every reservation row.
// Atomic — partial confirmation is impossible.
func (s *Store) ConfirmOrder(ctx context.Context, orderID int64, qrPayloads map[int64]string) ([]Ticket, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var conf, cancelled sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT confirmed_at, cancelled_at FROM orders WHERE id=?`, orderID).
		Scan(&conf, &cancelled); err != nil {
		return nil, err
	}
	if cancelled.Valid {
		return nil, ErrAlreadyClosed
	}
	if conf.Valid {
		return nil, ErrAlreadyPaid
	}

	now := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE orders SET confirmed_at=? WHERE id=?`,
		now.Unix(), orderID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE reservations SET confirmed_at=? WHERE order_id=?`,
		now.Unix(), orderID); err != nil {
		return nil, err
	}

	// Insert one Ticket per reservation. qrPayloads keyed by reservation.id.
	rows, err := tx.QueryContext(ctx, `SELECT id FROM reservations WHERE order_id=? ORDER BY id`, orderID)
	if err != nil {
		return nil, err
	}
	var resIDs []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		resIDs = append(resIDs, id)
	}
	rows.Close()

	tickets := make([]Ticket, 0, len(resIDs))
	for _, rid := range resIDs {
		qr, ok := qrPayloads[rid]
		if !ok {
			return nil, fmt.Errorf("ConfirmOrder: missing QR payload for reservation %d", rid)
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO tickets(reservation_id, qr_payload, issued_at) VALUES (?, ?, ?)`,
			rid, qr, now.Unix())
		if err != nil {
			return nil, err
		}
		tid, _ := res.LastInsertId()
		tickets = append(tickets, Ticket{
			ID: tid, ReservationID: rid, QRPayload: qr, IssuedAt: now,
		})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return tickets, nil
}

// LinkOrderToTGChat associates a web-buyer order with a Telegram chat so
// the bot can deliver the PDFs after payment in addition to (or instead
// of) the email channel. Idempotent: re-linking the same chat is a
// no-op; linking a different chat overwrites — the most recent /start
// wins.
//
// Both the order row and every reservation row in it get the new chat
// ids (reservations stay denormalised for legacy queries). Returns
// ErrCodeNotFound for missing codes, ErrAlreadyClosed for cancelled.
func (s *Store) LinkOrderToTGChat(ctx context.Context, code string, tgUserID, tgChatID int64) (Order, []OrderItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Order{}, nil, err
	}
	defer tx.Rollback()

	o, items, err := s.findOrderByCodeTx(ctx, tx, code)
	if err != nil {
		return o, nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE orders SET tg_user_id = ?, tg_chat_id = ? WHERE id = ?`,
		tgUserID, tgChatID, o.ID); err != nil {
		return o, nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE reservations SET tg_user_id = ?, tg_chat_id = ? WHERE order_id = ?`,
		tgUserID, tgChatID, o.ID); err != nil {
		return o, nil, err
	}
	if err := tx.Commit(); err != nil {
		return o, nil, err
	}
	o.TGUserID = tgUserID
	o.TGChatID = tgChatID
	return o, items, nil
}

// findOrderByCodeTx reuses the same scan as FindOrderByCode but inside an
// open transaction. Extracted so LinkOrderToTGChat reads order + items
// atomically with its own UPDATE.
func (s *Store) findOrderByCodeTx(ctx context.Context, tx *sql.Tx, code string) (Order, []OrderItem, error) {
	var o Order
	var conf, cancelled, reminded sql.NullInt64
	var createdAt, expiresAt int64
	err := tx.QueryRowContext(ctx, `
		SELECT id, code, buyer_name, buyer_email, tg_user_id, tg_chat_id,
		       total_kopecks, created_at, expires_at, confirmed_at, cancelled_at, reminded_at
		FROM orders WHERE code = ?`, code).Scan(
		&o.ID, &o.Code, &o.BuyerName, &o.BuyerEmail, &o.TGUserID, &o.TGChatID,
		&o.TotalKopecks, &createdAt, &expiresAt, &conf, &cancelled, &reminded)
	if errors.Is(err, sql.ErrNoRows) {
		return o, nil, ErrCodeNotFound
	}
	if err != nil {
		return o, nil, err
	}
	o.CreatedAt = time.Unix(createdAt, 0)
	o.ExpiresAt = time.Unix(expiresAt, 0)
	if conf.Valid {
		t := time.Unix(conf.Int64, 0)
		o.ConfirmedAt = &t
	}
	if cancelled.Valid {
		t := time.Unix(cancelled.Int64, 0)
		o.CancelledAt = &t
		return o, nil, ErrAlreadyClosed
	}
	if reminded.Valid {
		t := time.Unix(reminded.Int64, 0)
		o.RemindedAt = &t
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT `+resCols+`, r.cancelled_at,
		       s.id, s.show_id, s.row, s.col, s.x, s.y, s.label, s.category, s.price_kopecks, s.sellable
		FROM reservations r JOIN seats s ON s.id = r.seat_id
		WHERE r.order_id = ?
		ORDER BY r.id`, o.ID)
	if err != nil {
		return o, nil, err
	}
	defer rows.Close()
	var items []OrderItem
	for rows.Next() {
		var r Reservation
		var seat Seat
		if err := scanReservationWithSeat(rows, &r, &seat); err != nil {
			return o, nil, err
		}
		items = append(items, OrderItem{Reservation: r, Seat: seat})
	}
	return o, items, nil
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
			&r.ID, &r.SeatID, &r.TGUserID, &r.TGChatID, &r.BuyerName, &r.BuyerEmail,
			&r.AttendeeName, &r.Code,
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
			&r.ID, &r.SeatID, &r.TGUserID, &r.TGChatID, &r.BuyerName, &r.BuyerEmail,
			&r.AttendeeName, &r.Code,
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
// which was never paid for. Returns the freed seats so callers can
// broadcast SSE seat-status events. Safe to run on any schedule — it's
// idempotent and only touches stale rows.
//
// SELECT and UPDATE run in one transaction so we never report a seat
// as freed before its row is actually cancelled.
func (s *Store) SweepExpiredHolds(ctx context.Context) ([]Seat, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	rows, err := tx.QueryContext(ctx, `
		SELECT s.id, s.show_id, s.row, s.col, s.x, s.y,
		       s.label, s.category, s.price_kopecks, s.sellable
		FROM reservations r JOIN seats s ON s.id = r.seat_id
		WHERE r.cancelled_at IS NULL
		  AND r.confirmed_at IS NULL
		  AND r.expires_at < ?`, now)
	if err != nil {
		return nil, err
	}
	var freed []Seat
	for rows.Next() {
		var seat Seat
		var sellable int
		if err := rows.Scan(&seat.ID, &seat.ShowID, &seat.Row, &seat.Col,
			&seat.X, &seat.Y, &seat.Label, &seat.Category,
			&seat.PriceKopecks, &sellable); err != nil {
			rows.Close()
			return nil, err
		}
		seat.Sellable = sellable != 0
		freed = append(freed, seat)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE reservations
		SET cancelled_at = ?
		WHERE cancelled_at IS NULL
		  AND confirmed_at IS NULL
		  AND expires_at < ?`, now, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return freed, nil
}

func (s *Store) MarkReminded(ctx context.Context, reservationID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE reservations SET reminded_at = ? WHERE id = ?`, time.Now().Unix(), reservationID)
	return err
}

// --- users & sessions ---

// CreateUser inserts a new admin account. The caller is responsible for
// hashing the password (use internal/auth.HashPassword) — store doesn't
// import bcrypt to keep this package free of crypto deps.
func (s *Store) CreateUser(ctx context.Context, email, name, passwordHash string) (User, error) {
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users(email, password_hash, name, created_at) VALUES (?, ?, ?, ?)`,
		email, passwordHash, name, now.Unix())
	if err != nil {
		// SQLite reports UNIQUE constraint failures via the error string;
		// the modernc driver doesn't expose a typed error here, so the
		// substring check is the pragmatic option.
		if isUniqueErr(err) {
			return User{}, ErrEmailTaken
		}
		return User{}, err
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Email: email, PasswordHash: passwordHash, Name: name, CreatedAt: now}, nil
}

// FindUserByEmail returns the user row keyed by case-sensitive email.
func (s *Store) FindUserByEmail(ctx context.Context, email string) (User, error) {
	var u User
	var createdAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, name, created_at FROM users WHERE email = ?`, email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrUserNotFound
	}
	if err != nil {
		return u, err
	}
	u.CreatedAt = time.Unix(createdAt, 0)
	return u, nil
}

// FindUserByID returns the user row for the given id.
func (s *Store) FindUserByID(ctx context.Context, id int64) (User, error) {
	var u User
	var createdAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, name, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrUserNotFound
	}
	if err != nil {
		return u, err
	}
	u.CreatedAt = time.Unix(createdAt, 0)
	return u, nil
}

// CountUsers returns how many admin accounts exist. Used at startup to
// decide whether the bootstrap-admin ENV vars need to fire.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateSession persists a freshly minted session token. The token itself
// is generated by the caller (auth.NewToken) so the store stays
// crypto-free.
func (s *Store) CreateSession(ctx context.Context, userID int64, token string, ttl time.Duration) (Session, error) {
	now := time.Now()
	exp := now.Add(ttl)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(user_id, token, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		userID, token, now.Unix(), exp.Unix())
	if err != nil {
		return Session{}, err
	}
	id, _ := res.LastInsertId()
	return Session{ID: id, UserID: userID, Token: token, CreatedAt: now, ExpiresAt: exp}, nil
}

// FindSession looks up a session by token and joins the user row in one
// query — typical use is "is this cookie valid, and who does it belong
// to?". Returns ErrSessionNotFound for missing tokens and ErrSessionExpired
// for tokens past their TTL (the row is left for the sweeper).
func (s *Store) FindSession(ctx context.Context, token string) (Session, User, error) {
	var sess Session
	var u User
	var sessCreated, sessExpires, userCreated int64
	err := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.user_id, s.token, s.created_at, s.expires_at,
		       u.id, u.email, u.password_hash, u.name, u.created_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token = ?`, token).Scan(
		&sess.ID, &sess.UserID, &sess.Token, &sessCreated, &sessExpires,
		&u.ID, &u.Email, &u.PasswordHash, &u.Name, &userCreated)
	if errors.Is(err, sql.ErrNoRows) {
		return sess, u, ErrSessionNotFound
	}
	if err != nil {
		return sess, u, err
	}
	sess.CreatedAt = time.Unix(sessCreated, 0)
	sess.ExpiresAt = time.Unix(sessExpires, 0)
	u.CreatedAt = time.Unix(userCreated, 0)
	if sess.ExpiresAt.Before(time.Now()) {
		return sess, u, ErrSessionExpired
	}
	return sess, u, nil
}

// DeleteSession revokes a session by token. Missing token is not an
// error — logout should be idempotent.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// SweepExpiredSessions deletes rows whose expires_at has passed. Cheap to
// run on a slow ticker — the index on expires_at keeps it O(log n).
func (s *Store) SweepExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// isUniqueErr peeks at the SQLite error string to detect UNIQUE-constraint
// violations. modernc/sqlite doesn't expose a typed error here, so this is
// the pragmatic option. Surface looks like:
//
//	"constraint failed: UNIQUE constraint failed: users.email (2067)"
func isUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
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
