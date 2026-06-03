// Package events is a minimal in-process pub/sub for jot. The dash runs
// multiple goroutines (IIQ poller, XDR alert poller, SSE handlers) that
// need to share events without knowing about each other. This bus lets
// publishers fire-and-forget and subscribers receive everything until they
// unsubscribe.
//
// Deliberately simple: one process, one bus, unbuffered fan-out through a
// channel per subscriber. If a subscriber's channel is full, we drop rather
// than block — better to miss a UI refresh than wedge the poller.
package events

import (
	"sync"
	"time"
)

// Topic namespaces event kinds. Keep them dotted and stable; the dash SSE
// handler forwards topic strings to the browser verbatim.
type Topic string

const (
	TopicTicketNew     Topic = "ticket.new"
	TopicTicketUpdated Topic = "ticket.updated"
	TopicTicketClosed  Topic = "ticket.closed"
	TopicAuditAppend   Topic = "audit.append"
	TopicXDRAlert      Topic = "xdr.alert"
)

// Event is what publishers send. Payload is opaque — subscribers interpret
// it based on Topic. Time is auto-stamped by Publish if zero.
type Event struct {
	Topic   Topic     `json:"topic"`
	Time    time.Time `json:"t"`
	Payload any       `json:"payload,omitempty"`
}

// Bus is the fan-out hub. Zero value is ready to use.
type Bus struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

// NewBus returns a ready bus. Provided mostly for callers that want an
// explicit constructor; a &Bus{} literal works too.
func NewBus() *Bus { return &Bus{} }

// Subscribe returns a receive-only channel that gets every future event
// plus an unsubscribe func the caller must invoke to stop and free
// resources. The channel is buffered so a slow consumer doesn't block the
// publisher, but a full buffer means we drop events (see Publish).
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)

	b.mu.Lock()
	if b.subs == nil {
		b.subs = make(map[chan Event]struct{})
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	unsub := func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
	return ch, unsub
}

// Publish sends an event to every current subscriber. If a subscriber's
// channel is full, that delivery is silently dropped — we never block.
// This means a paused browser tab or a dead goroutine can miss events
// without starving everyone else.
func (b *Bus) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
			// drop; subscriber is not keeping up
		}
	}
}
