// Package timefmt is a tiny Ukrainian locale helper for date/time strings.
// Go's standard time package only knows English month names, so everything
// user-facing in this app routes through here.
package timefmt

import (
	"fmt"
	"time"
)

var ukMonthsGenitive = [...]string{
	"січня", "лютого", "березня", "квітня", "травня", "червня",
	"липня", "серпня", "вересня", "жовтня", "листопада", "грудня",
}

// Date returns "5 червня 2026".
func Date(t time.Time) string {
	return fmt.Sprintf("%d %s %d", t.Day(), ukMonthsGenitive[t.Month()-1], t.Year())
}

// DateTime returns "5 червня 2026 · 19:00".
func DateTime(t time.Time) string {
	return fmt.Sprintf("%s · %s", Date(t), t.Format("15:04"))
}

// Clock returns just "19:00".
func Clock(t time.Time) string { return t.Format("15:04") }
