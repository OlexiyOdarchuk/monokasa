# 50 · og · Open Graph meta-tag injection

Тонкий пакет: одна-єдина задача — взяти SPA-shell `index.html` і вставити перед `</head>` блок meta-тегів, специфічних для одного шоу. Соц-боти (Telegram, Facebook, Slack, Discord, Twitter) скрейпять URL без JS — їм потрібно, щоб title/description/image лежали прямо в HTML.

## Що рендериться

- `<title>` переписується (а не дублюється) на назву події
- `<meta name="description">` (fallback `<venue> · <date>`)
- `<link rel="canonical">`
- `og:type/site_name/title/description/url/image`
- `twitter:card/title/description/image`

`twitter:card` стає `summary_large_image` коли є poster URL, інакше `summary`.

## API

```go
type Props struct {
    URL         string    // абсолютний https://host/event/<slug>
    Title       string    // show.Title
    Description string    // fallback "<Venue> · <Date>"
    ImageURL    string    // абсолютний; "" — пропускає image-теги
    SiteName    string    // "monokasa" якщо порожнє
    StartsAt    time.Time // для fallback опису
    Venue       string    // для fallback опису
}

func Render(indexHTML []byte, p Props) []byte
func AbsoluteImageURL(baseURL, posterURL string) string
```

- `Render`: повертає **копію** indexHTML з вставленим блоком. Original не модифікується.
- `AbsoluteImageURL`: turn `/posters/abc.jpg` → `https://host/posters/abc.jpg`. External `https://…` пропускаються as-is.

## Як використовується

`cmd/app/main.go` реєструє хендлер `GET /event/{slug}` **до** SPA catch-all'а:

```
mux.HandleFunc("GET /event/{slug}", eventOGHandler(st, spa, cfg.BaseURL))
mux.Handle("/", spa)
```

Хендлер:
1. Лоадить show by slug (2с timeout). Помилка / 404 → falls through на plain SPA shell.
2. Рендерить OG props, вставляє теги через `og.Render`.
3. Віддає з `Cache-Control: public, max-age=60` — швидко переcache'ється після edits.

`origin` береться з `BASE_URL`, fallback — з `r.Host` + scheme (X-Forwarded-Proto враховано).

## Безпека

Усі attribute values проходять `html.EscapeString` — title типу `<script>alert(1)</script>` стане `&lt;script&gt;…`. Це закриває HTML-injection через user-controlled поля шоу.

## Як перевірити

```sh
curl -s https://your.host/event/<slug> | grep -i 'og:'
```

Або вставити URL у Telegram чат — preview-card з'явиться через 1-2с після паблішу повідомлення.

## Дотичне

- [[50-packages/webui]] — звідки беремо `IndexHTML()`
- [[70-decisions/why-self-host]] — чому SPA, не SSR
