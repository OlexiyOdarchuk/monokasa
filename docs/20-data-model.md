# 20 · Модель даних

## Сутності

```
   ┌───────┐     ┌──────┐
   │ shows │1───*│seats │
   └───┬───┘     └──┬───┘
       │            │
       │            │
       │       ┌────┴─────────┐
       │       │              │
       │  ┌────▼──────────┐   │
       └──┤ reservations  │   │
          │  (order_id ──┐│   │ ← seat_id 1:1
          └──┬───────────┘│   │
             │            │   │
             │       ┌────▼───▼─┐
             │       │  orders  │ ← (1) order ─→ (1..N) reservations
             │       └──────────┘    one pay code covers all
             │
             │
        ┌────▼─────┐
        │ tickets  │ ← created on Confirm, points at reservation, 1:1
        └──────────┘
```

Окремо: `users` (адмін-логіни) + `sessions` (auth-токени).

## Кожна таблиця

### shows
| col | type | заміт |
|---|---|---|
| id | INTEGER PK | |
| slug | TEXT UNIQUE | URL-handle, ASCII, fallback `show-<id>` для кириличних title |
| title | TEXT | |
| venue | TEXT | |
| starts_at | INTEGER | unix sec |
| description | TEXT | free-form |
| poster_url | TEXT | відносний `/posters/<32hex>.jpg` або external https:// |
| created_at | INTEGER | |
| archived_at | INTEGER | nullable; non-null = ховаємо з landing і бота |

### seats
| col | type | заміт |
|---|---|---|
| id, show_id, row, col | | UNIQUE(show_id, row, col) |
| x, y | REAL | canvas координати для drag&drop |
| label | TEXT | "" → "Ряд R · місце C" |
| category | TEXT | "vip", "balcony" тощо; впливає на колір |
| price_kopecks | INTEGER | per-seat ціна |
| sellable | INTEGER | 0 = проходи / технічні |

### orders
| col | type | заміт |
|---|---|---|
| id, code | INT, TEXT UNIQUE | code = 8-char base32 (a-z + 2-7) |
| buyer_name, buyer_email | | |
| tg_user_id, tg_chat_id | | 0 для web-only |
| total_kopecks | | сума по всіх місцях |
| created_at, expires_at | | |
| confirmed_at, cancelled_at, reminded_at | nullable | |

### reservations
| col | type | заміт |
|---|---|---|
| id, order_id, seat_id, tg_user_id, tg_chat_id | | |
| buyer_name, buyer_email | | дублюється з order для legacy queries |
| attendee_name | TEXT default '' | per-ticket ім'я; '' = fallback на BuyerName |
| code | TEXT UNIQUE | `code` для single-seat, `code.1`/`code.2`/… для multi |
| created_at, expires_at, confirmed_at, cancelled_at, reminded_at | | |

### tickets
| col | type | заміт |
|---|---|---|
| id, reservation_id | | один tкт per reservation |
| qr_payload | TEXT | HMAC-підписаний; см. [[50-packages/token]] |
| issued_at, used_at | | non-null used_at → "вже використано" на сканері |

### users
- bcrypt cost 12, password_hash

### sessions
- 256-bit opaque token, MaxAge 30 днів

### buyer_login_tokens
- Одноразовий 256-bit hex token emailed buyer'у для magic-link login
- email (lowercase), expires_at (TTL 15 хв), used_at (один shot)

### buyer_sessions
- 256-bit opaque cookie token для довгоживучої buyer-сесії
- email (lowercase), MaxAge 30 днів
- Окремо від `sessions` (admin) — щоб один браузер міг бути одночасно
  admin'ом і buyer'ом

## Стани сутностей

### Order
```
   ┌────────┐  CreateOrder
   │        │ ──────────────► held ──► confirmed (ConfirmOrder)
   │ (none) │                  │  │
   │        │                  │  └─► expired (expires_at < now, swept)
   │        │                  └────► cancelled (admin / user)
   └────────┘
```

### Reservation
дублює стан order'а; кожен рядок має власні `confirmed_at`, `cancelled_at`.

### Seat
не має поля стану — статус виводиться запитом:
- `seat_statuses(show_id)` → `{seat_id: free|held|sold}`
- `sold` = є reservation з confirmed_at IS NOT NULL
- `held` = reservation активна (expires_at > now, не cancelled, не confirmed)
- `free` = ні те, ні інше

### Ticket
- створюється тільки на ConfirmOrder, разом з `confirmed_at`.
- одноразовий: `UseTicket` ставить `used_at`. Повторне сканування →
  ErrTicketUsed.

## Коди

8-char base32 без визуально подібних символів (`a-z2-7`, нема
`01v8`). ~40 біт ентропії — норм для коротко-живих кодів.

- **Order code**: 8 chars, чисто `abc12345`.
- **Reservation code**: для single-seat = order code. Для multi = `<order>.N` (`abc12345.1`, `abc12345.2`).

Webhook matchить **bare 8 chars** з коментаря платежу → `FindOrderByCode`.

## Міграції

Стиль:
- Свіжа БД: великий `CREATE TABLE` блок з усіма колонками.
- Існуюча БД: послідовність `ALTER TABLE ADD COLUMN`, що SQLite тихо
  ігнорує при дублюванні.
- Backfill: `UPDATE ... WHERE col IS NULL` — idempotent.
- INSERT OR IGNORE для backfill, де може бути конфлікт UNIQUE.

Список у `internal/store/store.go::migrations`. Деталі → [[50-packages/store]].

## Codes vs Tickets

- **Code** — payload у коментарі платежу. Покупець бачить.
- **QR payload** — те, що зашите в quad-кодах. HMAC-підписане, прив'язане
  до `reservation_id` і `seat_id`. Покупець НЕ бачить (тільки сканер).

Розрізняти важливо: код для оплати ≠ код для входу.

## Дотичне

- [[50-packages/store]] — методи + міграційний список
- [[50-packages/token]] — як генерується code і QR payload
- [[70-decisions/multi-seat-orders]] — чому розділено order ↔ reservation
