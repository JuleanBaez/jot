package dash

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"jot/core/ad"
	"jot/core/audit"
	"jot/core/events"
	"jot/core/iiq"
)

// ── Request context / scope ──────────────────────────────────────────────────

// reqCtx is a short-lived context for handlers that call the IIQ client.
// We use a 20s cap so a hung IIQ tenant doesn't pin an HTTP worker forever.
func reqCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 20*time.Second)
}

// ── View models ──────────────────────────────────────────────────────────────

// ticketView wraps an iiq.Ticket with template-friendly helpers so the HTML
// doesn't need to know about time arithmetic.
type ticketView struct {
	iiq.Ticket
}

// AgeDisplay renders Created as a short relative string ("3m", "2h", "5d").
func (t ticketView) AgeDisplay() string { return ageDisplay(t.Created) }

type queuePage struct {
	Tickets []ticketView
}

type homePage struct {
	Now string
}

type healthPage struct {
	Checks []healthCheck
}

type healthCheck struct {
	Name   string
	Level  string // "ok" | "warn" | "err" | "pending"
	Detail string
}

// activityPage is the view model for the Recent Activity panel. Exposed as a
// distinct type so it's easy to add filters later (actor, result) without
// touching the handler signature.
type activityPage struct {
	Entries []audit.Entry
}

// ── / ────────────────────────────────────────────────────────────────────────

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	render(w, "layout", homePage{Now: time.Now().Format("Mon 15:04")})
}

// ── /iiq/queue (GET) ─────────────────────────────────────────────────────────

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	if s.iiq == nil {
		renderString(w, `<p class="muted">IIQ is not configured in <code>~/.jot/config.json</code>.</p>`)
		return
	}
	ctx, cancel := reqCtx(r)
	defer cancel()

	opts := iiq.ListOptions{
		// No status filter: show all tickets regardless of status so tickets
		// in "In Progress", "Open", or any tenant-specific active status are
		// visible. The adSyncIncidentIQ CLI command is the one that needs the
		// Submitted-only filter; the dash queue should show everything.
		AssigneeUserID:    s.cfg.IncidentIQ.MyUserID,
		IncludeUnassigned: true,
		PageSize:          100,
	}
	tickets, err := s.iiq.ListTickets(ctx, opts)
	if err != nil {
		renderString(w, fmt.Sprintf(`<p class="muted">IIQ query failed: %s</p>`, template.HTMLEscapeString(err.Error())))
		return
	}

	// One-line breadcrumb so the operator can tell "returned zero" apart
	// from "handler never ran" when debugging.
	log.Printf("[dash] /iiq/queue → %d tickets (assignee=%q, include_unassigned=%t)",
		len(tickets), s.cfg.IncidentIQ.MyUserID, opts.IncludeUnassigned)

	views := make([]ticketView, len(tickets))
	for i, t := range tickets {
		views[i] = ticketView{Ticket: t}
	}
	render(w, "ticket_queue", queuePage{Tickets: views})
}

// ── /iiq/take/{id} (POST) ────────────────────────────────────────────────────

