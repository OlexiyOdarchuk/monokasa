# 40 · Flow: покупець заходить у "Мої квитки"

## Сценарій

Покупець хоче подивитись свої квитки на сайті — побачити QR одразу,
без копання в email'ах, перевірити статус ще раз перед подією.

Аутентифікація — **passwordless через magic-link**. Без реєстрації,
без password manager. Хто володіє email'ом — той бачить свої квитки.

## Послідовність

```
buyer            frontend                backend                       SMTP
  │                 │                       │                            │
  │ open /my        │                       │                            │
  │────────────────►│ GET /api/public/my    │                            │
  │                 │──────────────────────►│ FindBuyerSession(cookie)   │
  │                 │                       │   → 401 (немає сесії)      │
  │                 │◄──────────────────────│                            │
  │ форма "введи    │ render <form>                                       │
  │  email"         │                       │                            │
  │                 │                       │                            │
  │ submit email    │                       │                            │
  │────────────────►│ POST /login/request   │                            │
  │                 │──────────────────────►│ CreateBuyerLoginToken      │
  │                 │                       │ (TTL 15 хв)                │
  │                 │                       │ async SendLoginLink ──────►│
  │                 │◄──────────────────────│ {status: "sent"}           │
  │ "лист пішов"    │                       │                            │
  │                 │                       │                            │ shipping
  │                 │                       │                            │ email...
  │ ◄───────── email ─────────────────────────────────────────────────────│
  │                                                                       │
  │ клік на magic-link                                                    │
  │ /api/public/login/consume?token=…                                     │
  │────────────────────────────────────────►│ ConsumeBuyerLoginToken     │
  │                                         │   (одноразовий burn)        │
  │                                         │ CreateBuyerSession         │
  │                                         │ Set-Cookie: monokasa_buyer │
  │                                         │ 303 Location: /my          │
  │◄────────────────────────────────────────│                            │
  │                                                                       │
  │ browser follows 303 to /my                                            │
  │                 │ GET /api/public/my    │                            │
  │                 │──────────────────────►│ FindBuyerSession(cookie)   │
  │                 │                       │   → email                  │
  │                 │◄──────────────────────│ {email: "..."}             │
  │                 │ GET /my/tickets       │                            │
  │                 │──────────────────────►│ BuyerTicketsByEmail        │
  │                 │◄──────────────────────│ orders[] з QR payloads     │
  │ список квитків  │ render QR canvas                                    │
  │ з QR кодами     │                       │                            │
```

## Чому magic-link, а не password

- **Менше тертя**: ввів email → отримав лист → клік → ти всередині.
  Жодного password manager / "забув пароль" / 2FA.
- **Менше коду**: нема reset flow, нема bcrypt для buyer'а, нема валідації паролів.
- **Не гірша безпека**: контроль над email = контроль над акаунтом. Якщо
  атакер може читати email — він і за класичним forgot-password
  процесом залогіниться. Token живе 15 хв, одноразовий.

Деталі обґрунтування → [[70-decisions/buyer-auth-magic-link]].

## Endpoint reference

| Метод | Шлях | Що робить |
|---|---|---|
| POST | `/api/public/login/request` | Створити login-токен + надіслати лист (або залогувати лінк, якщо SMTP не сконфігурований) |
| GET | `/api/public/login/consume?token=` | Burn token, set cookie, 303 на /my (або /my?error=… при невдачі) |
| POST | `/api/public/login/logout` | Видалити сесію + cookie |
| GET | `/api/public/my` | whoami; повертає email або 401 |
| GET | `/api/public/my/tickets` | Список замовлень + items + QR payloads |

## Schema

```
buyer_login_tokens
  id PK, token UNIQUE, email, created_at, expires_at (TTL 15 хв), used_at

buyer_sessions
  id PK, token UNIQUE, email, created_at, expires_at (30 днів)
```

Обидві email-колонки lowercase'нуть на запис, тому case-insensitive
матчинг із `orders.buyer_email` працює без normalize'у на читанні.

## Cookie

- **Назва**: `monokasa_buyer` (окрема від admin `monokasa_admin` — щоб
  один браузер міг бути одночасно admin'ом і buyer'ом без колізій)
- **HttpOnly**, **SameSite=Lax**, **Path=/**, **MaxAge 30d**
- **Secure** автоматично, якщо `SECURE_COOKIES=true` або запит по HTTPS

## Чому magic-link йде ПРЯМО на consume endpoint

Раніше пробували `link = /my?token=…`, SPA брав token і ходив `fetch`
на consume з `redirect: 'manual'`. У деяких браузерах **Set-Cookie з
opaqueredirect-відповідей не зберігається** — користувач "залогінювався"
але cookie не з'являлась, на наступному запиті 401.

Тепер magic-link = `/api/public/login/consume?token=…`. Browser сам
обробляє 303 з Set-Cookie + Location → cookie ставиться нативно,
користувач опиняється на /my з повною сесією.

## Edge cases

| Що | Як обробляється |
|---|---|
| SMTP не налаштований | `loginRequest` пише magic-link у `slog.Warn` замість надсилання. Frontend показує "⚠️ SMTP не налаштований — лінк у логах". Для локального тесту. |
| BASE_URL не виставлений | Резолвиться з `scheme + host` запиту (TLS + X-Forwarded-Proto aware). |
| Token прострочений | consume повертає 303 `/my?error=expired_token` → frontend показує "Посилання прострочене — запроси нове" |
| Token уже використаний | 303 `/my?error=invalid_token` → "Запроси нове" |
| Email не має жодних замовлень | /my/tickets повертає `[]` → frontend показує "У тебе немає бронювань. ← До афіші" |
| Buyer виходить (logout) | `DeleteBuyerSession` + clear cookie. Інші cookies (admin) не торкаються. |

## Файли

- `internal/store/store.go::CreateBuyerLoginToken / ConsumeBuyerLoginToken / CreateBuyerSession / FindBuyerSession / DeleteBuyerSession / BuyerTicketsByEmail`
- `internal/public/public.go::loginRequest / loginConsume / loginLogout / myWhoami / myTickets`
- `cmd/app/main.go::buyerLoginMailer` — SMTP композиція листа
- `frontend/src/routes/my/+page.svelte` — UI
- `frontend/src/lib/api.ts::BuyerTicket / BuyerTicketItem / BuyerTicketShow` — типи

## Дотичне

- [[buyer-web]] — звідки беруться замовлення
- [[50-packages/email]] — SMTP компонент
- [[70-decisions/buyer-auth-magic-link]] — обґрунтування підходу
