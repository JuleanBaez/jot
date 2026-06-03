package iiq

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"jot/core/config"
)

// fakeIIQ is a minimal stand-in for an Incident IQ tenant. It records the
// last request and returns canned responses so the smoke tests don't need a
// live tenant or network access. Each endpoint handler mirrors just enough
// of the real IIQ wire shape to exercise the projection code.
func fakeIIQ(t *testing.T) (*Client, *recorder, func()) {
	t.Helper()
	rec := &recorder{}

	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1.0/tickets", func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Data": map[string]any{
				"Items": []map[string]any{
					{
						"Id":                  "guid-1",
						"TicketId":            "T-1001",
						"Subject":             "Projector out in room 204",
						"Priority":            "High",
						"StatusId":            "st-open",
						"StatusName":          "Open",
						"ForDisplayName":      "Jane Requestor",
						"AssigneeDisplayName": "",
						"CreatedDate":         "2026-04-20T10:00:00Z",
					},
				},
			},
		})
	})

	// GET /tickets/{id} — used after Take to re-fetch
	mux.HandleFunc("/api/v1.0/tickets/", func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		// Path shape: /api/v1.0/tickets/{id}[/comments]
		if strings.HasSuffix(r.URL.Path, "/comments") {
			// POST comment — return empty OK
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Data": map[string]any{
				"Id":                  "guid-1",
				"TicketId":            "T-1001",
				"Subject":             "Projector out in room 204",
				"StatusId":            "st-open",
				"AssigneeDisplayName": "Me",
				"AssigneeUserId":      "me-uid",
			},
		})
	})

	srv := httptest.NewServer(mux)

	cfg := config.Jot{}
	cfg.IncidentIQ.BaseURL = srv.URL
	cfg.IncidentIQ.Token = "testtoken"

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, rec, func() { srv.Close() }
}

// recorder captures requests so tests can assert on headers / body.
type recorder struct {
	LastPath   string
	LastMethod string
	LastAuth   string
	LastBody   string
}

func (r *recorder) record(req *http.Request) {
	r.LastPath = req.URL.Path
	r.LastMethod = req.Method
	r.LastAuth = req.Header.Get("Authorization")
	b, _ := io.ReadAll(req.Body)
	r.LastBody = string(b)
}

func TestListTicketsProjection(t *testing.T) {
	c, rec, done := fakeIIQ(t)
	defer done()

	tickets, err := c.ListTickets(context.Background(), ListOptions{})
	if err != nil {
		t.Fatalf("ListTickets: %v", err)
	}
	if len(tickets) != 1 {
		t.Fatalf("want 1 ticket, got %d", len(tickets))
	}
	got := tickets[0]
	if got.TicketID != "T-1001" {
		t.Errorf("TicketID: want T-1001, got %q", got.TicketID)
	}
	if got.Subject == "" || got.Requestor == "" || got.Priority == "" {
		t.Errorf("missing projected fields: %+v", got)
	}
	if got.Created.IsZero() {
		t.Errorf("Created did not parse: %+v", got)
	}
	// Auth header must be Bearer <token>
	if rec.LastAuth != "Bearer testtoken" {
		t.Errorf("auth header: got %q", rec.LastAuth)
	}
}

func TestTakeRoundTrip(t *testing.T) {
	c, _, done := fakeIIQ(t)
	defer done()

	got, err := c.Take(context.Background(), "guid-1", "me-uid")
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if got.AssigneeID != "me-uid" {
		t.Errorf("assignee not set after Take: %+v", got)
	}
}

func TestResolveCallsClose(t *testing.T) {
	c, rec, done := fakeIIQ(t)
	defer done()

	err := c.Resolve(context.Background(), "guid-1", "st-closed", "done")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rec.LastMethod != http.MethodPatch {
		t.Errorf("want final PATCH, got %s", rec.LastMethod)
	}
	if !strings.Contains(rec.LastBody, "st-closed") {
		t.Errorf("close body missing status id: %q", rec.LastBody)
	}
}

func TestNewFailsWithoutConfig(t *testing.T) {
	if _, err := New(config.Jot{}); err == nil {
		t.Fatal("expected error when IIQ not configured")
	}
}

// TestAssigneeFilterIsClientSide verifies that ListOptions.AssigneeUserID is
// NOT sent as an IIQ facet — the wire body should only contain status and
// issuecategory — and that the returned slice is narrowed to the matching
// assignee (plus unassigned when IncludeUnassigned is set). Regression guard
// for the empty-queue bug we hit against a real tenant.
func TestAssigneeFilterIsClientSide(t *testing.T) {
	rec := &recorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1.0/tickets", func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Data": map[string]any{
				"Items": []map[string]any{
					{"Id": "a", "TicketId": "T-1", "AssigneeUserId": "me-uid"},
					{"Id": "b", "TicketId": "T-2", "AssigneeUserId": "someone-else"},
					{"Id": "c", "TicketId": "T-3", "AssigneeUserId": ""},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := config.Jot{}
	cfg.IncidentIQ.BaseURL = srv.URL
	cfg.IncidentIQ.Token = "tok"
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// "my tickets + unclaimed" — should drop T-2 only.
	got, err := c.ListTickets(context.Background(), ListOptions{
		AssigneeUserID:    "me-uid",
		IncludeUnassigned: true,
	})
	if err != nil {
		t.Fatalf("ListTickets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 (mine + unassigned), got %d: %+v", len(got), got)
	}

	// Wire body must NOT mention the assignee facet — that's the bug fix.
	if strings.Contains(rec.LastBody, `"assignee"`) {
		t.Errorf("assignee facet leaked onto the wire: %s", rec.LastBody)
	}
}
