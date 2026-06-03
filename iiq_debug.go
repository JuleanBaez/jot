package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"jot/core/config"
	"jot/core/iiq"
)

// handleIIQDebug implements `jot iiq debug`. It fires a real ListTickets call
// using the user's config and dumps the full request body and response to
// stdout, so we can identify missing fields / wrong facets / tenant quirks
// without guessing. Not hooked into anything destructive — pure read.
func handleIIQDebug(args []string) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "iiq debug: %v\n", err)
		os.Exit(1)
	}
	c, err := iiq.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "iiq debug: %v\n", err)
		os.Exit(1)
	}

	// By default mirror what the dash panel sends: status=submitted,
	// assignee filtered client-side. Operator can override facets via:
	//   jot iiq debug --status <id> --category <id> --no-status
	opts := iiq.ListOptions{
		StatusIDs: []string{cfg.IncidentIQ.SubmittedStatusID},
		PageSize:  100,
	}
	noStatus := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--no-status":
			noStatus = true
		case "--status":
			if i+1 < len(args) {
				opts.StatusIDs = []string{args[i+1]}
				i++
			}
		case "--category":
			if i+1 < len(args) {
				opts.CategoryIDs = append(opts.CategoryIDs, args[i+1])
				i++
			}
		case "--help", "-h":
			fmt.Println("Usage: jot iiq debug [--no-status] [--status <id>] [--category <id>]")
			fmt.Println()
			fmt.Println("Dumps the on-the-wire IIQ request + raw response so you can")
			fmt.Println("confirm facet names and field shapes for your tenant.")
			return
		}
	}
	if noStatus {
		opts.StatusIDs = nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, resp, err := c.ListTicketsRaw(ctx, opts)
	fmt.Println("── request body ─────────────────────────────────────")
	fmt.Println(req)
	fmt.Println()
	fmt.Println("── response ─────────────────────────────────────────")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	pretty, _ := json.MarshalIndent(resp, "", "  ")
	fmt.Println(string(pretty))

	// Quick summary so the operator doesn't have to scan 500 lines of JSON.
	if data, ok := resp["Data"].(map[string]any); ok {
		if items, ok := data["Items"].([]any); ok {
			fmt.Printf("\n── summary: %d items returned ─────────────\n", len(items))
			for _, it := range items {
				m, _ := it.(map[string]any)
				fmt.Printf("  %v  %v  assignee=%v  status=%v\n",
					m["TicketId"], m["Subject"], m["AssigneeDisplayName"], m["StatusName"])
			}
		}
	}
}
