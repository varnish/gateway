package varnishadm

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// parseVCLList parses the output from vcl.list command
func parseVCLList(payload string) (*VCLListResult, error) {
	result := &VCLListResult{}

	lines := strings.Split(strings.TrimSpace(payload), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		entry, err := parseVCLLine(line)
		if err != nil {
			return nil, fmt.Errorf("error parsing VCL line %q: %w", line, err)
		}

		result.Entries = append(result.Entries, entry)
	}

	return result, nil
}

// parseVCLLine parses a single line from vcl.list output
// Examples:
// "active      auto/warm          - vcl-api-orig (1 label)"
// "available  label/warm          - label-api -> vcl-api-orig (1 return(vcl))"
func parseVCLLine(line string) (VCLEntry, error) {
	entry := VCLEntry{}

	// Split by whitespace, but preserve multi-word parts
	parts := strings.Fields(line)
	if len(parts) < 4 {
		return entry, fmt.Errorf("insufficient parts in line")
	}

	entry.Status = parts[0]
	entry.Temperature = parts[1]
	// parts[2] is usually "-"

	// Find the part with parentheses for labels/returns
	var nameEnd int
	var parenPart string

	for i := 3; i < len(parts); i++ {
		if strings.Contains(parts[i], "(") {
			nameEnd = i
			// Join remaining parts to get full parentheses content
			parenPart = strings.Join(parts[i:], " ")
			break
		}
	}

	if nameEnd == 0 {
		// No parentheses found, just take the name part
		entry.Name = strings.Join(parts[3:], " ")
		return entry, nil
	}

	// Extract name (everything before parentheses)
	nameParts := parts[3:nameEnd]

	// Check if this is a label (contains "->")
	nameStr := strings.Join(nameParts, " ")
	if strings.Contains(nameStr, "->") {
		// This is a label entry like "label-api -> vcl-api-orig"
		labelParts := strings.Split(nameStr, "->")
		if len(labelParts) == 2 {
			entry.Name = strings.TrimSpace(labelParts[0])
			entry.LabelTarget = strings.TrimSpace(labelParts[1])
		}
	} else {
		entry.Name = nameStr
	}

	// Parse parentheses content
	if parenPart != "" {
		entry.Labels, entry.Returns = parseParenthesesContent(parenPart)
	}

	return entry, nil
}

var (
	reParenContent = regexp.MustCompile(`\(([^)]+)\)`)
	reLabelCount   = regexp.MustCompile(`(\d+)\s+label`)
	reReturnCount  = regexp.MustCompile(`(\d+)\s+return`)
)

// parseParenthesesContent extracts numbers from parentheses
// Examples: "(1 label)" -> labels=1, returns=0
//
//	"(1 return(vcl))" -> labels=0, returns=1
func parseParenthesesContent(content string) (labels, returns int) {
	// Extract content between parentheses
	matches := reParenContent.FindStringSubmatch(content)
	if len(matches) < 2 {
		return 0, 0
	}

	inner := matches[1]

	// Look for "N label" or "N return"
	if strings.Contains(inner, "label") {
		if numMatches := reLabelCount.FindStringSubmatch(inner); len(numMatches) >= 2 {
			if num, err := strconv.Atoi(numMatches[1]); err == nil {
				labels = num
			}
		}
	}

	if strings.Contains(inner, "return") {
		if numMatches := reReturnCount.FindStringSubmatch(inner); len(numMatches) >= 2 {
			if num, err := strconv.Atoi(numMatches[1]); err == nil {
				returns = num
			}
		}
	}

	return labels, returns
}

// parseTLSCertList parses the output from tls.cert.list command
// Expected format: Frontend State   Hostname         Certificate ID  Expiration date           OCSP stapling
func parseTLSCertList(payload string) (*TLSCertListResult, error) {
	result := &TLSCertListResult{}

	lines := strings.Split(strings.TrimSpace(payload), "\n")

	// Skip header line if present (contains "Frontend", "State", etc.)
	startIndex := 0
	if len(lines) > 0 && strings.Contains(lines[0], "Frontend") {
		startIndex = 1
	}

	for i := startIndex; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}

		entry, err := parseTLSCertLine(line)
		if err != nil {
			return nil, fmt.Errorf("error parsing TLS cert line %q: %w", line, err)
		}

		result.Entries = append(result.Entries, entry)
	}

	return result, nil
}

// parseTLSCertLine parses a single line from tls.cert.list output
// Expected format: "frontend state hostname cert-id Feb 29 13:38:00 2042 GMT true"
func parseTLSCertLine(line string) (TLSCertEntry, error) {
	entry := TLSCertEntry{}

	// Split by multiple spaces to handle column alignment
	parts := strings.Fields(line)
	// Need at least: frontend, state, hostname, cert-id, date(5 parts), ocsp = 10 parts
	if len(parts) < 10 {
		return entry, fmt.Errorf("insufficient parts in TLS cert line, expected at least 10, got %d", len(parts))
	}

	entry.Frontend = parts[0]
	entry.State = parts[1]
	entry.Hostname = parts[2]
	entry.CertificateID = parts[3]

	// Parse expiration date - format: "Feb 29 13:38:00 2042 GMT"
	// This is parts[4] through parts[8]
	dateStr := strings.Join(parts[4:9], " ")
	if expiration, err := time.Parse("Jan 02 15:04:05 2006 MST", dateStr); err == nil {
		entry.Expiration = expiration
	} else {
		// If parsing fails, log but don't error - expiration is optional for functionality
		// Could try alternative formats here if needed
	}

	// OCSP stapling is parts[9]
	ocspStr := parts[9]
	entry.OCSPStapling = strings.ToLower(ocspStr) == "enabled" || strings.ToLower(ocspStr) == "true"

	return entry, nil
}
