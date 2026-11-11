package dashboard

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNewStateTracker(t *testing.T) {
	bus := NewEventBus(10)
	st := NewStateTracker(bus, "v1.0.0")

	if st.IsReady() {
		t.Error("new tracker should not be ready")
	}
	if st.IsDraining() {
		t.Error("new tracker should not be draining")
	}
}

func TestStateTracker_SetReady(t *testing.T) {
	bus := NewEventBus(10)
	ch := bus.Subscribe()
	st := NewStateTracker(bus, "v1.0.0")

	st.SetReady()

	if !st.IsReady() {
		t.Error("expected ready after SetReady")
	}

	// Should have published a ready event
	select {
	case e := <-ch:
		if e.Type != EventReady {
			t.Errorf("expected EventReady, got %s", e.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ready event")
	}
	bus.Unsubscribe(ch)
}

func TestStateTracker_SetDraining(t *testing.T) {
	bus := NewEventBus(10)
	ch := bus.Subscribe()
	st := NewStateTracker(bus, "v1.0.0")

	st.SetDraining()

	if !st.IsDraining() {
		t.Error("expected draining after SetDraining")
	}

	select {
	case e := <-ch:
		if e.Type != EventDraining {
			t.Errorf("expected EventDraining, got %s", e.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for draining event")
	}
	bus.Unsubscribe(ch)
}

func TestStateTracker_UpdateServices(t *testing.T) {
	bus := NewEventBus(10)
	st := NewStateTracker(bus, "v1.0.0")

	services := map[string]ServiceState{
		"default/api": {
			Name:      "api",
			Namespace: "default",
			Backends: []BackendState{
				{Address: "10.0.0.1", Port: 8080},
				{Address: "10.0.0.2", Port: 8080},
			},
		},
	}
	st.UpdateServices(services)

	snap := st.Snapshot()
	if len(snap.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(snap.Services))
	}
	if snap.Services[0].Name != "api" {
		t.Errorf("expected service name 'api', got %q", snap.Services[0].Name)
	}
	if len(snap.Services[0].Backends) != 2 {
		t.Errorf("expected 2 backends, got %d", len(snap.Services[0].Backends))
	}
}

func TestStateTracker_UpdateVHosts(t *testing.T) {
	bus := NewEventBus(10)
	st := NewStateTracker(bus, "v1.0.0")

	vhosts := map[string]VHostState{
		"api.example.com": {
			Hostname: "api.example.com",
			Routes:   3,
			Services: []string{"default/api"},
		},
	}
	st.UpdateVHosts(vhosts)

	snap := st.Snapshot()
	if len(snap.VHosts) != 1 {
		t.Fatalf("expected 1 vhost, got %d", len(snap.VHosts))
	}
	if snap.VHosts[0].Hostname != "api.example.com" {
		t.Errorf("expected hostname 'api.example.com', got %q", snap.VHosts[0].Hostname)
	}
}

func TestStateTracker_Snapshot_SortsServices(t *testing.T) {
	bus := NewEventBus(10)
	st := NewStateTracker(bus, "v1.0.0")

	st.UpdateServices(map[string]ServiceState{
		"prod/zebra":  {Name: "zebra", Namespace: "prod"},
		"default/api": {Name: "api", Namespace: "default"},
		"prod/alpha":  {Name: "alpha", Namespace: "prod"},
	})

	snap := st.Snapshot()
	if len(snap.Services) != 3 {
		t.Fatalf("expected 3 services, got %d", len(snap.Services))
	}
	// Sorted by namespace, then name
	expected := []struct{ ns, name string }{
		{"default", "api"},
		{"prod", "alpha"},
		{"prod", "zebra"},
	}
	for i, exp := range expected {
		if snap.Services[i].Namespace != exp.ns || snap.Services[i].Name != exp.name {
			t.Errorf("services[%d]: expected %s/%s, got %s/%s",
				i, exp.ns, exp.name, snap.Services[i].Namespace, snap.Services[i].Name)
		}
	}
}

func TestStateTracker_Snapshot_SortsVHosts(t *testing.T) {
	bus := NewEventBus(10)
	st := NewStateTracker(bus, "v1.0.0")

	st.UpdateVHosts(map[string]VHostState{
		"zebra.example.com": {Hostname: "zebra.example.com"},
		"alpha.example.com": {Hostname: "alpha.example.com"},
	})

	snap := st.Snapshot()
	if snap.VHosts[0].Hostname != "alpha.example.com" {
		t.Errorf("expected alpha first, got %q", snap.VHosts[0].Hostname)
	}
	if snap.VHosts[1].Hostname != "zebra.example.com" {
		t.Errorf("expected zebra second, got %q", snap.VHosts[1].Hostname)
	}
}

func TestStateTracker_Snapshot_IncludesVersion(t *testing.T) {
	bus := NewEventBus(10)
	st := NewStateTracker(bus, "v2.3.4")
	snap := st.Snapshot()
	if snap.Version != "v2.3.4" {
		t.Errorf("expected version 'v2.3.4', got %q", snap.Version)
	}
}

func TestStateTracker_Snapshot_IncludesUptime(t *testing.T) {
	bus := NewEventBus(10)
	st := NewStateTracker(bus, "v1.0.0")
	// Snapshot should have a non-empty uptime
	snap := st.Snapshot()
	if snap.Uptime == "" {
		t.Error("expected non-empty uptime")
	}
}

func TestStateTracker_Snapshot_IncludesEvents(t *testing.T) {
	bus := NewEventBus(10)
	st := NewStateTracker(bus, "v1.0.0")

	bus.Publish(Event{Type: EventVCLReload, Message: "test"})

	snap := st.Snapshot()
	if len(snap.Events) != 1 {
		t.Fatalf("expected 1 event in snapshot, got %d", len(snap.Events))
	}
	if snap.Events[0].Type != EventVCLReload {
		t.Errorf("expected EventVCLReload, got %s", snap.Events[0].Type)
	}
}

func TestStateTracker_Snapshot_EmptyCollections(t *testing.T) {
	bus := NewEventBus(10)
	st := NewStateTracker(bus, "v1.0.0")
	snap := st.Snapshot()

	// Empty but non-nil slices for services and vhosts
	if snap.Services == nil {
		t.Error("expected non-nil services slice")
	}
	if snap.VHosts == nil {
		t.Error("expected non-nil vhosts slice")
	}
	if len(snap.Services) != 0 {
		t.Errorf("expected 0 services, got %d", len(snap.Services))
	}
	if len(snap.VHosts) != 0 {
		t.Errorf("expected 0 vhosts, got %d", len(snap.VHosts))
	}
}

func TestStateTracker_RouteVCL(t *testing.T) {
	bus := NewEventBus(16)
	st := NewStateTracker(bus, "v")
	if st.Snapshot().RouteVCL != nil {
		t.Fatal("RouteVCL set before the first slot pass")
	}

	// A snapshot published before Run subscribes is replayed.
	PublishRouteVCLSlots(bus, RouteVCLState{PoolSize: 4, MasterVCL: "vcl_a"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- st.Run(ctx) }()

	waitFor := func(cond func(*RouteVCLState) bool) *RouteVCLState {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if r := st.Snapshot().RouteVCL; r != nil && cond(r) {
				return r
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("route VCL state did not arrive")
		return nil
	}
	waitFor(func(r *RouteVCLState) bool { return r.MasterVCL == "vcl_a" })

	PublishRouteVCLSlots(bus, RouteVCLState{PoolSize: 4, MasterVCL: "vcl_b", Slots: []RouteVCLSlot{
		{Slot: 1, Policy: "ns/p", Route: "ns/r", VCLName: "route_1_abc", State: RouteVCLProgrammed},
		{Slot: 2, Policy: "ns/q", Route: "ns/s", State: RouteVCLFailed, Message: "boom"},
	}})
	r := waitFor(func(r *RouteVCLState) bool { return r.MasterVCL == "vcl_b" })
	if len(r.Slots) != 2 || r.Slots[1].Message != "boom" || r.Slots[0].VCLName != "route_1_abc" {
		t.Errorf("slots = %+v", r.Slots)
	}
	if p, f, ph := r.Counts(); p != 1 || f != 1 || ph != 2 {
		t.Errorf("counts = %d/%d/%d, want 1/1/2", p, f, ph)
	}

	// The snapshot is a copy: mutating it does not touch the tracker.
	r.Slots[0].Policy = "mutated"
	if got := st.Snapshot().RouteVCL.Slots[0].Policy; got != "ns/p" {
		t.Errorf("snapshot aliases tracker state: %q", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}

func TestRouteVCLEvents(t *testing.T) {
	bus := NewEventBus(8)
	PublishRouteVCLReload(bus, 3, "route_3_x", 250*time.Millisecond)
	PublishRouteVCLReload(bus, 4, "route_4_x", 0)
	PublishRouteVCLReloadFail(bus, 5, errors.New("bad"), "", 0)
	ev := bus.Recent()
	if len(ev) != 3 {
		t.Fatalf("events = %d", len(ev))
	}
	if ev[0].Type != EventRouteVCLReload || ev[0].Data["slot"] != "3" || ev[0].Data["compile_seconds"] != "0.25" || ev[0].Data["result"] != "ok" {
		t.Errorf("reload event %+v", ev[0])
	}
	if _, ok := ev[1].Data["compile_seconds"]; ok {
		t.Errorf("label-only reload carries a compile time: %+v", ev[1])
	}
	if ev[2].Type != EventRouteVCLReloadFail || ev[2].Data["result"] != "error" || ev[2].Message != "bad" {
		t.Errorf("fail event %+v", ev[2])
	}
	// nil bus is a no-op, like the other publishers.
	PublishRouteVCLReload(nil, 0, "x", 0)
	PublishRouteVCLReloadFail(nil, 0, errors.New("x"), "", 0)
	PublishRouteVCLSlots(nil, RouteVCLState{})
}
