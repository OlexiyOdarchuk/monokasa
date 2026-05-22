# ADR · Scanner як Telegram WebApp (поруч з shared-token cookie)

## Контекст

`/scan` — це сторінка для людини на вході (doorman, organizer). Камера
+ jsQR → POST `/scan/check` з QR payload → погашення. До цього доступ
гейтнувся shared `SCANNER_TOKEN`: organizer ділиться URL з токеном у
cookie, persona-staff відкриває посилання і сканує.

Проблеми shared token'у:
- довгоживий, легко витікає (screenshot, забутий tab у браузері
  пристрою з кількома користувачами)
- немає індивідуальної identity → audit не знає хто сканував
- незручно ділитися — треба окремий месенжер/email

Питання: дати organizer'у альтернативу через бот.

## Рішення

**WebApp кнопка «🔍 Сканер квитків (адмін)»** у `/start` меню бота,
видима тільки для `ADMIN_TG_ID`. Тапнув → відкривається `/scan?tg=1`
як Telegram Mini App, init data Telegram'а підписана bot token'ом
служить другим auth path поруч з shared-cookie.

### Як вериф

Telegram Mini App SDK кладе підписаний blob у
`window.Telegram.WebApp.initData`. Це query-string-encoded поля
(user, auth_date, …) плюс `hash`. Server відтворює:
1. Сортує поля key=value по ключу, з'єднує `\n`.
2. `secret_key = HMAC-SHA256(key="WebAppData", data=botToken)`.
3. `expected = hex(HMAC-SHA256(key=secret_key, data=sortedString))`.
4. `expected == hash` → blob справжній. Read `user.id`, check проти
   allow-list.

Bot token живе тільки на сервері; підробити підпис без нього
неможливо.

### Що змінилось

- `internal/web/tgwebapp.go` — `VerifyTelegramInitData(initData, botToken)`
  повертає user.id або err. 4 unit-теста: signature OK, mismatch,
  missing hash, missing user.
- `Scanner.EnableTelegramWebApp(botToken, []adminTGID)` — вмикає шлях.
  Викликається з main.go якщо `TG_TOKEN + ADMIN_TG_ID` задані.
- `Scanner.authOK` додав 3-й шлях: `X-Telegram-Init-Data` header
  → verify + user-id-in-allow-list.
- `GET /scan?tg=1` сервить scanner page без password gate (auth
  все одно на `/scan/check`).
- Bot's `sendEventList` додає WebApp button для `c.Sender().ID == AdminTGID`
  з URL `BASE_URL/scan?tg=1`. Потрібен https BASE_URL (TG вимога).
- Scanner page завантажує `telegram-web-app.js` (no-op поза TG),
  читає `initData` і шле в header на кожен `/scan/check` fetch.

## Trade-offs

| Проблема | Mitigation |
|---|---|
| Mini App відкривається тільки в TG | Cookie/password path лишається — це fallback для browser-only doorman'ів |
| Bot token leak = forge any user | Token єдиний для бота; вже секрет; крадіжа = catastrophic anyway |
| Init data has 24h replay window | Mono caller-side concern; ми перевіряємо `user.id` allow-list, мала вікно replay не дає бекдору поза admin scope |
| Single admin allow-list | `EnableTelegramWebApp([]int64{...})` приймає масив — додати helper у конфіг (ADMIN_TG_IDS, comma-separated) тривіально, коли треба декілька doorman'ів |

## Альтернативи що НЕ обрали

- **Одноразовий signed link через бота**: складніше (треба новий
  endpoint `/scan/grant`, time-limited tokens). WebApp простіший і
  «природніший» з TG-side.
- **Тільки WebApp, прибрати password**: ламає browser-only fallback
  для doorman'ів без TG.

## Дотичне

- [[50-packages/web]] — Scanner code
- [[30-endpoints]] — `/scan?tg=1`
- Telegram docs: https://core.telegram.org/bots/webapps#validating-data-received-via-the-web-app
