package dashboard

import (
	"sort"
	"sync"
	"time"
)

// ServiceState represents the current state of a backend service.
type ServiceState struct {
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Backends  []BackendState  `json:"backends"`
}

// BackendState represents a single backend endpoint.
type BackendState struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// VHostState represents a virtual host in the current config.
type VHostState struct {
	Hostname string   `json:"hostname"`
	Routes   int      `json:"routes"`
	Services []string `json:"services"` // namespace/name keys
}

// Snapshot is a point-in-time view of the chaperone state.
type Snapshot struct {
	Ready    bool           `json:"ready"`
	Draining bool           `json:"draining"`
	Uptime   string         `json:"uptime"`
	Version  string         `json:"version"`
	VHosts   []VHostState   `json:"vhosts"`
	Services []ServiceState `json:"services"`
	Events   []Event        `json:"events"`
}

// StateTracker aggregates current state from events and direct updates.
type StateTracker struct {
	mu        sync.RWMutex
	ready     bool
	draining  bool
	version   string
	startTime time.Time
	services  map[string]ServiceState // key: namespace/name
	vhosts    map[string]VHostState   // key: hostname
	bus       *EventBus
}

// NewStateTracker creates a state tracker connected to the given event bus.
func NewStateTracker(bus *EventBus, version string) *StateTracker {
	return &StateTracker{
		startTime: time.Now(),
		version:   version,
		services:  make(map[string]ServiceState),
		vhosts:    make(map[string]VHostState),
		bus:       bus,
	}
}

// SetReady marks the gateway as ready and publishes an event.
func (s *StateTracker) SetReady() {
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	s.bus.Publish(Event{
		Type:    EventReady,
		Message: "Gateway is ready to serve traffic",
	})
}

// SetDraining marks the gateway as draining and publishes an event.
func (s *StateTracker) SetDraining() {
	s.mu.Lock()
	s.draining = true
	s.mu.Unlock()
	s.bus.Publish(Event{
		Type:    EventDraining,
		Message: "Gateway is draining connections",
	})
}

// IsReady returns true if the gateway is ready.
func (s *StateTracker) IsReady() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready
}

// IsDraining returns true if the gateway is draining.
func (s *StateTracker) IsDraining() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.draining
}

// UpdateServices replaces the full service/backend state.
func (s *StateTracker) UpdateServices(services map[string]ServiceState) {
	s.mu.Lock()
	s.services = services
	s.mu.Unlock()
}

// UpdateVHosts replaces the full vhost state.
func (s *StateTracker) UpdateVHosts(vhosts map[string]VHostState) {
	s.mu.Lock()
	s.vhosts = vhosts
	s.mu.Unlock()
}

// Snapshot returns a point-in-time view of all state.
func (s *StateTracker) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	services := make([]ServiceState, 0, len(s.services))
	for _, svc := range s.services {
		services = append(services, svc)
	}
	sort.Slice(services, func(i, j int) bool {
		if services[i].Namespace != services[j].Namespace {
			return services[i].Namespace < services[j].Namespace
		}
		return services[i].Name < services[j].Name
	})

	vhosts := make([]VHostState, 0, len(s.vhosts))
	for _, vh := range s.vhosts {
		vhosts = append(vhosts, vh)
	}
	sort.Slice(vhosts, func(i, j int) bool {
		return vhosts[i].Hostname < vhosts[j].Hostname
	})

	return Snapshot{
		Ready:    s.ready,
		Draining: s.draining,
		Uptime:   time.Since(s.startTime).Truncate(time.Second).String(),
		Version:  s.version,
		VHosts:   vhosts,
		Services: services,
		Events:   s.bus.Recent(),
	}
}
