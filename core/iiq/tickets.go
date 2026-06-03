package iiq

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Ticket is the dash-facing view of an IIQ ticket. IIQ returns a huge blob
// per ticket; we project only what the UI uses. Extend as the panel grows.
type Ticket struct {
	ID         string    `json:"id"`
	TicketID   string    `json:"ticket_id"`   // IIQ's short display number, e.g. "37821"
	Subject    string    `json:"subject"`
	Priority   string    `json:"priority"`
	StatusID   string    `json:"status_id"`
	StatusName string    `json:"status_name"`
	Requestor  string    `json:"requestor"`   // display name of who opened it
	AssigneeID string    `json:"assignee_id"` // IIQ user GUID, may be empty
	Assignee   string    `json:"assignee"`    // display name, may be empty
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
	IsClosed   bool      `json:"is_closed"`
}

// ListOptions narrows a ListTickets query. Zero value returns everything
// open in the operator's team queue (up to PageSize).
//
// Facet names used on the wire ("status", "issuecategory") are the ones the
// legacy adSyncIncidentIQ already exercises against live tenants — known
// good. Additional filters like "assignee" are intentionally NOT sent on the
// wire because IIQ tenants disagree on the facet name; callers that want
// assignee filtering should pass AssigneeUserID and we filter client-side
// after projection.
type ListOptions struct {
	// AssigneeUserID filters to tickets assigned to a specific IIQ user.
	// Filter is applied CLIENT-SIDE after the IIQ response is projected,
	// because the wire facet name for assignee varies by tenant. Pass ""
	// to disable.
	AssigneeUserID string
	// StatusIDs narrows to specific statuses at the IIQ layer. Empty means
	// "don't filter by status" — the dash typically passes the configured
	// SubmittedStatusID here so the queue shows new/open tickets.
	StatusIDs []string
	// CategoryIDs narrows to specific issue categories at the IIQ layer.
	// Empty means "all categories".
	CategoryIDs []string
	// IncludeUnassigned, when true together with AssigneeUserID, keeps
	// tickets that have no assignee at all in the result. This matches the
	// real operator workflow: "my tickets + the unclaimed queue".
	IncludeUnassigned bool
	// PageSize is the max rows to return. Defaults to 50 when <= 0.
	PageSize int
}

// ListTicketsRaw issues the same request ListTickets does but returns the
// raw decoded JSON plus the on-the-wire body. Used by `jot iiq debug` to
// show the operator exactly what the tenant returned so we can identify
// missing fields / wrong facets without guessing.
func (c *Client) ListTicketsRaw(ctx context.Context, opts ListOptions) (request string, response map[string]any, err error) {
	size := opts.PageSize
	if size <= 0 {
		size = 50
	}

	filters := []map[string]string{}
	for _, sid := range opts.StatusIDs {
		if sid != "" {
			filters = append(filters, map[string]string{"Facet": "status", "Id": sid})
		}
	}
	for _, cid := range opts.CategoryIDs {
		if cid != "" {
			filters = append(filters, map[string]string{"Facet": "issuecategory", "Id": cid})
		}
	}
	body := map[string]any{"Filters": filters}

	reqBytes, _ := json.Marshal(body)
	response = map[string]any{}
	path := fmt.Sprintf("/tickets?$s=%d", size)
	if err := c.PostJSON(ctx, path, body, &response); err != nil {
		return string(reqBytes), response, err
	}
	return string(reqBytes), response, nil
}

// ticketsEnvelope matches IIQ's wire shape: Data.Items is a list of tickets.
// We decode into a loose map first and then project to Ticket so the field
// renames don't crash on tenant-specific custom fields.
type ticketsEnvelope struct {
	Data struct {
		Items []map[string]any `json:"Items"`
	} `json:"Data"`
}

// ListTickets queries /tickets with the given filters and projects the
// response into []Ticket. The dash calls this on every poll tick.
//
// Only facets known-good against real tenants go on the wire ("status",
// "issuecategory"). Assignee filtering is applied in Go after projection so
// an unknown-on-this-tenant facet never silently wipes the queue.
func (c *Client) ListTickets(ctx context.Context, opts ListOptions) ([]Ticket, error) {
	size := opts.PageSize
	if size <= 0 {
		size = 50
	}

	filters := []map[string]string{}
	for _, sid := range opts.StatusIDs {
		if sid != "" {
			filters = append(filters, map[string]string{"Facet": "status", "Id": sid})
		}
	}
	for _, cid := range opts.CategoryIDs {
		if cid != "" {
			filters = append(filters, map[string]string{"Facet": "issuecategory", "Id": cid})
		}
	}

	body := map[string]any{"Filters": filters}
	path := fmt.Sprintf("/tickets?$s=%d", size)

	var env ticketsEnvelope
	if err := c.PostJSON(ctx, path, body, &env); err != nil {
		return nil, err
	}

	all := make([]Ticket, 0, len(env.Data.Items))
	for _, raw := range env.Data.Items {
		t := projectTicket(raw)
		if !t.IsClosed {
			all = append(all, t)
		}
	}

	// Client-side assignee filter. "My tickets" = AssigneeID matches; add
	// the unassigned bucket ("") when IncludeUnassigned is set, so the
	// operator also sees what's sitting in the team queue waiting to be
	// claimed.
	if opts.AssigneeUserID == "" {
		return all, nil
	}
	filtered := all[:0]
	for _, t := range all {
		if t.AssigneeID == opts.AssigneeUserID {
			filtered = append(filtered, t)
			continue
		}
		if opts.IncludeUnassigned && t.AssigneeID == "" {
			filtered = append(filtered, t)
		}
	}
	return filtered, nil
}

