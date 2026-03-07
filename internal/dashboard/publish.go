package dashboard

import "time"

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
			"added":   itoa(added),
			"removed": itoa(removed),
			"total":   itoa(total),
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
			"vhosts":   itoa(vhosts),
			"services": itoa(services),
			"backends": itoa(backends),
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
		Data:    map[string]string{"count": itoa(count)},
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

func itoa(n int) string {
	// Avoid importing strconv for this simple helper
	if n == 0 {
		return "0"
	}
	if n < 0 {
		return "-" + itoa(-n)
	}
	digits := make([]byte, 0, 10)
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	// Reverse
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}
