package dashboard

import (
	"errors"
	"testing"
	"time"
)

func TestItoa(t *testing.T) {
	tests := []struct {
		input    int
		expected string
	}{
		{0, "0"},
		{1, "1"},
		{42, "42"},
		{100, "100"},
		{-1, "-1"},
		{-42, "-42"},
		{999999, "999999"},
	}
	for _, tc := range tests {
		got := itoa(tc.input)
		if got != tc.expected {
			t.Errorf("itoa(%d) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestPublish_NilBus(t *testing.T) {
	// All publish helpers should be safe to call with a nil bus
	PublishEndpointsChanged(nil, "test", 1, 0, 1)
	PublishGhostReload(nil, 1, 1, 1)
	PublishGhostReloadFail(nil, errors.New("fail"))
	PublishVCLReload(nil, "test")
	PublishVCLReloadFail(nil, errors.New("fail"))
	PublishTLSReload(nil, 1)
	PublishTLSReloadFail(nil, errors.New("fail"))
	PublishConfigMapUpdate(nil, "test")
	PublishVarnishConnected(nil)
	// Should not panic
}

func drainOne(t *testing.T, ch chan Event) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return Event{} // unreachable
	}
}

func TestPublishEndpointsChanged(t *testing.T) {
	bus := NewEventBus(10)
	ch := bus.Subscribe()

	PublishEndpointsChanged(bus, "default/api", 2, 1, 3)

	e := drainOne(t, ch)
	if e.Type != EventEndpointsChanged {
		t.Errorf("expected EventEndpointsChanged, got %s", e.Type)
	}
	if e.Message != "default/api" {
		t.Errorf("expected message 'default/api', got %q", e.Message)
	}
	if e.Data["added"] != "2" {
		t.Errorf("expected added=2, got %q", e.Data["added"])
	}
	if e.Data["removed"] != "1" {
		t.Errorf("expected removed=1, got %q", e.Data["removed"])
	}
	if e.Data["total"] != "3" {
		t.Errorf("expected total=3, got %q", e.Data["total"])
	}
	bus.Unsubscribe(ch)
}

func TestPublishGhostReload(t *testing.T) {
	bus := NewEventBus(10)
	ch := bus.Subscribe()

	PublishGhostReload(bus, 5, 3, 10)

	e := drainOne(t, ch)
	if e.Type != EventGhostReload {
		t.Errorf("expected EventGhostReload, got %s", e.Type)
	}
	if e.Data["vhosts"] != "5" {
		t.Errorf("expected vhosts=5, got %q", e.Data["vhosts"])
	}
	if e.Data["services"] != "3" {
		t.Errorf("expected services=3, got %q", e.Data["services"])
	}
	if e.Data["backends"] != "10" {
		t.Errorf("expected backends=10, got %q", e.Data["backends"])
	}
	bus.Unsubscribe(ch)
}

func TestPublishGhostReloadFail(t *testing.T) {
	bus := NewEventBus(10)
	ch := bus.Subscribe()

	PublishGhostReloadFail(bus, errors.New("connection refused"))

	e := drainOne(t, ch)
	if e.Type != EventGhostReloadFail {
		t.Errorf("expected EventGhostReloadFail, got %s", e.Type)
	}
	if e.Message != "connection refused" {
		t.Errorf("expected error message, got %q", e.Message)
	}
	bus.Unsubscribe(ch)
}

func TestPublishVCLReload(t *testing.T) {
	bus := NewEventBus(10)
	ch := bus.Subscribe()

	PublishVCLReload(bus, "vcl_001")

	e := drainOne(t, ch)
	if e.Type != EventVCLReload {
		t.Errorf("expected EventVCLReload, got %s", e.Type)
	}
	if e.Data["name"] != "vcl_001" {
		t.Errorf("expected name=vcl_001, got %q", e.Data["name"])
	}
	bus.Unsubscribe(ch)
}

func TestPublishVCLReloadFail(t *testing.T) {
	bus := NewEventBus(10)
	ch := bus.Subscribe()

	PublishVCLReloadFail(bus, errors.New("syntax error"))

	e := drainOne(t, ch)
	if e.Type != EventVCLReloadFail {
		t.Errorf("expected EventVCLReloadFail, got %s", e.Type)
	}
	if e.Message != "syntax error" {
		t.Errorf("expected error message, got %q", e.Message)
	}
	bus.Unsubscribe(ch)
}

func TestPublishTLSReload(t *testing.T) {
	bus := NewEventBus(10)
	ch := bus.Subscribe()

	PublishTLSReload(bus, 3)

	e := drainOne(t, ch)
	if e.Type != EventTLSReload {
		t.Errorf("expected EventTLSReload, got %s", e.Type)
	}
	if e.Data["count"] != "3" {
		t.Errorf("expected count=3, got %q", e.Data["count"])
	}
	bus.Unsubscribe(ch)
}

func TestPublishTLSReloadFail(t *testing.T) {
	bus := NewEventBus(10)
	ch := bus.Subscribe()

	PublishTLSReloadFail(bus, errors.New("cert expired"))

	e := drainOne(t, ch)
	if e.Type != EventTLSReloadFail {
		t.Errorf("expected EventTLSReloadFail, got %s", e.Type)
	}
	bus.Unsubscribe(ch)
}

func TestPublishConfigMapUpdate(t *testing.T) {
	bus := NewEventBus(10)
	ch := bus.Subscribe()

	PublishConfigMapUpdate(bus, "my-gateway-vcl")

	e := drainOne(t, ch)
	if e.Type != EventConfigMapUpdate {
		t.Errorf("expected EventConfigMapUpdate, got %s", e.Type)
	}
	if e.Message != "routing.json updated from ConfigMap my-gateway-vcl" {
		t.Errorf("unexpected message: %q", e.Message)
	}
	bus.Unsubscribe(ch)
}

func TestPublishVarnishConnected(t *testing.T) {
	bus := NewEventBus(10)
	ch := bus.Subscribe()

	PublishVarnishConnected(bus)

	e := drainOne(t, ch)
	if e.Type != EventVarnishConnected {
		t.Errorf("expected EventVarnishConnected, got %s", e.Type)
	}
	bus.Unsubscribe(ch)
}
