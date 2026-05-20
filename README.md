# monokasa

[![Built with Claude Code](https://img.shields.io/badge/Built%20with-Claude%20Code-D97757?logo=anthropic&logoColor=white)](https://claude.com/claude-code)

**Friendly self-host** для продажу квитків на події через монобанку. Один
контейнер дає тобі Telegram-бота, веб-сайт із мапою залу, адмін-панель і
сканер на вході. Платежі йдуть напряму на твою банку моно — без посередника,
без комісії платформи, без чужої БД.

Старт — `docker compose up`. Все, що треба сконфігурити — токен бота і
посилання на банку моно.

## Що в коробці

- **Бот** (Telegram, long-polling): афіша подій, in-chat мапа залу з
  multi-seat кошиком, оплата через банку моно, PDF із QR прямо в чат,
  нагадування за годину до старту.
- **Web для покупця**: лендінг із плитками подій (poster + дата),
  сторінка події з SVG-мапою, multi-seat вибір, статус оплати в реальному
  часі через poll-loop. Опційно — інтеграція з Telegram через deep-link,
  щоб PDF прийшов і на email, і в чат.
- **Admin web** (SvelteKit + cookie-сесії): CRUD подій, drag&drop редактор
  залу, завантаження афіші, гості/виторг по події, cancel із сповіщенням
  покупцю (TG + email).
- **Сканер** на вході: повноекранний QR-сканер у браузері, HMAC-перевірка
  квитків, sound+vibrate cues, anti-replay.
- **Webhook + reconcile**: monobank → одразу видає PDF. `/reconcile` —
  ручне догрібання пропущених транзакцій з виписки.
- **Email delivery** (SMTP): після оплати клієнт отримує PDF на пошту;
  multi-seat — N attachments одним листом.

Один інстанс = один organizer. Multi-tenant у плани не входить — це
свідомий вибір (безпека, довіра, простота).

## Quickstart

```sh
cp .env.example .env
# Заповни TG_TOKEN, TICKET_SECRET, MONO_JAR_LINK — мінімум, щоб стартувати.
docker compose up --build -d
```

Сервіс підняв `localhost:8093`:

- `/` — лендінг (порожній, поки не створиш першу подію в адмінці)
- `/admin/login` — вхід в адмінку
- `/event/<slug>` — сторінка покупця
- `/scan` — сканер на вході
- `/webhook` — endpoint для monobank
- `/health`, `/debug/vars` — операційні

Перший адмін — через `ADMIN_EMAIL` + `ADMIN_PASSWORD` в `.env` (one-shot,
прибрати після першого старту). Далі — увійшов, натиснув "+ Нова подія",
вписав назву/дату/зал → готово, лендінг показує плитку, бот її бачить.

## Як це працює

1. **Адмін** заходить у `/admin/login`, створює подію (назва, дата, зал
   N×M, ціна по дефолту). Може завантажити постер (multipart upload до
   5 МБ), вписати опис, редагувати кожне місце (drag&drop, ціна,
   категорія, sellable on/off).
2. Подія одразу видима на `/` і в боті — обидва підтягують список
   non-archived shows.
3. **Покупець** (web): відкриває `/event/<slug>`, тицяє вільні місця
   (одне або кілька, до 20), вписує ім'я та email, тисне «Забронювати».
   Сервер створює order — один платіжний код на N місць, кожне місце
   притримується на 15 хв (`HOLD`).
4. Фронт показує посилання на банку моно з прошитою сумою і коментарем,
   паралельно поллить `/api/public/reservations/<code>/status` кожні 5с.
5. **Покупець** (бот): `/start` → афіша → подія → «📋 Обрати місце» →
   inline-клавіатура з усіма місцями (✓ = в кошику, ✖ = продано, … =
   в чужому холді). Тапає, скільки треба, тисне «✅ Завершити (N · сума)»,
   вводить ім'я → бот віддає `Оплатити` button з тим самим pay-link.
   Альтернатива — кнопка `🗺 Відкрити мапу залу` як Telegram WebApp, що
   піднімає той самий `/event/<slug>` усередині Telegram (якщо заданий
   `BASE_URL`).
6. **Покупець** платить у моно. Коментар вже вписаний — переплутати
   неможливо.
7. **monobank** шле webhook → `pay.Processor` матчить код → ConfirmOrder
   → генерує по одному PDF на місце (A6, HMAC-підписаний QR) → шле всі
   у Telegram (якщо buyer прив'язав чат) і одним листом на email (якщо
   налаштований SMTP).
8. За годину до старту — нагадування у TG (один раз; `reminded_at`
   страхує від дублів після рестарту).
9. На вході людина відкриває `/scan?token=…` → сканер у браузері →
   зелений «✓ ПРОХОДЬ» / оранжевий «⏱ Вже використано» / червоний
   «✗ Недійсний».

**Admin cancel**: якщо адмін у веб-панелі скасовує бронь — order
скасовується цілком (усі місця), покупець отримує сповіщення в TG (якщо
прив'язував) і на email (якщо вписав).

## Архітектура

```
cmd/app/             # main: config, store, wiring бот+pay+admin+public+scan+SMTP+posters
internal/
  config/            # ENV → Config
  store/             # SQLite, migrations, CRUD: shows, seats, reservations, orders, tickets, users, sessions
  token/             # 8-символьний код базою 32 + HMAC-підписаний QR payload
  bot/               # Telegram: афіша, multi-seat picker, deep-link /start res_<code>, /my, /stats, /reconcile, /jar
  pay/               # monobank statement → match order → confirm → render N PDFs → deliver
  public/            # /api/public/*: список подій, деталі події, POST orders (multi), GET status
  admin/             # /api/admin/*: CRUD shows, seats batch save, guests, force-cancel, login/logout, me
  auth/              # bcrypt sessions, RequireAuth middleware (303 для browser, 401 для API)
  email/             # SMTP via net/smtp, handmade MIME multipart, N attachments per message
  posters/           # multipart upload (5 МБ cap), DetectContentType validation, static serve
  ticket/            # PDF A6 з вбудованим DejaVu Sans + QR
  web/               # сторінка сканера + POST /scan/check + per-IP rate-limit + cookie auth
  webui/             # embed.FS handler для Svelte SPA (cache-control immutable для /_app/immutable/*)
  metrics/           # expvar-лічильники (квитки, помилки вебхука, скани) — без зовнішніх залежностей
  timefmt/           # український форматер дати
  e2e/               # cross-package integration (store+pay+token+web+order flow)
frontend/            # SvelteKit 2 + Svelte 5 + Tailwind v4, adapter-static → internal/webui/dist
```

Внутрішні пакети **не імпортують один одного** — кожен оголошує свої
типи і потрібні йому інтерфейси (`Store`, `Coder`, `Notifier`, `Renderer`,
`EmailDelivery`). `cmd/app/main.go` тримає всі адаптери і робить wiring.
Будь-який пакет тривіально тестується ізольовано і будь-яку залежність
можна підмінити на фейк.

## Конфіг

Всі параметри — через ENV. У dev можна покласти `.env` поруч із бінарем,
godotenv підхопить.

### Обов'язкові

| змінна | що це |
|---|---|
| `TG_TOKEN` | токен бота від @BotFather |
| `TICKET_SECRET` | секрет HMAC для підпису QR; будь-який рядок ≥32 символів. **Не змінюй після випуску квитків** — старі QR стануть невалідними |
| `MONO_JAR_LINK` | посилання на банку моно (`https://send.monobank.ua/jar/...`), до якої клеїться `?a=<сума>&t=<код>` |

### Опційні

| змінна | за замовч. | що це |
|---|---|---|
| `HTTP_ADDR` | `:8093` | порт для всього: webhook, admin, public, scan, SPA |
| `DB_PATH` | `tix.db` | SQLite-файл (`/data/tix.db` усередині контейнера) |
| `BASE_URL` | (порожньо) | публічний URL інстансу; вмикає Telegram WebApp кнопку в боті |
| `BOT_USERNAME` | (порожньо) | @-handle бота без `@`; вмикає deep-link `t.me/...?start=res_<code>` на success-екрані web |
| `MONO_TOKEN` | (порожньо) | токен monobank Personal API; вмикає `/reconcile` і авто-реєстрацію вебхуку |
| `WEBHOOK_URL` | (порожньо) | публічний URL вебхуку; разом із `MONO_TOKEN` змушує бот при старті викликати `POST /personal/webhook` |
| `SCANNER_TOKEN` | (порожньо) | shared-секрет для `/scan`; порожнє = доступ без авторизації |
| `ADMIN_TG_ID` | `0` | TG user id адміна; йому доступні `/stats`, `/reconcile`, `/jar` в боті |
| `ADMIN_EMAIL` | (порожньо) | one-shot bootstrap: створює першого admin web-юзера; після першого запуску прибрати з `.env` |
| `ADMIN_PASSWORD` | (порожньо) | пара до `ADMIN_EMAIL` |
| `SECURE_COOKIES` | `false` | `true` форсує `Secure` cookie у проді (за HTTPS) |
| `HOLD` | `15m` | скільки бронь живе до оплати |
| `REMIND_BEFORE` | `1h` | за скільки до старту слати нагадування |
| `POSTERS_DIR` | `posters` (locally) / `/data/posters` (Docker compose) | де лежать завантажені афіші |
| `BACKEND_HOST_PORT` | `8093` | published port на хості (docker compose) |
| `CLOUDFLARED_TOKEN` | (порожньо) | токен Cloudflare Tunnel; активний лише з `--profile tunnel` |

### SMTP (для email-доставки)

Без цих змінних web-покупці все одно зможуть оплатити, просто PDF на
email не прилетить (бот-flow не торкається).

| змінна | за замовч. | що це |
|---|---|---|
| `SMTP_HOST` | (порожньо) | напр. `smtp.gmail.com`, `smtp.resend.com` |
| `SMTP_PORT` | `587` | 587 для STARTTLS, 465 для implicit TLS |
| `SMTP_USER` | (порожньо) | логін SMTP |
| `SMTP_PASS` | (порожньо) | пароль / API-key |
| `SMTP_FROM` | (порожньо) | From-адреса (рекомендується мати SPF/DKIM) |
| `SMTP_IMPLICIT_TLS` | `false` | `true` для портів типу 465 |

### Initial show (тільки якщо БД порожня)

Якщо при першому старті в БД ще немає жодної події, monokasa засіває
одну з цих параметрів. Після цього адмін керує подіями через веб —
ці змінні ігноруються.

| змінна | за замовч. |
|---|---|
| `SHOW_TITLE` | `Моя вистава` |
| `SHOW_VENUE` | `Театральна площа` |
| `SHOW_STARTS_AT` | `2026-06-01T19:00:00+03:00` (RFC3339) |
| `ROWS` / `COLS` | `5` / `6` |
| `PRICE_KOPECKS` | `25000` (250.00 ₴) |

## Запуск

### Docker (рекомендовано)

```sh
cp .env.example .env       # заповни обов'язкові
docker compose up --build -d
```

Один контейнер `backend` на `localhost:8093` (`BACKEND_HOST_PORT`):
- Telegram-бот (long-polling)
- HTTP: `/webhook`, `/admin/*`, `/api/*`, `/scan`, `/health`, `/debug/vars`, `/*` (Svelte SPA)

`/data` — volume для SQLite і афіш. distroless образ, `nonroot` user.

### Cloudflare Tunnel (публічний HTTPS без власного домена)

Webhook monobank вимагає публічний HTTPS endpoint. Без власного домена
і VPS з TLS — підніми безкоштовний tunnel:

1. Зареєструйся на [Cloudflare Zero Trust](https://dash.cloudflare.com).
2. `Access → Tunnels → Create a tunnel` → connector `Cloudflared` → скопіюй токен.
3. Додай у `.env`: `CLOUDFLARED_TOKEN=eyJ...`
4. У панелі CF додай public hostname (`<щось>.trycloudflare.com` або
   свій домен) → service `http://backend:8093`.
5. `docker compose --profile tunnel up -d`

Тепер вебхук monobank можна реєструвати на цей URL і поставити в `.env`:
```
WEBHOOK_URL=https://<щось>.trycloudflare.com/webhook
BASE_URL=https://<щось>.trycloudflare.com
```

Бот при старті сам викличе `POST /personal/webhook` (якщо є `MONO_TOKEN`).

### Локальний dev без Docker

```sh
# Frontend збирається окремо (інакше "/" віддасть stub-сторінку):
cd frontend && npm install && npm run build
rm -rf ../internal/webui/dist && cp -r build ../internal/webui/dist
cd ..

go build -o monokasa ./cmd/app
./monokasa
```

HMR-loop для фронту: `cd frontend && npm run dev` (на 5173) + бекенд на
8093; API виклики йдуть на той самий origin через Vite proxy.

Реєстрація вебхука вручну (без `MONO_TOKEN` у проді):

```sh
curl -X POST https://api.monobank.ua/personal/webhook \
     -H "X-Token: $MONO_TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"webHookUrl":"https://your.host/webhook"}'
```

### Make-цілі

`Makefile` має зручні таргети для dev: `make build` (frontend → embed
→ go build), `make run`, `make test`, `make clean`.

## Admin web

`/admin/login` — bcrypt-сесія в HttpOnly cookie (`monokasa_admin`,
Path=/, SameSite=Lax, MaxAge=30 днів). Logout (`POST /admin/logout`)
видаляє сесію в БД.

Перший адмін: задаєш `ADMIN_EMAIL` + `ADMIN_PASSWORD` перед першим
стартом. Бот побачить порожню `users` → забекриптить пароль (cost 12)
→ збереже → виведе попередження з проханням прибрати ці ENV. Далі —
БД джерело правди.

Що вміє адмін у веб:
- **Список подій**: статистика по кожній (продано / тримається / вільно / виторг).
- **Створити подію**: title, venue, дата, зал N×M, дефолтна ціна.
- **Редагувати подію**: title, venue, дата, опис, постер (file upload до 5 МБ).
- **Редактор залу**: drag&drop місць по сітці, ціна, категорія,
  toggle `sellable`. Зміни батчем (PATCH).
- **Гості**: список бронювань події з фільтром по статусу, force-cancel
  (cascade на весь order → сповіщення покупцю в TG + email).
- **Archive**: ховає подію з лендінгу і бота (БД не чистить).

## Команди бота

Покупець:
- `/start`, `/start res_<code>` — афіша / прив'язка web-броні до чату.
- `/events`, `/seats` — список подій.
- `/my` — мої бронювання зі статусами і кодами.

Адмін (`ADMIN_TG_ID`):
- `/stats` — продажі по поточній активній події.
- `/reconcile [тривалість]` — догрести пропущені оплати через
  `/personal/statement` (default 24h, можна `48h`, `7d`). Потрібен
  `MONO_TOKEN`. Mono лімітує 1 запит/60s, тому довге вікно займе хвилину.
- `/jar` — баланс і ціль банки моно.

`/reconcile` — рятівна сітка, якщо webhook загубився (downtime, рестарт,
неправильний URL). Проганяє транзакції через ту саму гілку
`pay.Processor`, тому подвійно нічого не підтвердиться: `ConfirmOrder`
ідемпотентний.

## Сканер на вході

Відкрий `https://your.host/scan`. Якщо `SCANNER_TOKEN` заданий —
покажеться форма з полем «пароль». Введи `SCANNER_TOKEN` → отримуєш
HttpOnly cookie (`Path=/scan`, 12 годин) → потрапляєш у fullscreen-сканер.
Якщо `SCANNER_TOKEN` порожній — форма не показується.

Перебір паролю обмежений per-IP rate-limit (10 спроб/с), той самий, що
на `/scan/check`.

Сканер працює в Safari/Chrome (на iOS — лише Safari, не in-app браузер
Telegram), вимагає камеру, далі:
- зелений «✓ ПРОХОДЬ» + м'який акорд → пускати;
- оранжевий «⏱ Вже використано» + один тон + потрійна вібрація + час
  першого сканування → не пускати, з'ясовувати;
- червоний «✗ Недійсний» + різкий тон → підробка / чужий QR.

QR парситься у браузері (`jsQR` через CDN), на сервер летить лише
payload-рядок, який перевіряється HMAC-ом і одноразово погашається в БД.

## Tests

```sh
go test ./...
```

Покрито найризикованіше:
- HMAC roundtrip і tamper-detection (`internal/token`);
- парсер коду з вільного коментаря і ціни (`internal/pay`);
- валідація імені / email (`internal/public`, `internal/bot`);
- побудова URL для банки моно;
- інтеграційні `store` на справжньому SQLite: race на місце,
  multi-seat orders, скасування + cascade, подвійний confirm/use,
  статуси, статистика, reminder-flow;
- e2e: webhook → pay → email → scan flow з фейковими SMTP/TG.

## База даних

Один SQLite-файл (`tix.db` або `/data/tix.db`), `WAL` ввімкнено через
DSN. Схема в `internal/store/store.go`: `shows`, `seats`, `reservations`,
`orders`, `tickets`, `users`, `sessions`. Часи лежать unix-секундами —
зручно колупати через `sqlite3 ... '.headers on' 'select datetime(created_at, "unixepoch") from orders'`.

Міграції — idempotent (`ALTER … ADD COLUMN`, `INSERT OR IGNORE`,
`UPDATE … WHERE x IS NULL`). Можна боятися мінорних апдейтів менше.

Бекап: `cp tix.db tix.db.bak` під час непікової години. Або
`sqlite3 tix.db ".backup tix.db.bak"` — не блокує писемні запити.

## Troubleshooting

**Оплата пройшла, а PDF не прийшов.** Найімовірніше, webhook не дійшов
(downtime / неправильний URL / monobank не дотиснувся). Перевір логи на
`mono webhook ready` при старті і на `webhook: …` помилки. Дієве —
`/reconcile 2h` від адмін-акаунта. Якщо `MONO_TOKEN` не виставлений,
`/reconcile` не активується.

**Web-покупець оплатив, але сторінка не оновилася.** Поллінг тримається
30 хв, оновлення раз на 5 с. Якщо за 30 хв webhook не прилетів — статус
залишиться `held` → через `HOLD` `expired`. Тоді треба `/reconcile`.

**Email не приходить.** Перевір `SMTP_HOST/PORT/USER/PASS/FROM` —
порожній `SMTP_HOST` = email вимкнено мовчки. Лог при відправці пише
`email send …`. Gmail: app-password, не звичайний.

**Адмін cancel — покупець не отримав сповіщення.** Якщо покупець
бронював через бот або прив'язав TG через deep-link — TG. Якщо вписав
email і SMTP налаштований — лист. Якщо ні те, ні інше — admin відмінив
тихо (без каналу зв'язку).

**Сканер не бачить камеру.** Браузер віддає камеру тільки на HTTPS
(виняток — `localhost`). Переконайся, що `/scan` через https. На iOS —
лише Safari, не in-app браузер Telegram.

**QR сканується, але «✗ Недійсний».** HMAC не зійшовся: або змінили
`TICKET_SECRET` після випуску, або QR з іншого інстансу.

**Бот не реагує.** Перевір лог на `telegram bot up`. Якщо немає —
`TG_TOKEN` невалідний. Якщо є, але `/stats` мовчить — не той
`ADMIN_TG_ID` (звір з тим, що шле `@userinfobot`).

**`/reconcile` довго думає.** Нормально: monobank лімітує
`/personal/statement` 1 запит/60s на акаунт. Якщо в `ClientInfo`
кілька акаунтів — буде хвилину-дві.

**`SQLITE_CANTOPEN` у контейнері після оновлення.** Старий volume
залишив `/data` із root-овнерством, а distroless контейнер тепер
ходить як `nonroot`. Лікується одним `docker compose down -v` + старт
наново (volume пересоздасться з правильним owner).

## Що свідомо не зроблено

- **Multi-tenant SaaS**: один інстанс = один organizer. Платежі йдуть
  на банку organizer'а напряму — у нас немає чужих коштів, чужих
  персональних даних і атаки на чужу довіру.
- **Повернення коштів**: рефанди — руками через моно. На пропущений
  webhook є `/reconcile`.
- **Платіжний шлюз / еквайринг**: розрахунок іде на банку моно
  напряму, без проміжного API.
- **Авторизація сканера складніша за shared-token**: достатньо для
  персоналу, який має фізичний доступ до камери при вході.
- **Live updates через SSE**: зараз web-фронт поллить статус кожні 5с;
  SSE-канал на seat-зміни запланований, але не критичний.
- **Telegram-Stars / built-in checkout**: monobank-jar дає більший
  поріг купюр без верифікації і ходить напряму на твою банку.

Якщо щось із цього стає потрібним — структура така, що додається без
ламання решти.

## Ліцензія

MIT.
