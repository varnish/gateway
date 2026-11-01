package dashboard

import (
	"encoding/json"
	"strconv"
	"time"
)

// Results of a route VCL load, as carried in the Data["result"] of route VCL
// reload events and used as the `result` label of the chaperone metrics.
const (
	RouteVCLOK           = "ok"
	RouteVCLCompileError = "compile_error" // VCC rejected the document
	RouteVCLOtherError   = "error"         // refused for another reason, or vcl.label failed
)

// Slot states of RouteVCLSlot.State.
const (
	RouteVCLProgrammed = "programmed"
	RouteVCLFailed     = "failed" // compile failed (or waiting); the slot serves its previous VCL or the placeholder
)

// RouteVCLSlot is one populated route VCL slot of the pool.
type RouteVCLSlot struct {
	Slot    int    `json:"slot"`    // -1 when the manifest key is not a slot number
	Policy  string `json:"policy"`  // namespace/name of the VarnishVCLPolicy
	Route   string `json:"route"`   // namespace/name of the HTTPRoute
	VCLName string `json:"vclName"` // VCL the slot label points at ("" = placeholder)
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

// RouteVCLState is the route VCL view of one chaperone: the pool, the active
// master and every populated slot. The other slots of the pool answer from
// the placeholder.
type RouteVCLState struct {
	PoolSize  int            `json:"poolSize"`
	MasterVCL string         `json:"masterVcl"`
	Slots     []RouteVCLSlot `json:"slots"`
}

// Counts returns how many slots are programmed, failed and (the rest of the
// pool) still on the placeholder.
func (s RouteVCLState) Counts() (programmed, failed, placeholder int) {
	inPool := 0
	for _, sl := range s.Slots {
		if sl.State == RouteVCLProgrammed {
			programmed++
		} else {
			failed++
		}
		if sl.Slot >= 0 && sl.Slot < s.PoolSize {
			inPool++
		}
	}
	return programmed, failed, max(0, s.PoolSize-inPool)
}

// PublishRouteVCLReload reports a route VCL that is now labelled for its
// slot. compile is how long the vcl.inline took, 0 when the VCL was already
// loaded (only the label was retried) and nothing was compiled.
func PublishRouteVCLReload(bus *EventBus, slot int, name string, compile time.Duration) {
	if bus == nil {
		return
	}
	data := map[string]string{
		"slot":   strconv.Itoa(slot),
		"name":   name,
		"result": RouteVCLOK,
	}
	if compile > 0 {
		data["compile_seconds"] = strconv.FormatFloat(compile.Seconds(), 'f', -1, 64)
	}
	bus.Publish(Event{
		Type:    EventRouteVCLReload,
		Message: "Route VCL programmed: slot " + strconv.Itoa(slot) + " -> " + name,
		Data:    data,
	})
}

// PublishRouteVCLReloadFail reports a route VCL that could not be loaded or
// labelled. result is RouteVCLCompileError or RouteVCLOtherError; compile is
// the vcl.inline duration, 0 when no compile was attempted.
func PublishRouteVCLReloadFail(bus *EventBus, slot int, err error, result string, compile time.Duration) {
	if bus == nil {
		return
	}
	if result == "" {
		result = RouteVCLOtherError
	}
	data := map[string]string{
		"slot":   strconv.Itoa(slot),
		"result": result,
	}
	if compile > 0 {
		data["compile_seconds"] = strconv.FormatFloat(compile.Seconds(), 'f', -1, 64)
	}
	bus.Publish(Event{
		Type:    EventRouteVCLReloadFail,
		Message: err.Error(),
		Data:    data,
	})
}

// PublishRouteVCLSlots carries the route VCL state after a slot pass to the
// state tracker and the metrics updater.
func PublishRouteVCLSlots(bus *EventBus, st RouteVCLState) {
	if bus == nil {
		return
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return
	}
	programmed, failed, placeholder := st.Counts()
	bus.Publish(Event{
		Type: EventRouteVCLSlots,
		Message: "Route VCL slots: " + strconv.Itoa(programmed) + " programmed, " +
			strconv.Itoa(failed) + " failed, " + strconv.Itoa(placeholder) + " placeholder",
		Data: map[string]string{"snapshot": string(raw)},
	})
}

// RouteVCLState decodes the snapshot of an EventRouteVCLSlots event.
func (e Event) RouteVCLState() (RouteVCLState, bool) {
	var st RouteVCLState
	raw, ok := e.Data["snapshot"]
	if e.Type != EventRouteVCLSlots || !ok {
		return st, false
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return st, false
	}
	return st, true
}
