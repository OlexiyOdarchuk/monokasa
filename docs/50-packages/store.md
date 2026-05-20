# 50 · Package: store

SQLite persistence layer. Найбільший пакет — він тримає схему,
міграції і всі CRUD-методи.

## Public API

- **Open / Close** — відкриває БД, прокручує міграції, активує WAL.
- **CRUD shows**: `CreateShow`, `UpdateShow`, `ListShows`, `LoadShowBySlug`, `ArchiveShow`, `ActiveShow`.
- **CRUD seats**: `Seats`, `AddSeat`, `UpdateSeats` (batch), `RemoveSeat`, `FindFreeSeat`, `SeatStatuses`.
- **Reservations**: `Reserve` (shim над CreateOrder), `CancelReservation`, `FindReservationByCode`, `MyReservations`.
- **Orders**: `CreateOrder` (атомарно N seats), `FindOrderByCode`, `ConfirmOrder`, `LinkOrderToTGChat`, `AdminCancelReservation` (cascade на весь order).
- **Tickets**: `Confirm` (legacy), `UseTicket`, `FindReservationByTicket`.
- **Reminders**: `ConfirmedNotYetReminded`, `MarkReminded`.
- **Sweep**: `SweepExpiredHolds` — гасить покинуті hold'и.
- **Users / sessions**: `FindUserByEmail`, `CreateUser`, `CreateSession`, `FindSession`, `DeleteSession`, `SweepSessions`.
- **Stats**: `Stats(showID)` → Total/Sold/Held/Free/RevenueKopecks.

## Залежності

- `modernc.org/sqlite` — pure-Go SQLite driver (без CGo, deplo'їться в distroless).
- `database/sql`, `time`, `strings`.

Жодних import з інших internal-пакетів — store є кореневим вузлом
залежностей.

## Файли

- `store.go` — Open, міграції, всі методи (~1500 рядків)
- `types.go` — структури (`Show`, `Seat`, `Order`, `Reservation`, `Ticket`, `User`, `Session`, `Stats`, `SeatPatch`, `NewSeat`)
- `store_test.go` — integration тести на справжньому SQLite в `t.TempDir()`

## Ключові інваріанти

- Усі транзакції — `BeginTx` → `defer Rollback` → `Commit`.
- Reservation `code` UNIQUE. Multi-seat reservations мають `code.N` сабкод.
- Order `code` теж UNIQUE — і це **bare 8-char**, з ним матчиться
  webhook платіж.
- `confirmed_at`, `cancelled_at` — обидва nullable, обидва — Unix sec.
- `SweepExpiredHolds` — single SQL UPDATE, idempotent.

## Міграції

Стиль: великий `CREATE TABLE IF NOT EXISTS` блок для свіжої БД +
послідовність `ALTER TABLE ADD COLUMN` для існуючої. SQLite тихо
ігнорує дублі ADD COLUMN — тому ці виклики безпечні при кожному boot'і.

Backfill робиться через `UPDATE ... WHERE col IS NULL` або
`INSERT OR IGNORE ... WHERE NOT EXISTS` — idempotent.

Список міграцій лежить у `var migrations = []string{...}` всередині
файлу. Деталі моделі → [[20-data-model]].

## Адаптери

`cmd/app/main.go` обгортає `*store.Store` у:
- `botStore` для `internal/bot`
- `payStore` для `internal/pay`
- `authStore` для `internal/auth`
- `adminStore` для `internal/admin`
- `postersStore` — ні, posters не використовує store

Кожен адаптер переносить типи store ↔ типи цільового пакета (бо вони
не імпортують один одного — [[10-architecture]]).

## Тестування

`newTestStore(t)` створює свіжу БД в `t.TempDir()` і повертає
`*Store`. Усі тести — integration на справжньому SQLite (не моки),
бо помилки міграцій / транзакцій люблять ховатися від мок-тестів.

## Дотичне

- [[20-data-model]] — схема таблиць
- [[10-architecture]] — як склеєно з іншими пакетами
- [[70-decisions/why-sqlite]] — чому не Postgres
