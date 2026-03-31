package dashboard

import (
	"testing"
	"time"
)

func TestNewEventBus(t *testing.T) {
	bus := NewEventBus(10)
	if bus == nil {
		t.Fatal("NewEventBus returned nil")
	}
	if bus.size != 10 {
		t.Fatalf("expected size 10, got %d", bus.size)
	}
	if len(bus.subscribers) != 0 {
		t.Fatalf("expected 0 subscribers, got %d", len(bus.subscribers))
	}
}

func TestEventBus_Recent_Empty(t *testing.T) {
	bus := NewEventBus(10)
	events := bus.Recent()
	if events != nil {
		t.Fatalf("expected nil for empty bus, got %v", events)
	}
}

func TestEventBus_Publish_And_Recent(t *testing.T) {
	bus := NewEventBus(10)

	bus.Publish(Event{Type: EventReady, Message: "first"})
	bus.Publish(Event{Type: EventDraining, Message: "second"})

	events := bus.Recent()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Message != "first" {
		t.Errorf("expected first event message 'first', got %q", events[0].Message)
	}
	if events[1].Message != "second" {
		t.Errorf("expected second event message 'second', got %q", events[1].Message)
	}
}

func TestEventBus_Publish_SetsTime(t *testing.T) {
	bus := NewEventBus(10)
	before := time.Now()
	bus.Publish(Event{Type: EventReady, Message: "test"})
	after := time.Now()

	events := bus.Recent()
	if events[0].Time.Before(before) || events[0].Time.After(after) {
		t.Errorf("event time %v not between %v and %v", events[0].Time, before, after)
	}
}

func TestEventBus_Publish_PreservesExplicitTime(t *testing.T) {
	bus := NewEventBus(10)
	explicit := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	bus.Publish(Event{Type: EventReady, Time: explicit, Message: "test"})

	events := bus.Recent()
	if !events[0].Time.Equal(explicit) {
		t.Errorf("expected explicit time %v, got %v", explicit, events[0].Time)
	}
}

func TestEventBus_RingBuffer_Wrap(t *testing.T) {
	bus := NewEventBus(3)

	// Publish 5 events into a buffer of size 3
	for i := 0; i < 5; i++ {
		bus.Publish(Event{Type: EventReady, Message: itoa(i)})
	}

	events := bus.Recent()
	if len(events) != 3 {
		t.Fatalf("expected 3 events after wrap, got %d", len(events))
	}
	// Should have events 2, 3, 4 (oldest first)
	if events[0].Message != "2" {
		t.Errorf("expected oldest event '2', got %q", events[0].Message)
	}
	if events[1].Message != "3" {
		t.Errorf("expected middle event '3', got %q", events[1].Message)
	}
	if events[2].Message != "4" {
		t.Errorf("expected newest event '4', got %q", events[2].Message)
	}
}

func TestEventBus_RingBuffer_ExactFill(t *testing.T) {
	bus := NewEventBus(3)

	for i := 0; i < 3; i++ {
		bus.Publish(Event{Type: EventReady, Message: itoa(i)})
	}

	events := bus.Recent()
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	for i, e := range events {
		if e.Message != itoa(i) {
			t.Errorf("event[%d]: expected %q, got %q", i, itoa(i), e.Message)
		}
	}
}

func TestEventBus_Subscribe_ReceivesEvents(t *testing.T) {
	bus := NewEventBus(10)
	ch := bus.Subscribe()

	bus.Publish(Event{Type: EventReady, Message: "hello"})

	select {
	case e := <-ch:
		if e.Message != "hello" {
			t.Errorf("expected message 'hello', got %q", e.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}

	bus.Unsubscribe(ch)
}

func TestEventBus_Unsubscribe_ClosesChannel(t *testing.T) {
	bus := NewEventBus(10)
	ch := bus.Subscribe()
	bus.Unsubscribe(ch)

	// Channel should be closed
	_, ok := <-ch
	if ok {
		t.Fatal("expected channel to be closed after unsubscribe")
	}
}

func TestEventBus_Unsubscribe_StopsDelivery(t *testing.T) {
	bus := NewEventBus(10)
	ch := bus.Subscribe()
	bus.Unsubscribe(ch)

	// Publishing after unsubscribe should not panic
	bus.Publish(Event{Type: EventReady, Message: "after unsub"})
}

func TestEventBus_MultipleSubscribers(t *testing.T) {
	bus := NewEventBus(10)
	ch1 := bus.Subscribe()
	ch2 := bus.Subscribe()

	bus.Publish(Event{Type: EventReady, Message: "broadcast"})

	for _, ch := range []chan Event{ch1, ch2} {
		select {
		case e := <-ch:
			if e.Message != "broadcast" {
				t.Errorf("expected 'broadcast', got %q", e.Message)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for event")
		}
	}

	bus.Unsubscribe(ch1)
	bus.Unsubscribe(ch2)
}

func TestEventBus_SlowSubscriber_DoesNotBlock(t *testing.T) {
	bus := NewEventBus(10)
	// Subscribe but never read — channel buffer is 64
	ch := bus.Subscribe()

	// Publish more than the channel buffer
	for i := 0; i < 100; i++ {
		bus.Publish(Event{Type: EventReady, Message: itoa(i)})
	}
	// Should not deadlock; some events will be dropped for the slow subscriber

	bus.Unsubscribe(ch)
}
