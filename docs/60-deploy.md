# 60 · Deploy

## Шляхи деплою

1. **Docker compose** (рекомендовано) — один файл, один контейнер.
2. **Cloudflared tunnel** (опційно, в тому ж compose) — публічний HTTPS
   без власного домена.
3. **Make + native binary** (для розробки / специфічних кейсів).

## Docker compose

Файл `docker-compose.yml` тримає одну службу `backend` + опційний
`cloudflared` під profile `tunnel`.

### Звичайний старт

```sh
cp .env.example .env
# вписати TG_TOKEN, TICKET_SECRET, MONO_JAR_LINK (мінімум)
docker compose up --build -d
```

- Backend слухає всередині контейнера `:8093`, маппиться на хост
  `${BACKEND_HOST_PORT:-8093}`.
- SQLite + posters у named volume `db_data` → `/data` усередині.
- Image — distroless static nonroot. Жодного shell.

### Tunnel profile

```sh
docker compose --profile tunnel up -d
```

- Додатково піднімається `cloudflared` контейнер з `TUNNEL_TOKEN`
  (= `${CLOUDFLARED_TOKEN}` із .env).
- У панелі Cloudflare маршрутизуй public hostname на
  `http://backend:8093` (service name всередині compose).

### Стоп / очистка

```sh
docker compose down            # БД лишається
docker compose down -v         # повний reset: видаляє volume db_data
```

## Dockerfile (3 стадії)

```
Stage 1: node:lts (alpine)
  → cd frontend && npm install && npm run build
  → cp -r build /shared/dist

Stage 2: golang:1.x
  → COPY backend code + shared/dist → internal/webui/dist
  → go build -trimpath -ldflags="-s -w" -o /out/monokasa ./cmd/app
  → mkdir /out/data && chown -R 65532:65532 /out/data
    (nonroot UID — інакше SQLite не зможе писати в named volume)

Stage 3: gcr.io/distroless/static-debian12:nonroot
  → COPY --from=backend /out/monokasa /monokasa
  → COPY --from=backend --chown=nonroot:nonroot /out/data /data
  → EXPOSE 8093
  → USER nonroot:nonroot
  → ENTRYPOINT ["/monokasa"]
```

Image: ~30 МБ, zero shell, zero package manager → нічого ламати з SSH.

## ENV reference

Повний список з дефолтами → [README.md](../README.md#конфіг).

Особливі моменти для прода:
- `BASE_URL=https://your.host` — інакше Telegram WebApp кнопка не
  з'явиться, тільки in-chat picker.
- `BOT_USERNAME=mybot` (без @) — інакше public success-екран не
  покаже "Підключити Telegram" кнопку.
- `SMTP_*` — без них web-buyer email-доставки нема (всі бот-flow
  працюють).
- `SECURE_COOKIES=true` за HTTPS-проксі. App також авто-детектує
  `X-Forwarded-Proto: https`, тому для cloudflared це переважно
  zero-config.

## Native binary (без Docker)

```sh
make build      # frontend + go build → ./monokasa
./monokasa      # читає ENV з .env (godotenv)
```

Або руками:
```sh
cd frontend && npm install && npm run build
rm -rf ../internal/webui/dist && cp -r build ../internal/webui/dist
cd .. && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o monokasa ./cmd/app
./monokasa
```

`CGO_ENABLED=0` — `modernc.org/sqlite` чистий Go, тому можна без libc;
бінарник запускається в distroless / scratch / Alpine без додаткових
залежностей.

## Webhook реєстрація

Якщо є `MONO_TOKEN` + `WEBHOOK_URL` — main.go при старті викликає
`POST /personal/webhook` з exponential-backoff retry (5 спроб, ~30 с
сумарно). Mono GET-пінгає URL перед прийняттям підписки, тому retry
переживає повільне піднімання cloudflared.

Без `MONO_TOKEN` — реєструй вручну:
```sh
curl -X POST https://api.monobank.ua/personal/webhook \
     -H "X-Token: $MONO_TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"webHookUrl":"https://your.host/webhook"}'
```

## Acquiring (опційно)

Для шоу з `payment_method='acquiring'` — реальний merchant invoice
замість jar prefill. Дві річі треба:

1. **`MONO_ACQUIRING_TOKEN`** в env — окремий від `MONO_TOKEN`, видається
   monobank business support (https://api.monobank.ua/docs/acquiring.html).
2. **`BASE_URL`** — обов'язково https; інакше `redirectUrl` / `webHookUrl`
   будуть невалідні і monobank відмовиться створювати invoice.

Webhook реєструється на monobank-side не нашим запитом — banking
sysadmin вписує `https://your.host/webhook/acquiring` у налаштуваннях
мерчанта. Підпис верифіковано через `/api/merchant/pubkey` (lazy-fetch +
in-memory cache; ротація ключа = restart процесу).

Перемикати методи можна per-show через admin UI (`PATCH /api/admin/shows/{id}`
з `payment_method`). Старі шоу залишаються на jar; нові можна одразу
ставити на acquiring без міграції.

## Backup

Один файл — `tix.db`:
```sh
sqlite3 /data/tix.db ".backup /backups/tix-$(date +%F).db"
```
Робиться без зупинки сервісу (WAL mode дозволяє).

Постери (`/data/posters/`) — звичайний rsync.

## Troubleshooting

Базові кейси є в README ([Troubleshooting](../README.md#troubleshooting)).

Найчастіше:
- **`SQLITE_CANTOPEN`** після оновлення з ранніх версій → `docker compose down -v` (старий volume з root-овнерством).
- **Webhook timing out** → cloudflared ще піднімається; зачекати 30 с,
  або переглянути логи `cloudflared` в compose.
- **Email не йде** → перевір `SMTP_HOST/USER/PASS/FROM`; Gmail вимагає
  app-password.
- **PDF не приходить у Telegram** → `BuyerEmail` пустий + чат не
  прив'язаний; перевір `BOT_USERNAME` + чи відкривав buyer deep link.

## Дотичне

- README.md — швидкий старт
- [[40-flows/webhook]] — як обробляється платіж після того, як moнo
  дотиснувся
