package dashboard

import (
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
		"prod/zebra": {Name: "zebra", Namespace: "prod"},
		"default/api": {Name: "api", Namespace: "default"},
		"prod/alpha": {Name: "alpha", Namespace: "prod"},
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