func (s *Server) handleTake(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/iiq/take/")
	if id == "" {
		http.Error(w, "missing ticket id", http.StatusBadRequest)
		return
	}
	if s.iiq == nil {
		http.Error(w, "IIQ not configured", http.StatusServiceUnavailable)
		return
	}
	uid := s.cfg.IncidentIQ.MyUserID
	if uid == "" {
		http.Error(w, "incident_iq.my_user_id not set — cannot Take", http.StatusPreconditionFailed)
		return
	}

	ctx, cancel := reqCtx(r)
	defer cancel()

	t, err := s.iiq.Take(ctx, id, uid)
	if err != nil {
		_ = audit.Fail("dash", "iiq.ticket.take", id, err, nil)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = audit.Ok("dash", "iiq.ticket.take", id, map[string]any{"assignee": uid})
	s.bus.Publish(events.Event{Topic: events.TopicTicketUpdated, Payload: map[string]string{"id": id}})

	render(w, "ticket_row", ticketView{Ticket: t})
}

// ── /iiq/resolve/{id} (POST) ─────────────────────────────────────────────────

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/iiq/resolve/")
	if id == "" {
		http.Error(w, "missing ticket id", http.StatusBadRequest)
		return
	}
	if s.iiq == nil {
		http.Error(w, "IIQ not configured", http.StatusServiceUnavailable)
		return
	}
	closedID := s.cfg.IncidentIQ.ClosedStatusID
	if closedID == "" {
		http.Error(w, "incident_iq.closed_status_id not set", http.StatusPreconditionFailed)
		return
	}

	// HTMX sends hx-prompt input via header.
	note := strings.TrimSpace(r.Header.Get("HX-Prompt"))

	ctx, cancel := reqCtx(r)
	defer cancel()

	if err := s.iiq.Resolve(ctx, id, closedID, note); err != nil {
		_ = audit.Fail("dash", "iiq.ticket.resolve", id, err, nil)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = audit.Ok("dash", "iiq.ticket.resolve", id, map[string]any{"note": note})
	s.bus.Publish(events.Event{Topic: events.TopicTicketClosed, Payload: map[string]string{"id": id}})

	// Swap the row out entirely — the row disappears from the panel.
	// Returning an empty string with hx-swap="outerHTML" removes the <tr>.
	w.Header().Set("Content-Type", "text/html")
	w.Write(nil)
}

// ── /audit/tail ──────────────────────────────────────────────────────────────

// handleActivity renders the Recent Activity panel. Audit is append-only so
// we just pull the last N entries newest-first and let the template do the
// rest.
//
// The panel is refreshed three ways:
//   - HTMX "load" trigger on initial render
//   - SSE "audit.append" trigger fired by the in-process audit→bus bridge,
//     so dash-side actions (Take / Resolve) reflect instantly
//   - a 10s polling fallback, so audit entries written by the jot CLI in
//     another process — which the in-process hook doesn't see — still show
//     up within one poll interval
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	entries, err := audit.Tail(50)
	if err != nil {
		renderString(w, fmt.Sprintf(`<p class="muted">Audit read failed: %s</p>`, template.HTMLEscapeString(err.Error())))
		return
	}
	render(w, "activity", activityPage{Entries: entries})
}

// ── /health/strip ────────────────────────────────────────────────────────────

func (s *Server) handleHealthStrip(w http.ResponseWriter, r *http.Request) {
	checks := []healthCheck{
		boolCheck("Config", s.cfg.AD.BaseDN != "", "base_dn empty"),
		boolCheck("IIQ", s.iiq != nil, "not configured"),
		boolCheck("Chat", s.cfg.GoogleChat.OpsSpace != "", "ops_space empty"),
		boolCheck("XDR", s.cfg.CortexXDR.FQDN != "" || s.cfg.CortexXDR.ScriptPath != "", "not configured"),
	}
	render(w, "health_strip", healthPage{Checks: checks})
}

