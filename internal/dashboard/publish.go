package dashboard

import (
	"strconv"
	"time"
)

// Publish helpers for common events.

func PublishEndpointsChanged(bus *EventBus, service string, added, removed, total int) {
	if bus == nil {
		return
	}
	bus.Publish(Event{
		Type:    EventEndpointsChanged,
		Message: service,
		Data: map[string]string{
			"service": service,
			"added":   strconv.Itoa(added),
			"removed": strconv.Itoa(removed),
			"total":   strconv.Itoa(total),
		},
	})
}

func PublishGhostReload(bus *EventBus, vhosts, services, backends int) {
	if bus == nil {
		return
	}
	bus.Publish(Event{
		Type:    EventGhostReload,
		Message: "ghost.json regenerated and reloaded",
		Data: map[string]string{
			"vhosts":   strconv.Itoa(vhosts),
			"services": strconv.Itoa(services),
			"backends": strconv.Itoa(backends),
		},
	})
}

func PublishGhostReloadFail(bus *EventBus, err error) {
	if bus == nil {
		return
	}
	bus.Publish(Event{
		Type:    EventGhostReloadFail,
		Message: err.Error(),
	})
}

func PublishVCLReload(bus *EventBus, name string) {
	if bus == nil {
		return
	}
	bus.Publish(Event{
		Type:    EventVCLReload,
		Message: "VCL reloaded: " + name,
		Data:    map[string]string{"name": name},
	})
}

func PublishVCLReloadFail(bus *EventBus, err error) {
	if bus == nil {
		return
	}
	bus.Publish(Event{
		Type:    EventVCLReloadFail,
		Message: err.Error(),
	})
}

func PublishTLSReload(bus *EventBus, count int) {
	if bus == nil {
		return
	}
	bus.Publish(Event{
		Type:    EventTLSReload,
		Message: "TLS certificates reloaded",
		Data:    map[string]string{"count": strconv.Itoa(count)},
	})
}

func PublishTLSReloadFail(bus *EventBus, err error) {
	if bus == nil {
		return
	}
	bus.Publish(Event{
		Type:    EventTLSReloadFail,
		Message: err.Error(),
	})
}

func PublishConfigMapUpdate(bus *EventBus, name string) {
	if bus == nil {
		return
	}
	bus.Publish(Event{
		Type:    EventConfigMapUpdate,
		Time:    time.Now(),
		Message: "routing.json updated from ConfigMap " + name,
	})
}

func PublishVarnishConnected(bus *EventBus) {
	if bus == nil {
		return
	}
	bus.Publish(Event{
		Type:    EventVarnishConnected,
		Message: "varnishadm connection established",
	})
}
