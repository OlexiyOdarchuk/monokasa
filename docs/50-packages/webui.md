# 50 · Package: webui

Embed.FS handler для скомпільованого Svelte SPA. Один маленький файл,
але важливе склеювання.

## Public API

```go
//go:embed all:dist
var distFS embed.FS

func NewHandler() (http.Handler, error)
```

## Як це працює

1. Svelte збирається в `frontend/build/` через `adapter-static`.
2. Скрипт (`make frontend` / Dockerfile) копіює `frontend/build/` →
   `internal/webui/dist/`.
3. `go build` компілює `dist/` у бінарник через `embed.FS`.
4. У рантаймі `webui.NewHandler()` повертає `http.FileServer`-подібний
   handler, що віддає файли з embed.

## Cache headers

- `/_app/immutable/*` (hashed assets від Vite) → `Cache-Control: public, max-age=31536000, immutable`
- Усе інше (index.html) → `Cache-Control: no-cache` (треба перевіряти при
  кожному запиті, бо asset hashes міняються при rebuild)

## SPA-fallback

Невідомий path під `/` → віддаємо `index.html` (бо SvelteKit
client-side routing). На сервері існують реальні маршрути типу
`/event/<slug>` — їх перехоплює embed handler і шле index.html, далі
браузер виконує Svelte client-side router.

`/api/*`, `/admin/*`, `/posters/*`, `/scan*`, `/webhook`, `/health`,
`/debug/vars` — реєструються в `mux` ПЕРЕД webui handler'ом, тому SPA
fallback не перехоплює.

## Локальний dev

Без Docker:
```sh
cd frontend && npm install && npm run build
rm -rf ../internal/webui/dist && cp -r build ../internal/webui/dist
cd .. && go build ./...
```

Або просто `make build` (робить це автоматично).

Stub `dist/index.html` лишається в git, щоб `go build` працював на
свіжому клоні без npm.

## Файли

- `webui.go` — все ~80 рядків
- `dist/index.html` — stub у git; інше gitignored і регенерується

## Дотичне

- [[60-deploy]] — як це збирається у Docker / make
