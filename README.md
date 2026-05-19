# monokasa

[![Built with Claude Code](https://img.shields.io/badge/Built%20with-Claude%20Code-D97757?logo=anthropic&logoColor=white)](https://claude.com/claude-code)

Telegram-бот, що продає квитки на одну виставу через монобанку, видає
квитки у вигляді PDF з QR-кодом і має веб-сторінку-сканер для контролю
на вході.

Один захід, один зал, один show в базі. Ніяких касирів, особистого
кабінету чи інтеграції з еквайрингом — це MVP для разової події, який
можна підняти на найдешевшому VPS за вечір.

## Quickstart

Мінімум, щоб бот ожив:

```sh
cat > .env <<'EOF'
TG_TOKEN=123:abc...           # від @BotFather
TICKET_SECRET=$(openssl rand -hex 32)
MONO_JAR_LINK=https://send.monobank.ua/jar/abc123
EOF

go build -o monokasa ./cmd/app
./monokasa
```

Бот стартує, створює `tix.db`, заповнює зал 5×6 по 250.00 ₴ і слухає
`:8090`. Далі — пробрось HTTPS до `:8090` (cloudflared/ngrok у dev) і
зареєструй вебхук у моно на `https://your.host/webhook` (див. нижче).
Решта параметрів — у таблиці [Конфіг](#конфіг).

## Як це працює

1. **Користувач** пише боту, бачить мапу місць (`/seats`), тицяє вільне.
2. Бот питає ім'я і прізвище — саме воно піде на квиток.
3. Бот віддає посилання на mono-банку з уже заповненими сумою і
   коротким кодом-коментарем (8 символів base32). Місце притримується
   на 15 хвилин (`HOLD`).
4. Користувач платить у застосунку моно. Коментар відразу вписаний — переплутати
   не вийде.
5. monobank шле вебхук на `/webhook`. Бот матчить код → бронь, генерує
   PDF-квиток A6 з QR (HMAC-підписаний) і кидає в той самий чат.
6. За годину до вистави всім купленим приходить нагадування (один раз,
   `reminded_at` страхує від дублів після рестарту).
7. На вході людина з телефоном відкриває `/scan?token=…` — це повноекранний
   сканер, який світить зеленим «✓ ПРОХОДЬ», показує ім'я, місце і час
   купівлі; на повторне сканування — оранжеве «⏱ Вже використано».

## Архітектура

```
cmd/app/             # main: завантажує config, відкриває store, склеює бот+web+pay,
                     # адаптери до go-monobank-sdk (webhook, reconcile, jar)
internal/
  config/            # ENV → Config
  store/             # SQLite, схема, міграції, всі CRUD-методи, sweep-expired
  token/             # 8-символьний код + HMAC-підписаний QR payload
  bot/               # Telegram: /seats, /my, /stats, /reconcile, /jar, callback'и, ForceReply для імені
                     # + sweeper, що чистить покинуті pending-picks
  pay/               # обробник monobank statement-події: match → confirm → render → send
                     # використовується і вебхуком, і /reconcile (одна гілка — нуль розбіжностей)
  ticket/            # PDF A6 з вбудованим DejaVu Sans + QR
  web/               # HTML-сторінка сканера + POST /scan/check + per-IP rate-limit + cookie auth
  metrics/           # expvar-лічильники (квитки, помилки вебхука, скани) — лист, без домену
  timefmt/           # український форматер дати (для main)
  e2e/               # cross-package integration test (store+pay+token+web)
```

Внутрішні пакети **не імпортують один одного** — кожен оголошує власні
типи і потрібні йому інтерфейси (`Store`, `Coder`, `Notifier`, `Renderer`).
`cmd/app/main.go` тримає всі адаптери і робить wiring. Завдяки цьому
будь-який пакет тривіально тестується ізольовано і будь-яку залежність
можна підмінити на фейк.

## Конфіг

Всі параметри — через ENV. У dev можна покласти `.env` поруч із бінарем,
godotenv підхопить.

| змінна | за замовч. | що це |
|---|---|---|
| `TG_TOKEN` | — *обов'язково* | токен бота від @BotFather |
| `TICKET_SECRET` | — *обов'язково* | секрет HMAC для підпису QR; будь-який рядок ≥32 символів |
| `MONO_JAR_LINK` | — *обов'язково* | посилання на банку моно, до якої клеїться `?a=…&t=код` |
| `MONO_TOKEN` | (порожньо) | токен monobank Personal API; вмикає `/reconcile` і авто-реєстрацію вебхуку |
| `WEBHOOK_URL` | (порожньо) | публічний URL вебхуку; разом із `MONO_TOKEN` змушує бот при старті викликати `POST /personal/webhook` |
| `SCANNER_TOKEN` | (порожньо) | shared-секрет для `/scan`; порожнє = доступ без авторизації |
| `ADMIN_TG_ID` | `0` | TG user id, якому доступні `/stats`, `/reconcile`, `/jar` |
| `ADMIN_EMAIL` | (порожньо) | one-shot bootstrap: створює першого admin web-юзера при свіжому старті; після першого запуску прибрати з `.env` |
| `ADMIN_PASSWORD` | (порожньо) | пара до `ADMIN_EMAIL`; обидва треба разом, після bootstrap прибрати |
| `SECURE_COOKIES` | `false` | `true` форсує `Secure` cookie за HTTPS у проді; локально лиши `false` |
| `HTTP_ADDR` | `:8090` | порт HTTP-сервера (вебхук + сканер + /health) |
| `DB_PATH` | `tix.db` | де лежатиме SQLite-файл |
| `SHOW_TITLE` | `Моя вистава` | для шапки квитка і повідомлень бота |
| `SHOW_VENUE` | `Театральна площа` | те ж саме |
| `SHOW_STARTS_AT` | `2026-06-01T19:00:00+03:00` | RFC3339 |
| `ROWS` / `COLS` | `5` / `6` | сітка місць; створюється один раз при першому запуску |
| `PRICE_KOPECKS` | `25000` | ціна одного місця в копійках (250.00 ₴) |
| `HOLD` | `15m` | скільки бронь живе до оплати |
| `REMIND_BEFORE` | `1h` | за скільки до старту слати «вже сьогодні о…» |

## Запуск

### Через Docker (рекомендовано)

```sh
cp .env.example .env  # заповни TG_TOKEN, TICKET_SECRET, MONO_JAR_LINK
docker compose up --build
```

Один контейнер `monokasa` на `localhost:8093` — у ньому одразу:
- Telegram-бот (long-polling)
- HTTP-сервер: monobank `/webhook`, `/admin/login`, `/scan`, `/health`,
  `/debug/vars`, і `/*` — вшитий Svelte SPA

Docker build збирає Svelte окремим stage'м і клеїть результат у Go-бінарник
через `embed.FS`, тому у проді — один distroless image, нуль Node-runtime,
нічого додатково подавати.

### Cloudflare Tunnel (публічний HTTPS без власного домена)

Webhook monobank вимагає публічний HTTPS endpoint. Якщо немає власного
домена і VPS з TLS — підніми безкоштовний tunnel:

1. Зареєструйся на [Cloudflare Zero Trust](https://dash.cloudflare.com) (картка не потрібна)
2. `Access → Tunnels → Create a tunnel` → connector `Cloudflared` → копіюй токен
3. Додай у `.env`: `CLOUDFLARED_TOKEN=eyJ...`
4. У панелі Cloudflare додай public hostname (`<щось>.trycloudflare.com` або свій домен) → `http://backend:8093`
5. `docker compose --profile tunnel up`

Тепер вебхук monobank можна реєструвати на цей URL, а в `.env` поставити
`WEBHOOK_URL=https://<щось>.trycloudflare.com/webhook` — бекенд сам
зареєструє його при старті (якщо є `MONO_TOKEN`).

### Без Docker (для розробки бекенду)

```sh
# Зібрати фронт окремо (інакше "/" віддасть stub-сторінку)
cd frontend && npm install && npm run build
rm -rf ../internal/webui/dist && cp -r build ../internal/webui/dist
cd ..

go build -o monokasa ./cmd/app
./monokasa
```

HTTPS для webhook'а — через `cloudflared tunnel` або `ngrok` як раніше.
Якщо просто хочеш dev-loop з HMR — є `frontend/Dockerfile` (запусти
окремо: `cd frontend && docker build -t monokasa-fe . && docker run -p 5173:5173 -v ./src:/app/src monokasa-fe`).

Реєстрацію вебхука можна зробити вручну через monobank API або автоматично —
проставити `MONO_TOKEN` + `WEBHOOK_URL=https://your.host/webhook`, тоді бот
сам викличе `POST /personal/webhook` при старті. Mono GET-пінгає URL перед
тим, як прийняти підписку, тому виклик іде з exponential-backoff retry
(5 спроб, ~30s сумарно), щоб пережити повільне піднімання TLS-фронту.

Якщо не хочеш давати боту `MONO_TOKEN` у проді — зареєструй вручну:

```sh
curl -X POST https://api.monobank.ua/personal/webhook \
     -H "X-Token: $MONO_TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"webHookUrl":"https://your.host/webhook"}'
```

### Службові ендпоінти

- `GET /health` — readiness: 200 `ok`, якщо БД відповідає; 503 при таймауті
  чи помилці пінга SQLite. Підходить для load-balancer health-check.
- `GET /debug/vars` — лічильники `expvar` у JSON-форматі (видані квитки за
  webhook/reconcile, помилки вебхука, OK/used/invalid сканування + поточні
  sold/held/free gauges). Скрейпиться будь-яким Prometheus textfile-exporter.

## Команди бота

Для покупця:

- `/seats` — мапа місць (вільні / зайняті / тимчасово в холді);
- `/my` — список своїх бронювань зі статусами і кодом для оплати;
- `/start`, `/help` — вітання + список доступних команд.

Для адміна (`ADMIN_TG_ID`):

- `/stats` — продажі, виторг, скільки місць ще вільно;
- `/reconcile [тривалість]` — пройти по виписках monobank за вікно (за замовч.
  24h, можна `/reconcile 48h`, `/reconcile 7d`) і вручну підтвердити кожну
  оплату, для якої webhook не дійшов. Потрібен `MONO_TOKEN`. Mono лімітує
  `/personal/statement` 1 запит/60s на акаунт, тому довге вікно займе хвилину-дві;
- `/jar` — баланс і ціль банки моно (резолвиться через `send.monobank.ua`,
  довгий id кешується в пам'яті).

`/reconcile` — це рятувальна сітка на випадок, коли webhook загубився
(downtime, рестарт, перенесення хосту). Проганяє кожну транзакцію через
ту саму гілку `pay.Processor`, що й боєвий хук, тому подвійно нічого не
підтвердиться — `Confirm` ідемпотентний на рівні store.

## Admin web (вхід)

`/admin/login` — форма входу для адміна, який керуватиме подіями через
веб (повноцінний UI прийде в наступному PR; зараз — мінімальна HTML-форма
як doorway до cookie-сесії).

Перший адмін створюється one-shot через ENV: задаєш у `.env`
`ADMIN_EMAIL=you@example.com` і `ADMIN_PASSWORD=<щось>` перед першим
стартом. Бот при старті побачить, що БД ще без юзерів, забекриптить
пароль (bcrypt cost 12), збереже і **виведе попередження в лог** з
проханням прибрати `ADMIN_PASSWORD` з ENV. Після цього перезапуски
ігнорують ці змінні — БД стає джерелом правди.

Логін → HttpOnly cookie `monokasa_admin` (Path=/, SameSite=Lax, MaxAge=30
днів). Logout (`POST /admin/logout`) видаляє сесію в БД і чистить cookie.
Сесії, що протекли, періодично прибираються sweeper'ом — `FindSession`
все одно блокує expired-токени на читанні, sweep лише прибирає мертві
рядки.

## Сканер на вході

Відкрий `https://your.host/scan` у браузері. Якщо `SCANNER_TOKEN` заданий,
сторінка покаже форму з полем «пароль» — введи значення `SCANNER_TOKEN`,
бот видасть HttpOnly-куку (`Path=/scan`, 12 годин життя) і перекине на
сам сканер. Усі подальші POST-и `/scan/check` користуються кукою. Якщо
`SCANNER_TOKEN` порожній — форма не показується, сканер відкривається
одразу. Перебір паролю ловить per-IP rate-limit (10 спроб/с), той самий,
що й на `/scan/check`.

Відкривається в Safari/Chrome, просить камеру, далі fullscreen-режим зі
звуковими сигналами:

- зелений «✓ ПРОХОДЬ» + м'який акорд → людину пускати;
- оранжевий «⏱ Вже використано» + один тон + потрійна вібрація + час
  першого сканування → не пускати, з'ясовувати;
- червоний «✗ Недійсний» + різкий тон → підробка/чужий QR.

Сторінка зчитує QR у браузері (`jsQR` через CDN), на сервер летить лише
рядок payload, який потім перевіряється HMAC-ом і одноразово погашається
в БД.

## Tests

```sh
go test ./...
```

Покрито найризикованіше: HMAC roundtrip і tamper-detection, парсер коду
з вільного коментаря, валідація імені, побудова URL для банки моно, +
інтеграційні тести `store` на справжньому SQLite (race на місце, скасування,
подвійний confirm/use, статуси, статистика, reminder-flow).

## Бази даних

Один SQLite-файл, `WAL` ввімкнено через DSN. Схема в
`internal/store/store.go`: `shows`, `seats`, `reservations`, `tickets`.
Часи лежать unix-секундами — щоб можна було колупати базу через
`sqlite3` + `date(... ,'unixepoch')`.

Бекап: `cp tix.db tix.db.bak` під час непікової години. Або
`sqlite3 tix.db ".backup tix.db.bak"` без зупинки сервісу.

## Troubleshooting

**Оплата пройшла, а PDF не прийшов.** Найімовірніше, webhook не дійшов
(downtime / неправильно зареєстрований URL / monobank не дотиснувся).
Перевір логи на рядок `mono webhook ready, keyId=…` при старті і на
`webhook: …` помилки. Дієве — `/reconcile 2h` від адмін-акаунта: він
прокачає виписку і добʼє все, що було пропущено. Якщо `MONO_TOKEN`
не виставлений, `/reconcile` не активується — додай і перезапусти.

**Сканер не бачить камеру.** Браузер віддає камеру тільки на HTTPS
(виняток — `localhost`). Переконайся, що `/scan` відкритий через https,
а не через IP/HTTP. На iOS — лише Safari, не in-app браузер Telegram.

**QR сканується, але «✗ Недійсний».** Значить HMAC не зійшовся: або
змінили `TICKET_SECRET` після випуску цього квитка, або QR з іншого
інстансу/тестового запуску. Подивись `internal/store/store.go` → таблиця
`tickets`, поле `qr_payload`.

**Бот не реагує на команди.** Перевір, що в логах є `telegram bot up`.
Якщо ні — `TG_TOKEN` невалідний. Якщо так, але `/stats` мовчить — у тебе
не той `ADMIN_TG_ID` (порівняй з тим, що шле `@userinfobot`).

**`/reconcile` довго думає.** Це нормально: monobank лімітує
`/personal/statement` 1 запит/60s на акаунт. Якщо в `ClientInfo`
кілька акаунтів+банок — буде минута-дві.

## Що свідомо не зроблено

- мульти-show: завжди один захід на одну базу;
- особистий кабінет / повернення коштів: усе руками через `sqlite3` і моно
  (на пропущений webhook є `/reconcile`, але рефанди — ручні);
- проміжний платіжний шлюз: розрахунок іде безпосередньо на банку моно;
- авторизація сканера складніша за shared-token;
- метрики/трейсинг: стандартний `log` у stdout.

Якщо щось із цього стає потрібним — структура така, що додається без
ламання решти.

## Ліцензія

MIT.
