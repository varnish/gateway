package vcl

import (
	"slices"
	"strings"
)

// Merge combines generated VCL with one or more user-provided VCL documents.
//
// VCL requires all imports to appear before any subroutines, and the vcl version
// declaration must come first. This function handles the merge properly:
//
//  1. Extract vcl version and imports from the generated and every user VCL
//  2. Output: vcl version (generated only), generated imports, user imports
//     (deduplicated, in document order), generated body, each user body in
//     order, postamble
//
// VCL allows multiple definitions of the same subroutine - they are concatenated
// at compile time. If user defines vcl_backend_fetch, their code runs after the
// gateway routing call. With several user documents, an earlier document's
// subroutine code runs before a later one's.
//
// Empty user documents are ignored; with none left the result is the generated
// VCL followed by the postamble.
func Merge(generatedVCL string, userVCLs ...string) string {
	var users []vclParts
	for _, u := range userVCLs {
		if u != "" {
			users = append(users, parseVCL(u))
		}
	}
	if len(users) == 0 {
		return generatedVCL + postambleVCL
	}

	genParts := parseVCL(generatedVCL)

	var sb strings.Builder

	// VCL version (from generated, user's is dropped)
	if genParts.version != "" {
		sb.WriteString(genParts.version)
		sb.WriteString("\n\n")
	}

	// Ghost import first (from generated)
	seen := make([]string, 0, len(genParts.imports))
	for _, imp := range genParts.imports {
		sb.WriteString(imp)
		sb.WriteString("\n")
		seen = append(seen, imp)
	}

	// User imports, skipping any already emitted
	for _, u := range users {
		for _, imp := range u.imports {
			if containsImport(seen, imp) {
				continue
			}
			sb.WriteString(imp)
			sb.WriteString("\n")
			seen = append(seen, imp)
		}
	}

	// Blank line between imports and body
	sb.WriteString("\n")

	// Generated body, then each user body (subroutines)
	writeBody(&sb, genParts.body)
	for _, u := range users {
		writeBody(&sb, u.body)
	}

	// Postamble: runs after user VCL to handle deferred actions (e.g., return(pass))
	sb.WriteString(postambleVCL)

	return sb.String()
}

// writeBody appends a VCL body, newline-terminated, if it is non-empty.
func writeBody(sb *strings.Builder, body string) {
	if body == "" {
		return
	}
	sb.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		sb.WriteString("\n")
	}
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
