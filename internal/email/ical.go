package email

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// EventInvite is the minimal subset of show fields needed to compose
// an .ics file that mail clients render as "Add to calendar".
type EventInvite struct {
	Title    string
	Venue    string
	StartsAt time.Time
	// Duration of the event for DTEND. Defaults to 2h when zero — most
	// shows fit, and clients only use this to block off the slot on the
	// buyer's calendar, not to drive any logic.
	Duration time.Duration
	// StableID seeds the iCalendar UID so re-sending the same invite
	// updates the existing event in the buyer's calendar instead of
	// adding a duplicate. Slug, show ID — anything stable per show.
	StableID string
	// Organizer is the mail address shown on the calendar event. Pass
	// the same address as the SMTP From; defaults to "noreply@monokasa"
	// when empty so the .ics is still valid.
	Organizer string
}

// BuildICS renders a minimal but RFC-5545-conformant VCALENDAR with one
// VEVENT. CRLF line endings; no line folding (lines stay short in our
// inputs — long Cyrillic titles encode to ~150 bytes which Apple/Gmail
// both tolerate without folding).
func BuildICS(inv EventInvite) []byte {
	if inv.Duration <= 0 {
		inv.Duration = 2 * time.Hour
	}
	if inv.Organizer == "" {
		inv.Organizer = "noreply@monokasa"
	}
	uid := inv.StableID
	if uid == "" {
		// Hash title+venue+start as a fallback so the calendar entry is
		// still stable across re-deliveries of the same invite.
		sum := sha256.Sum256([]byte(inv.Title + "|" + inv.Venue + "|" + inv.StartsAt.UTC().Format(time.RFC3339)))
		uid = hex.EncodeToString(sum[:8])
	}
	start := inv.StartsAt.UTC()
	end := start.Add(inv.Duration)

	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteString("\r\n") }
	w("BEGIN:VCALENDAR")
	w("VERSION:2.0")
	w("PRODID:-//monokasa//tickets//EN")
	w("METHOD:PUBLISH")
	w("BEGIN:VEVENT")
	w("UID:" + uid + "@monokasa")
	fmt.Fprintf(&b, "DTSTAMP:%s\r\n", time.Now().UTC().Format("20060102T150405Z"))
	fmt.Fprintf(&b, "DTSTART:%s\r\n", start.Format("20060102T150405Z"))
	fmt.Fprintf(&b, "DTEND:%s\r\n", end.Format("20060102T150405Z"))
	w("SUMMARY:" + escapeICS(inv.Title))
	if inv.Venue != "" {
		w("LOCATION:" + escapeICS(inv.Venue))
	}
	w("ORGANIZER:mailto:" + inv.Organizer)
	w("STATUS:CONFIRMED")
	w("TRANSP:OPAQUE")
	w("END:VEVENT")
	w("END:VCALENDAR")
	return []byte(b.String())
}

// escapeICS escapes the four characters iCalendar TEXT property values
// must protect: backslash, semicolon, comma, newline. No other escaping
// is needed for UTF-8 payloads.
func escapeICS(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `;`, `\;`)
	s = strings.ReplaceAll(s, `,`, `\,`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
