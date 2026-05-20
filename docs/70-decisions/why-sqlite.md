# ADR · Чому SQLite, а не Postgres

## Контекст

Початково я був готовий писати під Postgres (звичка). При переході на
self-host передумав.

## Рішення

**SQLite (modernc.org/sqlite, pure-Go) у WAL-режимі.**

## Чому

### Self-host friendly

Один organizer = одна машина. Розділяти БД на окремий сервер немає
сенсу. SQLite живе у файлі поряд з бінарником — ні docker network,
ні health checks для БД, ні credential management.

### Pure-Go driver

`modernc.org/sqlite` — pure-Go (без libsqlite3). Це означає:
- `CGO_ENABLED=0` під час `go build` → distroless / scratch / Alpine
  без додаткових deps;
- крос-компіляція тривіальна (`GOOS=linux GOARCH=arm64 go build` —
  працює).

### WAL mode

`PRAGMA journal_mode=WAL` дає:
- одночасні reads + 1 writer без локів на selects;
- backup через `.backup` без зупинки сервісу;
- ~10× швидші writes на наших обʼємах.

Для monokasa (events до 500 квитків) це з гарантією не bottleneck.

### Простота міграцій

`ALTER TABLE ADD COLUMN` у SQLite **тихо ігнорує дублі** — ідеально
для idempotent migrations. Список migrations live у Go-коді, без
окремого migration tool.

### Зручний backup і інспекція

`cp tix.db tix.db.bak` — повна копія. `sqlite3 tix.db` — інспекція з
любого пристрою. Жодного `pg_dump` / `pg_restore` / роботи з ролями.

## Що ми втрачаємо

| Postgres-фіча | Що замість |
|---|---|
| Concurrent writers | Для 500-квитка-events 1 writer достатньо. |
| Replication | Backup-копія. Якщо event критичний — `cp` перед стартом. |
| Сильніша типізація / strict mode | SQLite має strict mode у 3.37+, ми його не вмикаємо явно (не критично для цього scope). |
| Row-level security | Self-host = один tenant. RLS не треба. |
| Триггери / складні CTE | Логіка в Go-коді, не в БД. |

## Коли б ми переїхали на Postgres

- Якщо колись з'явиться "організація з 10 кафедр і 50 одночасних
  подій" — concurrent writers стане відчутним.
- Multi-tenant SaaS — це б тригернуло перепис цього ADR.

## Альтернативи що НЕ обрали

- **CGo-driver SQLite** (`mattn/go-sqlite3`) — швидший на ~20%, але
  require'ить C-toolchain для білда → distroless не запрацює без
  кастомного імеджу. Не вартує.
- **PocketBase / Litestream** — overkill для нашої моделі.
- **In-memory + JSON snapshot** — не масштабується ні в крос-рестарт,
  ні в reliability.

## Дотичне

- [[20-data-model]] — схема
- [[50-packages/store]] — package overview
