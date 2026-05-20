package email

import (
	"strings"
	"testing"
	"time"
)

func TestBuildICSContainsRequiredFields(t *testing.T) {
	inv := EventInvite{
		Title:    "Концерт",
		Venue:    "Театральна площа",
		StartsAt: time.Date(2026, 6, 15, 19, 0, 0, 0, time.UTC),
		StableID: "show-42",
	}
	out := string(BuildICS(inv))
	for _, want := range []string{
		"BEGIN:VCALENDAR",
		"END:VCALENDAR",
		"BEGIN:VEVENT",
		"END:VEVENT",
		"UID:show-42@monokasa",
		"DTSTART:20260615T190000Z",
		"DTEND:20260615T210000Z", // default 2h
		"SUMMARY:Концерт",
		"LOCATION:Театральна площа",
		"STATUS:CONFIRMED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestBuildICSEscapesSpecialChars(t *testing.T) {
	inv := EventInvite{
		Title:    `One; two, three\four`,
		StartsAt: time.Now(),
		StableID: "x",
	}
	out := string(BuildICS(inv))
	if !strings.Contains(out, `SUMMARY:One\; two\, three\\four`) {
		t.Errorf("special chars not escaped:\n%s", out)
	}
}

func TestBuildICSUIDFallback(t *testing.T) {
	inv := EventInvite{
		Title:    "Same",
		Venue:    "Same",
		StartsAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	a := string(BuildICS(inv))
	b := string(BuildICS(inv))
	// Pull just the UID line from each to compare — DTSTAMP differs by ms.
	uidA := findLine(a, "UID:")
	uidB := findLine(b, "UID:")
	if uidA == "" || uidA != uidB {
		t.Errorf("UID fallback not stable: %q vs %q", uidA, uidB)
	}
}

func TestBuildICSCustomDuration(t *testing.T) {
	inv := EventInvite{
		Title:    "Short",
		StartsAt: time.Date(2026, 1, 1, 18, 0, 0, 0, time.UTC),
		Duration: 90 * time.Minute,
		StableID: "x",
	}
	out := string(BuildICS(inv))
	if !strings.Contains(out, "DTEND:20260101T193000Z") {
		t.Errorf("custom duration not applied:\n%s", out)
	}
}

func findLine(s, prefix string) string {
	for line := range strings.Lines(s) {
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}
