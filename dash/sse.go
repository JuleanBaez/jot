package dash

import (
	"encoding/json"
	"fmt"
	"net/http"

	"jot/core/events"
)

// handleSSE upgrades the request to a server-sent-events stream and forwards
// every event from the bus to the client. The HTMX sse extension listens on
// named events (see sse-swap in the templates), so we emit each one with an
// `event:` line matching the Topic.
//
// Keeps the handler simple: no buffering, no backpressure — Subscribe returns
// a channel that already drops when the subscriber is slow, so we inherit
// that behavior without any extra code here.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // defeat any reverse-proxy buffering

	// Initial comment line flushes the headers to the browser so htmx's sse
	// extension considers the connection open immediately.
	fmt.Fprint(w, ": jot dash connected\n\n")
	flusher.Flush()

	ch, unsub := s.bus.Subscribe()
	defer unsub()

	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, e)
			flusher.Flush()
		}
	}
}

// writeSSE writes one event frame. HTMX's sse ext triggers on `event:` so we
// always set it. Data can be empty — consumers that only care about the
// trigger (e.g. ticket-panel refresh) will ignore the body.
func writeSSE(w http.ResponseWriter, e events.Event) {
	fmt.Fprintf(w, "event: %s\n", e.Topic)
	if e.Payload != nil {
		if b, err := json.Marshal(e.Payload); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", b)
			return
		}
	}
	fmt.Fprint(w, "data: {}\n\n")
}
