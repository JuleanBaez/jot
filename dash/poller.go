package dash

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"jot/core/events"
	"jot/core/iiq"
)

// pollIIQ is the background loop that watches for new tickets in the
// operator's queue. When it sees a ticket ID it hasn't seen before, it
// publishes events.TopicTicketNew on the bus — every connected browser
// refreshes its queue panel via SSE.
//
// Dedupe strategy: keep a set of ticket GUIDs we've already emitted. First
// poll primes the set without firing events (so the dash doesn't shout
// about every ticket in your queue when you boot up). Subsequent polls
// emit for anything new.
func (s *Server) pollIIQ(ctx context.Context) {
	if s.iiq == nil {
		return
	}
	interval := time.Duration(s.cfg.Dash.PollSeconds) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}

	seen := newSeenSet(2048)
	primed := false

	tick := time.NewTicker(interval)
	defer tick.Stop()

	// Poll immediately on startup so the first panel render is fresh.
	s.pollOnce(ctx, seen, &primed)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.pollOnce(ctx, seen, &primed)
		}
	}
}

func (s *Server) pollOnce(ctx context.Context, seen *seenSet, primed *bool) {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	tickets, err := s.iiq.ListTickets(qctx, iiq.ListOptions{
		AssigneeUserID: s.cfg.IncidentIQ.MyUserID,
		PageSize:       100,
	})
	if err != nil {
		// Log quietly — transient IIQ blips shouldn't spam the console.
		fmt.Fprintf(os.Stderr, "[dash] iiq poll: %v\n", err)
		return
	}

	if !*primed {
		for _, t := range tickets {
			seen.add(t.ID)
		}
		*primed = true
		return
	}

	for _, t := range tickets {
		if seen.add(t.ID) {
			s.bus.Publish(events.Event{
				Topic: events.TopicTicketNew,
				Payload: map[string]string{
					"id":       t.ID,
					"ticketId": t.TicketID,
					"subject":  t.Subject,
				},
			})
		}
	}
}

// seenSet is a tiny bounded set of ticket IDs used for dedupe. We cap it so
// a long-running dash doesn't leak memory across a very chatty tenant.
// When the cap is hit we clear the oldest half; false-positive re-emit is
// fine because the panel refreshes are idempotent.
type seenSet struct {
	mu    sync.Mutex
	items map[string]int64 // id -> insert counter
	seq   int64
	cap   int
}

func newSeenSet(cap int) *seenSet {
	if cap <= 0 {
		cap = 1024
	}
	return &seenSet{items: make(map[string]int64, cap), cap: cap}
}

// add returns true if id was NOT already present.
func (s *seenSet) add(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; ok {
		return false
	}
	if len(s.items) >= s.cap {
		// Evict the oldest half by counter.
		cutoff := s.seq - int64(s.cap/2)
		for k, v := range s.items {
			if v < cutoff {
				delete(s.items, k)
			}
		}
	}
	s.seq++
	s.items[id] = s.seq
	return true
}
