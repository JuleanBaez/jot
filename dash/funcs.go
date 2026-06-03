package dash

import (
	"fmt"
	"html/template"
	"time"
)

// tmplFuncs are helpers available in every template. Keep this list small —
// prefer doing shaping in Go and passing ready-to-render data in.
var tmplFuncs = template.FuncMap{
	"ageDisplay":   ageDisplay,
	"shortTime":    shortTime,
	"shortSubject": shortSubject,
}

// ageDisplay turns a past time into a short human string: "3m", "2h", "5d".
// Zero time renders as "—" so tickets with missing CreatedDate don't look
// broken in the panel.
func ageDisplay(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// shortTime renders a time for the activity feed. Today's entries show as
// "15:04" (local), older entries show the date too ("Apr 21 15:04"). Keeps
// the activity column narrow while staying unambiguous when the dash has
// been left open across midnight.
func shortTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	local := t.Local()
	now := time.Now()
	if local.Year() == now.Year() && local.YearDay() == now.YearDay() {
		return local.Format("15:04")
	}
	return local.Format("Jan 2 15:04")
}

// shortSubject truncates a subject to max characters, adding an ellipsis.
// Avoids blowing out the activity row with long ticket GUIDs.
func shortSubject(s string) string {
	const max = 24
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
