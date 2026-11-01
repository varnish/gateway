package vcl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Operator -> chaperone contract for per-HTTPRoute VCL (#94).
//
// The gateway's "{gateway}-vcl" ConfigMap carries MainVCLKey (the master,
// GenerateMaster(N, gatewayUserVCL)) and PoolSizeKey (decimal N). Both are
// written in the same update, so the master in any given ConfigMap version
// needs exactly the labels gwvcl-0..N-1. PoolSizeKey absent or "0" means the
// feature is off and main.vcl is loaded as before.
//
// A separate "{gateway}-route-vcl" ConfigMap carries ManifestKey (a
// RouteVCLManifest as JSON) and, per occupied slot, SlotKey(slot) with the
// full route VCL document (GenerateRouteVCL output). The ConfigMap may be
// absent; that is an empty manifest.

const (
	// MainVCLKey is the "{gateway}-vcl" ConfigMap key holding the master VCL.
	MainVCLKey = "main.vcl"

	// PoolSizeKey is the "{gateway}-vcl" ConfigMap key holding the slot pool
	// size N (decimal). Absent or "0" disables route VCL.
	PoolSizeKey = "route-vcl-pool-size"

	// ManifestKey is the "{gateway}-route-vcl" ConfigMap key holding the
	// RouteVCLManifest.
	ManifestKey = "manifest.json"

	// RouteVCLConfigMapSuffix is appended to the Gateway name to form the
	// route VCL ConfigMap name.
	RouteVCLConfigMapSuffix = "-route-vcl"

	// routeVCLPrefix prefixes the vcl.load names of route VCLs:
	// route_<slot>_<first 12 hex of the document hash>.
	routeVCLPrefix = "route_"

	// routeVCLHashChars is how much of the document hash goes in the name.
	routeVCLHashChars = 12
)

// SlotKey returns the "{gateway}-route-vcl" ConfigMap key holding the route
// VCL document for a slot ("slot-7.vcl").
func SlotKey(slot int) string {
	return "slot-" + strconv.Itoa(slot) + ".vcl"
}

// Reasons a policy is listed in RouteVCLManifest.Unassigned.
const (
	// UnassignedNoSlotAvailable: every slot in the pool is occupied.
	UnassignedNoSlotAvailable = "NoSlotAvailable"
	// UnassignedSlotOutsidePool: the policy held a slot at or above a
	// reduced pool size, and no free slot inside the pool was left for it.
	UnassignedSlotOutsidePool = "SlotOutsidePool"
	// UnassignedConfigMapFull: adding the document would push the route VCL
	// ConfigMap past the operator's size guard.
	UnassignedConfigMapFull = "ConfigMapFull"
)

// RouteVCLManifest describes which slot holds which policy's VCL.
type RouteVCLManifest struct {
	// Slots maps the decimal slot number to its occupant.
	Slots map[string]RouteVCLSlot `json:"slots"`
	// Unassigned lists eligible policies that did not get a slot. The
	// chaperone ignores it; the operator uses it for status and metrics.
	Unassigned []RouteVCLUnassigned `json:"unassigned,omitempty"`
	// Released lists slots whose occupant left and that are not free for
	// another policy yet. The chaperone ignores it: a released slot is not
	// in Slots, so its label is pointed at the placeholder and ghost drops
	// it from routing. The operator keeps it out of the free list until the
	// previous occupant's VCL can no longer be reached through it.
	Released []RouteVCLReleased `json:"released,omitempty"`
}

// RouteVCLReleased is a slot in quarantine after its occupant left.
type RouteVCLReleased struct {
	// Slot is the slot number.
	Slot int `json:"slot"`
	// Policy is the previous occupant as "<namespace>/<name>".
	Policy string `json:"policy"`
	// Route is the previous occupant's HTTPRoute as "<namespace>/<name>".
	Route string `json:"route"`
	// Hash is the manifest hash the slot had when it was released.
	Hash string `json:"hash"`
	// ReleasedAt is when the operator released the slot.
	ReleasedAt metav1.Time `json:"releasedAt"`
}

// RouteVCLUnassigned is an eligible policy that has no slot on the gateway.
type RouteVCLUnassigned struct {
	// Policy is the VarnishVCLPolicy as "<namespace>/<name>".
	Policy string `json:"policy"`
	// Route is the policy's target HTTPRoute as "<namespace>/<name>".
	Route string `json:"route"`
	// Reason is one of the Unassigned* constants.
	Reason string `json:"reason"`
}

// RouteVCLSlot is one occupied slot in a RouteVCLManifest.
type RouteVCLSlot struct {
	// Policy is the VarnishVCLPolicy as "<namespace>/<name>".
	Policy string `json:"policy"`
	// Route is the policy's target HTTPRoute as "<namespace>/<name>". The
	// operator matches it against ghost route names to set vcl_slot.
	Route string `json:"route"`
	// Generation is the policy's metadata.generation the document was built from.
	Generation int64 `json:"generation"`
	// Hash is RouteVCLHash of the slot's document.
	Hash string `json:"hash"`
}

// RouteVCLHash returns the manifest hash of a route VCL document: lowercase
// hex SHA-256 of its bytes.
func RouteVCLHash(document string) string {
	sum := sha256.Sum256([]byte(document))
	return hex.EncodeToString(sum[:])
}

// ParseRouteVCLManifest decodes ManifestKey. An empty string is an empty
// manifest.
func ParseRouteVCLManifest(data string) (RouteVCLManifest, error) {
	var m RouteVCLManifest
	if strings.TrimSpace(data) == "" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		return RouteVCLManifest{}, fmt.Errorf("json.Unmarshal(%s): %w", ManifestKey, err)
	}
	return m, nil
}

// ParsePoolSize decodes PoolSizeKey. An empty string is 0.
func ParsePoolSize(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("strconv.Atoi(%s=%q): %w", PoolSizeKey, s, err)
	}
	if n < 0 || n > maxPoolSize {
		return 0, fmt.Errorf("%s=%d out of range 0..%d", PoolSizeKey, n, maxPoolSize)
	}
	return n, nil
}

// routeVCLName is the vcl.load name for a slot's document.
func routeVCLName(slot int, hash string) string {
	return routeVCLPrefix + strconv.Itoa(slot) + "_" + hash[:routeVCLHashChars]
}

// parseSlotLabel returns the slot number of a "gwvcl-<n>" label name.
func parseSlotLabel(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, "gwvcl-")
	if !ok || rest == "" {
		return 0, false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(rest)
	if err != nil || SlotLabel(n) != name {
		return 0, false
	}
	return n, true
}
