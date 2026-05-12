module github.com/OlexiyOdarchuk/mono-tix

go 1.26.3

require (
	github.com/joho/godotenv v1.5.1
	github.com/jung-kurt/gofpdf v1.16.2
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	github.com/vtopc/go-monobank v0.0.0-00010101000000-000000000000
	gopkg.in/telebot.v3 v3.3.8
	modernc.org/sqlite v1.50.1
)

require (
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.1 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/vtopc/epoch v1.3.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/vtopc/go-monobank => github.com/OlexiyOdarchuk/go-monobank v0.22.1-0.20260512224841-21bfa58795db
