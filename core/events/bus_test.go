package events

import (
	"sync"
	"testing"
	"time"
)

func TestPublishFansOutToAllSubscribers(t *testing.T) {
	b := NewBus()

	a, unsubA := b.Subscribe()
	defer unsubA()
	c, unsubC := b.Subscribe()
	defer unsubC()

	b.Publish(Event{Topic: TopicTicketNew, Payload: "hello"})

	for _, ch := range []<-chan Event{a, c} {
		select {
		case e := <-ch:
			if e.Topic != TopicTicketNew {
				t.Errorf("got topic %q", e.Topic)
			}
			if e.Time.IsZero() {
				t.Error("time not stamped")
			}
		case <-time.After(500 * time.Millisecond):
			t.Error("subscriber did not receive event")
		}
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	b := NewBus()
	ch, unsub := b.Subscribe()

	unsub()

	// Publishing after unsubscribe must not panic or block.
	b.Publish(Event{Topic: TopicTicketNew})

	// Channel should be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("received on channel after unsubscribe")
		}
	default:
		// closed channel reads return immediately; if we hit default
		// that's fine too — means no events were queued.
	}
}

func TestSlowSubscriberIsDroppedNotBlocking(t *testing.T) {
	b := NewBus()
	_, unsub := b.Subscribe() // never read from this channel
	defer unsub()

	done := make(chan struct{})
	go func() {
		// Publishing 1000 events must not block on the slow subscriber's
		// 32-deep buffer. If the bus blocks, this goroutine never
		// finishes and the test times out.
		for i := 0; i < 1000; i++ {
			b.Publish(Event{Topic: TopicTicketNew})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on slow subscriber")
	}
}

func TestConcurrentPublishSubscribe(t *testing.T) {
	b := NewBus()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, unsub := b.Subscribe()
			defer unsub()
			for j := 0; j < 100; j++ {
				b.Publish(Event{Topic: TopicTicketNew})
			}
		}()
	}
	wg.Wait()
}
