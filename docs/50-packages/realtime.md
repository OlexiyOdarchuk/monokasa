# 50 · Package: realtime

In-process pub/sub hub для подій "сидіння змінило статус". Публікують
producers (public reserve, pay confirm, admin cancel); споживає SSE
endpoint у `internal/public`, який стрімить події фронтенду.

## Public API

```go
type SeatStatus string
const (
    SeatFree SeatStatus = "free"
    SeatHeld SeatStatus = "held"
    SeatSold SeatStatus = "sold"
)

type Event struct {
    Type   string     `json:"type"`     // "seat_status"
    SeatID int64      `json:"seat_id"`
    Status SeatStatus `json:"status"`
}

type Hub struct { ... }
func New() *Hub
func (h *Hub) Subscribe(showID int64) (<-chan Event, func())   // unsub func MUST be called
func (h *Hub) Publish(showID int64, ev Event)                  // non-blocking, nil-safe
func (h *Hub) Subscribers() int
```

## Семантика

- Hub keyed by **show ID** (не slug) — slug-rename не лишає підписників
  орфанами.
- `Publish` ніколи не блокує: повний буфер підписника → frame скидається.
  Слабкий клієнт не може загальмувати hub.
- `nil`-safe: `(*Hub)(nil).Publish(...)` = no-op. Зручно у wiring'у, де
  hub може бути disabled (тести).
- `Subscribe` повертає read-only channel + unsubscribe-функцію.
  `unsub()` — ідемпотентна (захищена `sync.Once`).

## Synchronization

- `Subscribe` / `Unsubscribe` тримають **write lock** (`sync.RWMutex`).
- `Publish` тримає **read lock** під час send; send використовує
  `select { case ch <- ev: default: }`, тому не блокує.
- `close(ch)` робиться **усередині write lock'а** під час `unsub` —
  це гарантує що жоден `Publish` не тримає reference на цей канал у
  своїй ітерації, бо ітерація йде під read lock'ом, якого ми не
  можемо отримати, поки тримається write lock.

Race detector підтверджує (`go test -race`).

## Хто публікує

| Точка | Подія | Файл |
|---|---|---|
| Public reserve / order create | held | `internal/public/public.go` |
| Pay confirm (webhook + reconcile) | sold | `internal/pay/pay.go` |
| Admin force-cancel (cascade) | free × N | `internal/admin/admin.go` |

Не публікують (зараз — на доопрацювання):
- bot reserve (single і multi-seat)
- SweepExpiredHolds (hold → free)

## Хто підписується

`GET /api/public/shows/{slug}/events` — SSE-endpoint у public-пакеті.
Резолвить slug → showID → `Hub.Subscribe(showID)`. Стрімить події у
текстовий формат `data: <json>\n\n`. Keep-alive `: ping\n\n` кожні 25
секунд щоб proxy / browser не закрили idle-з'єднання.

## Frontend integration

`frontend/src/routes/event/[slug]/+page.svelte` тримає `EventSource`
відкритим протягом життя сторінки. Кожна подія оновлює `show.seats[i].taken`
реактивно. Якщо seat у нашому кошику став taken (хтось інший забрав)
— автоматично прибираємо з selection.

Browser EventSource робить reconnect сам (default 3с) — нам не треба
писати retry-логіку.

## Чому не WebSockets / pgnotify / Redis

- **WebSockets**: bidirectional, але нам треба тільки server → client.
  SSE простіший: автоматичний reconnect, працює через HTTP/1.1 без
  upgrade dance, проходить більшість corp-proxy.
- **pgnotify**: SQLite, не Postgres.
- **Redis pub/sub**: один процес — нема куди роз'єднувати pub і sub.

## Trade-offs / known limitations

- **One process**: hub не fan-out між replicas. Якщо колись з'явиться
  multi-instance — треба буде поміняти на щось network-aware (NATS,
  Redis stream).
- **Drop on slow**: повільний клієнт пропустить події. SSE-frontend
  fallback'ає на наступний GET `/api/public/shows/{slug}` коли йому
  знадобиться актуальна картина (наприклад на ре-фокусі вкладки —
  можна додати в майбутньому).

## Файли

- `hub.go` — Hub, Event, Subscribe, Publish (~120 рядків)
- `hub_test.go` — таблиці + race-test з concurrent sub/pub

## Дотичне

- [[50-packages/public]] — SSE endpoint
- [[20-data-model]] — звідки беруться seat-статуси
- [[40-flows/buyer-web]] — як це бачить покупець
