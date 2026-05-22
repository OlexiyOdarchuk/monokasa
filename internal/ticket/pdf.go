// Package ticket renders the printed/PDF ticket for a confirmed reservation.
package ticket

import (
	"bytes"
	_ "embed"
	"fmt"
	"image/png"
	"time"

	"github.com/jung-kurt/gofpdf"
	"github.com/skip2/go-qrcode"
)

//go:embed fonts/DejaVuSans.ttf
var fontRegular []byte

//go:embed fonts/DejaVuSans-Bold.ttf
var fontBold []byte

// Show is the subset of show info the ticket needs to print.
type Show struct {
	Title    string
	Venue    string
	StartsAt time.Time
}

// Seat is the subset of seat info the ticket needs to print. Category=="GA"
// switches the renderer to general-admission layout (no row/col, just
// "Квиток №N").
type Seat struct {
	Row      int
	Col      int
	Category string
}

// RenderPDF returns an A6 ticket with a navy header band, big seat
// callout, framed QR and footer. Uses embedded DejaVu Sans for Cyrillic.
// buyerName is shown above the seat callout when non-empty.
func RenderPDF(show Show, seat Seat, buyerName, qrPayload string) ([]byte, error) {
	q, err := qrcode.New(qrPayload, qrcode.Medium)
	if err != nil {
		return nil, fmt.Errorf("qrcode: %w", err)
	}
	q.DisableBorder = false
	var qrBuf bytes.Buffer
	if err := png.Encode(&qrBuf, q.Image(512)); err != nil {
		return nil, fmt.Errorf("encode qr: %w", err)
	}

	pdf := gofpdf.NewCustom(&gofpdf.InitType{
		UnitStr:        "mm",
		Size:           gofpdf.SizeType{Wd: 105, Ht: 148}, // A6 portrait
		OrientationStr: "P",
	})
	pdf.AddUTF8FontFromBytes("DejaVu", "", fontRegular)
	pdf.AddUTF8FontFromBytes("DejaVu", "B", fontBold)

	pageW, pageH := pdf.GetPageSize()
	pdf.SetMargins(0, 0, 0)
	pdf.SetAutoPageBreak(false, 0)
	pdf.AddPage()

	// Header band.
	const headerH = 28.0
	pdf.SetFillColor(20, 30, 60)
	pdf.Rect(0, 0, pageW, headerH, "F")
	pdf.SetTextColor(255, 255, 255)
	pdf.SetFont("DejaVu", "B", 16)
	pdf.SetXY(0, 6)
	pdf.CellFormat(pageW, 8, show.Title, "", 1, "C", false, 0, "")
	pdf.SetFont("DejaVu", "", 10)
	pdf.SetX(0)
	pdf.CellFormat(pageW, 5, show.Venue, "", 1, "C", false, 0, "")
	pdf.SetX(0)
	pdf.CellFormat(pageW, 5, formatDateTime(show.StartsAt), "", 1, "C", false, 0, "")

	// Buyer name (optional).
	pdf.SetTextColor(20, 30, 60)
	pdf.SetY(headerH + 4)
	if buyerName != "" {
		pdf.SetFont("DejaVu", "", 11)
		pdf.CellFormat(pageW, 5, buyerName, "", 1, "C", false, 0, "")
	}

	// Seat callout. GA shows render "GA · квиток №N" since there is no
	// row/col — every ticket is a guaranteed entry from a single pool.
	if seat.Category == "GA" {
		pdf.SetFont("DejaVu", "", 11)
		pdf.CellFormat(pageW, 5, "ВХІД", "", 1, "C", false, 0, "")
		pdf.SetFont("DejaVu", "B", 32)
		pdf.CellFormat(pageW, 14, fmt.Sprintf("GA · квиток №%d", seat.Col), "", 1, "C", false, 0, "")
	} else {
		pdf.SetFont("DejaVu", "", 11)
		pdf.CellFormat(pageW, 5, "МІСЦЕ", "", 1, "C", false, 0, "")
		pdf.SetFont("DejaVu", "B", 32)
		pdf.CellFormat(pageW, 14, fmt.Sprintf("ряд %d · %d", seat.Row, seat.Col), "", 1, "C", false, 0, "")
	}

	// QR.
	imgOpts := gofpdf.ImageOptions{ImageType: "PNG", ReadDpi: false}
	pdf.RegisterImageOptionsReader("qr", imgOpts, &qrBuf)
	const qrSize = 55.0
	qrX := (pageW - qrSize) / 2
	qrY := pdf.GetY() + 2
	pdf.ImageOptions("qr", qrX, qrY, qrSize, qrSize, false, imgOpts, 0, "")
	pdf.SetDrawColor(20, 30, 60)
	pdf.SetLineWidth(0.4)
	pdf.Rect(qrX-2, qrY-2, qrSize+4, qrSize+4, "D")

	// Footer.
	pdf.SetTextColor(80, 80, 80)
	pdf.SetFont("DejaVu", "", 8)
	pdf.SetY(pageH - 14)
	pdf.CellFormat(pageW, 4, "Покажи цей QR на вході — нам цього досить.", "", 1, "C", false, 0, "")
	pdf.CellFormat(pageW, 4, "Видано "+formatDateTime(time.Now()), "", 1, "C", false, 0, "")

	var out bytes.Buffer
	if err := pdf.Output(&out); err != nil {
		return nil, fmt.Errorf("pdf output: %w", err)
	}
	return out.Bytes(), nil
}

var ukMonthsGenitive = [...]string{
	"січня", "лютого", "березня", "квітня", "травня", "червня",
	"липня", "серпня", "вересня", "жовтня", "листопада", "грудня",
}

func formatDateTime(t time.Time) string {
	return fmt.Sprintf("%d %s %d · %s",
		t.Day(), ukMonthsGenitive[t.Month()-1], t.Year(), t.Format("15:04"))
}
