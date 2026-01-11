package vcl

import (
	"strings"
)

// Merge combines generated VCL with user-provided VCL.
//
// VCL allows multiple definitions of the same subroutine - they are concatenated
// at compile time. This means we don't need to parse or modify user VCL at all.
//
// The merge order is:
//  1. Generated VCL (imports, vcl_init, gateway_backend_fetch, vcl_backend_fetch)
//  2. User VCL appended after
//
// If the user also defines vcl_backend_fetch, their code runs after the gateway
// routing call. Users who need pre-routing logic should use vcl_recv instead.
func Merge(generatedVCL, userVCL string) string {
	if userVCL == "" {
		return generatedVCL
	}

	var sb strings.Builder
	sb.WriteString(generatedVCL)

	// Ensure there's a newline before user VCL
	if !strings.HasSuffix(generatedVCL, "\n") {
		sb.WriteString("\n")
	}

	sb.WriteString(userVCL)

	// Ensure final newline
	if !strings.HasSuffix(userVCL, "\n") {
		sb.WriteString("\n")
	}

	return sb.String()
}
