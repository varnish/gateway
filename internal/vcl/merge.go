package vcl

import (
	"slices"
	"strings"
)

// Merge combines generated VCL with user-provided VCL.
//
// VCL requires all imports to appear before any subroutines, and the vcl version
// declaration must come first. This function handles the merge properly:
//
//  1. Extract vcl version and imports from both generated and user VCL
//  2. Output: vcl version, ghost import, user imports, generated body, user body
//
// VCL allows multiple definitions of the same subroutine - they are concatenated
// at compile time. If user defines vcl_backend_fetch, their code runs after the
// gateway routing call.
func Merge(generatedVCL, userVCL string) string {
	if userVCL == "" {
		return generatedVCL
	}

	// Parse both VCLs
	genParts := parseVCL(generatedVCL)
	userParts := parseVCL(userVCL)

	var sb strings.Builder

	// VCL version (from generated, user's is dropped)
	if genParts.version != "" {
		sb.WriteString(genParts.version)
		sb.WriteString("\n\n")
	}

	// Ghost import first (from generated)
	for _, imp := range genParts.imports {
		sb.WriteString(imp)
		sb.WriteString("\n")
	}

	// User imports (skip if duplicate of ghost)
	for _, imp := range userParts.imports {
		if !containsImport(genParts.imports, imp) {
			sb.WriteString(imp)
			sb.WriteString("\n")
		}
	}

	// Blank line between imports and body
	sb.WriteString("\n")

	// Generated body (subroutines)
	if genParts.body != "" {
		sb.WriteString(genParts.body)
		if !strings.HasSuffix(genParts.body, "\n") {
			sb.WriteString("\n")
		}
	}

	// User body (subroutines)
	if userParts.body != "" {
		sb.WriteString(userParts.body)
		if !strings.HasSuffix(userParts.body, "\n") {
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// vclParts holds the parsed sections of a VCL file.
type vclParts struct {
	version string   // "vcl 4.1;" line
	imports []string // import statements
	body    string   // everything else (subroutines, backends, etc.)
}

// parseVCL splits VCL into version, imports, and body sections.
// Uses simple line-by-line parsing without full VCL parsing.
func parseVCL(vcl string) vclParts {
	var parts vclParts
	var bodyLines []string

	lines := strings.Split(vcl, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip empty lines in header section
		if trimmed == "" && parts.body == "" && len(bodyLines) == 0 {
			continue
		}

		// VCL version declaration
		if strings.HasPrefix(trimmed, "vcl ") && strings.HasSuffix(trimmed, ";") {
			parts.version = trimmed
			continue
		}

		// Import statement
		if strings.HasPrefix(trimmed, "import ") && strings.HasSuffix(trimmed, ";") {
			parts.imports = append(parts.imports, trimmed)
			continue
		}

		// Everything else is body
		bodyLines = append(bodyLines, line)
	}

	// Join body lines, trimming leading empty lines
	body := strings.Join(bodyLines, "\n")
	parts.body = strings.TrimLeft(body, "\n")

	return parts
}

// containsImport checks if an import statement already exists in the list.
func containsImport(imports []string, imp string) bool {
	return slices.Contains(imports, imp)
}
