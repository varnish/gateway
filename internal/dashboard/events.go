package dashboard

import (
	"sync"
	"time"
)

// EventType identifies the kind of event.
type EventType string

const (
	EventEndpointsChanged EventType = "endpoints_changed"
	EventGhostReload      EventType = "ghost_reload"
	EventGhostReloadFail  EventType = "ghost_reload_fail"
	EventVCLReload        EventType = "vcl_reload"
	EventVCLReloadFail    EventType = "vcl_reload_fail"
	EventTLSReload        EventType = "tls_reload"
	EventTLSReloadFail    EventType = "tls_reload_fail"
	EventConfigMapUpdate  EventType = "configmap_update"
	EventReady            EventType = "ready"
	EventDraining         EventType = "draining"
	EventVarnishConnected EventType = "varnish_connected"
)

// Event represents something that happened in the chaperone.
type Event struct {
	Type    EventType         `json:"type"`
	Time    time.Time         `json:"time"`
	Message string            `json:"message"`
	Data    map[string]string `json:"data,omitempty"`
}

// EventBus is a simple pub/sub for dashboard events.
// It keeps a ring buffer of recent events and notifies subscribers.
type EventBus struct {
	mu          sync.RWMutex
	ring        []Event
	size        int
	pos         int
	full        bool
	subscribers map[chan Event]struct{}
}

// NewEventBus creates an event bus with the given ring buffer capacity.
func NewEventBus(capacity int) *EventBus {
	return &EventBus{
		ring:        make([]Event, capacity),
		size:        capacity,
		subscribers: make(map[chan Event]struct{}),
	}
}

// Publish sends an event to all subscribers and stores it in the ring buffer.
func (b *EventBus) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}

	b.mu.Lock()
	// Store in ring buffer
	b.ring[b.pos] = e
	b.pos = (b.pos + 1) % b.size
	if b.pos == 0 {
		b.full = true
	}

	// Copy subscriber set under lock
	subs := make([]chan Event, 0, len(b.subscribers))
	for ch := range b.subscribers {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	// Send to subscribers outside lock (non-blocking)
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
			// Subscriber too slow, drop event
		}
	}
}

// Subscribe returns a channel that receives future events.
// The channel has a buffer to absorb short bursts.
// Call Unsubscribe when done.
func (b *EventBus) Subscribe() chan Event {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel.
func (b *EventBus) Unsubscribe(ch chan Event) {
	b.mu.Lock()
	delete(b.subscribers, ch)
	b.mu.Unlock()
	close(ch)
}

// Recent returns all events in the ring buffer, oldest first.
func (b *EventBus) Recent() []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if !b.full && b.pos == 0 {
		return nil
	}

	var events []Event
	if b.full {
		// Ring wrapped: pos..end, then 0..pos
		events = make([]Event, 0, b.size)
		events = append(events, b.ring[b.pos:]...)
		events = append(events, b.ring[:b.pos]...)
	} else {
		events = make([]Event, b.pos)
		copy(events, b.ring[:b.pos])
	}
	return events
}