// UpdateStatus patches a ticket's status field.
func (c *Client) UpdateStatus(ctx context.Context, id, statusID string) error {
	if id == "" || statusID == "" {
		return fmt.Errorf("UpdateStatus: id and statusID required")
	}
	return c.PatchJSON(ctx, "/tickets/"+id, map[string]string{"StatusId": statusID}, nil)
}

// GetTicket fetches one ticket by its IIQ GUID.
func (c *Client) GetTicket(ctx context.Context, id string) (Ticket, error) {
	var env struct {
		Data map[string]any `json:"Data"`
	}
	if err := c.GetJSON(ctx, "/tickets/"+id, &env); err != nil {
		return Ticket{}, err
	}
	return projectTicket(env.Data), nil
}

// Take assigns a ticket to the given IIQ user GUID. Returns the updated
// ticket so the caller can re-render the row.
func (c *Client) Take(ctx context.Context, id, userID string) (Ticket, error) {
	if id == "" || userID == "" {
		return Ticket{}, fmt.Errorf("Take: ticket id and user id are required")
	}
	body := map[string]string{"AssigneeUserId": userID}
	if err := c.PatchJSON(ctx, "/tickets/"+id, body, nil); err != nil {
		return Ticket{}, err
	}
	return c.GetTicket(ctx, id)
}

// Comment posts a public resolution comment on a ticket.
func (c *Client) Comment(ctx context.Context, id, note string) error {
	if id == "" {
		return fmt.Errorf("Comment: ticket id required")
	}
	body := map[string]any{"Comment": note, "IsPublic": true}
	return c.PostJSON(ctx, "/tickets/"+id+"/comments", body, nil)
}

// Resolve posts a comment (if non-empty) and patches status to closed.
// Comment failure is logged by the caller but does not stop the close — the
// important step is the status change.
func (c *Client) Resolve(ctx context.Context, id, closedStatusID, note string) error {
	if closedStatusID == "" {
		return fmt.Errorf("Resolve: closed_status_id not configured")
	}
	if strings.TrimSpace(note) != "" {
		// Don't fail the whole Resolve if the comment post fails; we log
		// it at the dash layer and continue to the status change.
		_ = c.Comment(ctx, id, note)
	}
	return c.PatchJSON(ctx, "/tickets/"+id, map[string]string{"StatusId": closedStatusID}, nil)
}

// ── Projection ────────────────────────────────────────────────────────────────

func projectTicket(raw map[string]any) Ticket {
	// Nested objects IIQ embeds per ticket.
	assignedTo, _ := raw["AssignedToUser"].(map[string]any)
	forUser, _ := raw["For"].(map[string]any)
	step, _ := raw["WorkflowStep"].(map[string]any)

	return Ticket{
		ID:         asString(raw["TicketId"]),     // GUID — used for Take/Resolve API calls
		TicketID:   asString(raw["TicketNumber"]), // short display number shown in UI
		Subject:    asString(raw["Subject"]),
		Priority:   priorityName(raw["Priority"]),
		StatusID:   asString(step["StatusId"]),
		StatusName: asString(step["StatusName"]),
		Requestor:  asString(forUser["Name"]),
		AssigneeID: asString(raw["AssignedToUserId"]),
		Assignee:   asString(assignedTo["Name"]),
		Created:    asTime(raw["CreatedDate"]),
		Updated:    asTime(raw["ModifiedDate"]),
		IsClosed:   asBool(raw["IsClosed"]),
	}
}

func priorityName(v any) string {
	switch fmt.Sprintf("%v", v) {
	case "1":
		return "Urgent"
	case "2":
		return "High"
	case "3":
		return "Medium"
	default:
		return "Normal"
	}
}

func asBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func asTime(v any) time.Time {
	s := asString(v)
	if s == "" {
		return time.Time{}
	}
	// IIQ uses ISO-8601 with optional fractional seconds and TZ.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
