package main

import (
	"bytes"
	"fmt"
	"image/png"
	"time"

	"github.com/jung-kurt/gofpdf"
	"github.com/skip2/go-qrcode"
)

// RenderTicketPDF returns the bytes of a single-page A6 ticket containing
// show details and the signed QR code. No external fonts — gofpdf's bundled
// Arial covers Latin; for Cyrillic we register DejaVu if available, but
// fallback gracefully to the ASCII transliteration.
func RenderTicketPDF(show Show, seat Seat, qrPayload string) ([]byte, error) {
	q, err := qrcode.New(qrPayload, qrcode.Medium)
	if err != nil {
		return nil, fmt.Errorf("qrcode: %w", err)
	}
	q.DisableBorder = false
	qrImg := q.Image(256)

	var qrBuf bytes.Buffer
	if err := png.Encode(&qrBuf, qrImg); err != nil {
		return nil, fmt.Errorf("encode qr: %w", err)
	}

	pdf := gofpdf.NewCustom(&gofpdf.InitType{
		UnitStr:        "mm",
		Size:           gofpdf.SizeType{Wd: 105, Ht: 148}, // A6 portrait
		OrientationStr: "P",
	})
	pdf.SetMargins(10, 12, 10)
	pdf.AddPage()

	// Try to register a Cyrillic-capable font; fall back to Helvetica if missing.
	if pdf.Ok() {
		pdf.SetFont("Helvetica", "B", 16)
	}

	pdf.CellFormat(0, 10, ascii(show.Title), "", 1, "C", false, 0, "")
	pdf.SetFont("Helvetica", "", 11)
	pdf.CellFormat(0, 6, ascii(show.Venue), "", 1, "C", false, 0, "")
	pdf.CellFormat(0, 6, show.StartsAt.Format("Mon 2 Jan 2006, 15:04"), "", 1, "C", false, 0, "")

	pdf.Ln(4)
	pdf.SetFont("Helvetica", "B", 24)
	seatStr := fmt.Sprintf("Row %d  Seat %d", seat.Row, seat.Col)
	pdf.CellFormat(0, 14, seatStr, "", 1, "C", false, 0, "")

	pdf.Ln(2)

	imgOpts := gofpdf.ImageOptions{ImageType: "PNG", ReadDpi: false}
	pdf.RegisterImageOptionsReader("qr", imgOpts, &qrBuf)
	const qrSizeMM = 60
	pageWidth, _ := pdf.GetPageSize()
	x := (pageWidth - qrSizeMM) / 2
	pdf.ImageOptions("qr", x, pdf.GetY(), qrSizeMM, qrSizeMM, true, imgOpts, 0, "")

	pdf.SetY(-22)
	pdf.SetFont("Helvetica", "I", 8)
	pdf.CellFormat(0, 4, fmt.Sprintf("Issued %s", time.Now().Format(time.RFC3339)), "", 1, "C", false, 0, "")
	pdf.CellFormat(0, 4, "Show this QR at the entrance.", "", 1, "C", false, 0, "")

	var out bytes.Buffer
	if err := pdf.Output(&out); err != nil {
		return nil, fmt.Errorf("pdf output: %w", err)
	}
	return out.Bytes(), nil
}

// ascii downgrades Cyrillic to a best-effort Latin transliteration so the
// bundled Helvetica font can render it without needing a custom .ttf.
func ascii(s string) string {
	var b []rune
	for _, r := range s {
		if v, ok := translit[r]; ok {
			b = append(b, []rune(v)...)
			continue
		}
		if r < 128 {
			b = append(b, r)
		} else {
			b = append(b, '?')
		}
	}
	return string(b)
}

var translit = map[rune]string{
	'А': "A", 'Б': "B", 'В': "V", 'Г': "H", 'Ґ': "G", 'Д': "D", 'Е': "E",
	'Є': "Ye", 'Ж': "Zh", 'З': "Z", 'И': "Y", 'І': "I", 'Ї': "Yi", 'Й': "Y",
	'К': "K", 'Л': "L", 'М': "M", 'Н': "N", 'О': "O", 'П': "P", 'Р': "R",
	'С': "S", 'Т': "T", 'У': "U", 'Ф': "F", 'Х': "Kh", 'Ц': "Ts", 'Ч': "Ch",
	'Ш': "Sh", 'Щ': "Shch", 'Ь': "", 'Ю': "Yu", 'Я': "Ya",
	'а': "a", 'б': "b", 'в': "v", 'г': "h", 'ґ': "g", 'д': "d", 'е': "e",
	'є': "ye", 'ж': "zh", 'з': "z", 'и': "y", 'і': "i", 'ї': "yi", 'й': "y",
	'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r",
	'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch",
	'ш': "sh", 'щ': "shch", 'ь': "", 'ю': "yu", 'я': "ya",
	'’': "'", '«': "\"", '»': "\"", '—': "-", '–': "-",
}