func boolCheck(name string, ok bool, warnDetail string) healthCheck {
	if ok {
		return healthCheck{Name: name, Level: "ok"}
	}
	return healthCheck{Name: name, Level: "warn", Detail: warnDetail}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// openStatusIDs picks which statuses count as "open" for the queue view.
// Takes the configured SubmittedStatusID (from incident_iq.submitted_status_id)
// so we can send a known-good facet to IIQ. If it's empty we return nil,
// which makes ListTickets omit the status filter entirely — better to show
// the whole team queue than to silently return zero.
func openStatusIDs(submittedID string) []string {
	if submittedID == "" {
		return nil
	}
	return []string{submittedID}
}

// render executes a named template into the response.
// ── /ad/reset (POST) ─────────────────────────────────────────────────────────

func (s *Server) handleADReset(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	if username == "" {
		renderString(w, `<span class="qa-err">username required</span>`)
		return
	}
	l, err := ad.Connect(s.cfg)
	if err != nil {
		_ = audit.Fail("dash", "ad.reset", username, err, nil)
		renderString(w, `<span class="qa-err">`+template.HTMLEscapeString(err.Error())+`</span>`)
		return
	}
	defer l.Close()
	userDN, err := ad.FindUserDN(l, s.cfg, username)
	if err != nil {
		_ = audit.Fail("dash", "ad.reset", username, err, nil)
		renderString(w, `<span class="qa-err">`+template.HTMLEscapeString(err.Error())+`</span>`)
		return
	}
	if err := ad.ResetPasswordFlag(l, userDN); err != nil {
		_ = audit.Fail("dash", "ad.reset", username, err, nil)
		renderString(w, `<span class="qa-err">`+template.HTMLEscapeString(err.Error())+`</span>`)
		return
	}
	_ = audit.Ok("dash", "ad.reset", username, nil)
	renderString(w, `<span class="qa-ok">✓ password reset at next login</span>`)
}

// ── /ad/unlock (POST) ────────────────────────────────────────────────────────

func (s *Server) handleADUnlock(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.FormValue("username"))
	if username == "" {
		renderString(w, `<span class="qa-err">username required</span>`)
		return
	}
	l, err := ad.Connect(s.cfg)
	if err != nil {
		_ = audit.Fail("dash", "ad.unlock", username, err, nil)
		renderString(w, `<span class="qa-err">`+template.HTMLEscapeString(err.Error())+`</span>`)
		return
	}
	defer l.Close()
	userDN, err := ad.FindUserDN(l, s.cfg, username)
	if err != nil {
		_ = audit.Fail("dash", "ad.unlock", username, err, nil)
		renderString(w, `<span class="qa-err">`+template.HTMLEscapeString(err.Error())+`</span>`)
		return
	}
	if err := ad.Unlock(l, userDN); err != nil {
		_ = audit.Fail("dash", "ad.unlock", username, err, nil)
		renderString(w, `<span class="qa-err">`+template.HTMLEscapeString(err.Error())+`</span>`)
		return
	}
	_ = audit.Ok("dash", "ad.unlock", username, nil)
	renderString(w, `<span class="qa-ok">✓ account unlocked</span>`)
}

// ── /ad/provision (POST) ─────────────────────────────────────────────────────

func (s *Server) handleADProvision(w http.ResponseWriter, r *http.Request) {
	first := strings.TrimSpace(r.FormValue("first"))
	last := strings.TrimSpace(r.FormValue("last"))
	role := strings.TrimSpace(r.FormValue("role"))
	if first == "" || last == "" {
		renderString(w, `<span class="qa-err">first and last name required</span>`)
		return
	}
	l, err := ad.Connect(s.cfg)
	if err != nil {
		_ = audit.Fail("dash", "ad.provision", first+" "+last, err, nil)
		renderString(w, `<span class="qa-err">`+template.HTMLEscapeString(err.Error())+`</span>`)
		return
	}
	defer l.Close()
	res, err := ad.Provision(l, s.cfg, first, last, role, "")
	if err != nil {
		_ = audit.Fail("dash", "ad.provision", first+" "+last, err, nil)
		renderString(w, `<span class="qa-err">`+template.HTMLEscapeString(err.Error())+`</span>`)
		return
	}
	_ = audit.Ok("dash", "ad.provision", res.Username, map[string]any{"role": role, "email": res.Email})
	renderString(w, `<span class="qa-ok">✓ `+template.HTMLEscapeString(res.Username)+` created · `+template.HTMLEscapeString(res.Email)+`</span>`)
}

// ── /ad/quick (GET) ──────────────────────────────────────────────────────────

func (s *Server) handleQuickActions(w http.ResponseWriter, r *http.Request) {
	render(w, "quick_actions", nil)
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	buf.WriteTo(w)
}

func renderString(w http.ResponseWriter, html string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, html)
}
