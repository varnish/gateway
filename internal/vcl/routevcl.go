package vcl

import (
	"strconv"
	"strings"
)

// Per-HTTPRoute VCL (#94).
//
// Layout inside varnishd:
//
//	gwvcl-placeholder   GeneratePlaceholder(), what unused or failed slots point at
//	gwvcl-0..N-1        labels, one per slot, each pointing at a route VCL or the placeholder
//	master (active)     GenerateMaster(N, gatewayUserVCL), jumps to gwvcl-<slot>
//
// Ghost sets DispatchHeader to the matched route's slot number; the master's
// vcl_recv turns that into return (vcl(gwvcl-<slot>)). A route VCL is
// GenerateRouteVCL(gatewayUserVCL, routeVCL): the same preamble without the
// dispatch block, so ghost routes the request again inside the route VCL and
// the trust-boundary unsets run again after Req_Rollback.

// PlaceholderVCLName is the vcl.load name of the placeholder VCL that unused
// or failed slot labels point at.
const PlaceholderVCLName = "gwvcl-placeholder"

// DispatchHeader is the request header ghost sets to the matched route's VCL
// slot number (decimal, no padding).
const DispatchHeader = "X-Ghost-VCL-Slot"

// SlotLabel returns the VCL label name for a slot. The number is formatted the
// same way ghost formats DispatchHeader (decimal, no padding).
func SlotLabel(slot int) string {
	return "gwvcl-" + strconv.Itoa(slot)
}

// dispatchBlock renders the vcl_recv fragment that jumps to a slot's label.
// It returns "" for slotPoolSize <= 0, which removes the sentinel line and
// leaves the preamble exactly as Generate() renders it.
//
// The slot lookup is a balanced binary tree on std.integer(header, -1) rather
// than a linear if-chain: P1b measured a 256-entry linear chain at +330 ns for
// the last slot (-23% synthetic throughput), while the tree was flat across
// slots. Every pool size uses the tree so there is one code path. A value
// outside 0..slotPoolSize-1 (or not a number, -1) reaches a leaf whose
// equality test fails and falls through to the unset.
func dispatchBlock(slotPoolSize int) string {
	if slotPoolSize <= 0 {
		return ""
	}
	hdr := "req.http." + DispatchHeader
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString("    # Route VCL dispatch: ghost set " + DispatchHeader + " for the matched route.\n")
	sb.WriteString("    # return(vcl) is illegal after a restart (503) and ESI subrequests always\n")
	sb.WriteString("    # run in the master, so guard both.\n")
	sb.WriteString("    if (" + hdr + " && req.restarts == 0 && req.esi_level == 0) {\n")
	writeDispatchTree(&sb, "std.integer("+hdr+", -1)", 0, slotPoolSize, 2)
	sb.WriteString("        # Slot outside the pool: stay in the master and do not leak the\n")
	sb.WriteString("        # header to user VCL or the backend.\n")
	sb.WriteString("        unset " + hdr + ";\n")
	sb.WriteString("    }\n")
	return sb.String()
}

// writeDispatchTree writes a balanced binary search over slots [lo, hi) at the
// given indent depth (4 spaces per level). Leaves are equality tests.
func writeDispatchTree(sb *strings.Builder, expr string, lo, hi, depth int) {
	ind := strings.Repeat("    ", depth)
	if hi-lo == 1 {
		sb.WriteString(ind + "if (" + expr + " == " + strconv.Itoa(lo) + ") { return (vcl(" + SlotLabel(lo) + ")); }\n")
		return
	}
	mid := lo + (hi-lo)/2
	sb.WriteString(ind + "if (" + expr + " < " + strconv.Itoa(mid) + ") {\n")
	writeDispatchTree(sb, expr, lo, mid, depth+1)
	sb.WriteString(ind + "} else {\n")
	writeDispatchTree(sb, expr, mid, hi, depth+1)
	sb.WriteString(ind + "}\n")
}

// GenerateMaster returns the active ("master") VCL: preamble with a dispatch
// block for slots 0..slotPoolSize-1, then the gateway user VCL, then the
// postamble. With slotPoolSize == 0 the output is byte-identical to
// Merge(Generate(), gatewayUserVCL).
//
// Every label gwvcl-0..gwvcl-<slotPoolSize-1> must exist before the master is
// loaded, or vcl.load fails with "Symbol not found".
func GenerateMaster(slotPoolSize int, gatewayUserVCL string) string {
	return Merge(mustGeneratePreamble(DefaultGhostConfigPath, slotPoolSize), gatewayUserVCL)
}

// generateMaster is GenerateMaster with a custom ghost config path (tests).
func generateMaster(ghostConfigPath string, slotPoolSize int, gatewayUserVCL string) (string, error) {
	preamble, err := generatePreamble(ghostConfigPath, slotPoolSize)
	if err != nil {
		return "", err
	}
	return Merge(preamble, gatewayUserVCL), nil
}

// GenerateRouteVCL returns the VCL loaded behind a slot label: preamble (no
// dispatch block), route VCL, gateway user VCL, postamble (gate G5: compose).
// The preamble's trust-boundary unsets are part of it on purpose: the jump
// from the master rolls req back to the client's headers, so they must be
// stripped again before ghost runs.
//
// The route VCL comes before the gateway user VCL. VCL concatenates
// same-named subroutines in document order and a return ends the
// subroutine, so whichever document comes first can cut the other off. The
// gateway VCL commonly ends vcl_recv with a return (the shipped default is
// `sub vcl_recv { return (pass); }`); placed first, it kept every route
// vcl_recv from running. Route first, the route's code runs in every
// subroutine and the gateway VCL still runs after it (and keeps the last
// word) unless the route returns, the same rule the gateway VCL already has
// toward the postamble.
func GenerateRouteVCL(gatewayUserVCL, routeVCL string) string {
	return Merge(mustGeneratePreamble(DefaultGhostConfigPath, 0), routeVCL, gatewayUserVCL)
}

// generateRouteVCL is GenerateRouteVCL with a custom ghost config path (tests).
func generateRouteVCL(ghostConfigPath, gatewayUserVCL, routeVCL string) (string, error) {
	preamble, err := generatePreamble(ghostConfigPath, 0)
	if err != nil {
		return "", err
	}
	return Merge(preamble, routeVCL, gatewayUserVCL), nil
}

// GeneratePlaceholder returns a standalone VCL with no VMOD imports that
// answers every request with a 503. Unused and failed slot labels point at it.
func GeneratePlaceholder() string {
	return `vcl 4.1;

backend dummy none;

sub vcl_recv {
    return (synth(503, "Route VCL not loaded"));
}

sub vcl_synth {
    set resp.http.X-Ghost-Route-VCL = "unprogrammed";
    set resp.http.Content-Type = "text/plain; charset=utf-8";
    set resp.body = "Route VCL not loaded";
    return (deliver);
}
`
}
