package vcl

import (
	"github.com/varnish/gateway/internal/ghost"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ConvertPathMatch converts a Gateway API path match to a ghost.PathMatch.
// A nil input yields nil. PathPrefix "/" is the K8s API server default when no
// path is specified; it matches all paths and has no specificity, so it is
// treated as no path match. Without this, API-defaulted routes get inflated
// priorities that break Gateway API precedence (e.g., method PATCH with
// implicit PathPrefix / would outrank an explicit PathPrefix /path5).
func ConvertPathMatch(p *gatewayv1.HTTPPathMatch) *ghost.PathMatch {
	if p == nil {
		return nil
	}
	pathType := ghost.PathMatchPathPrefix // default
	if p.Type != nil {
		switch *p.Type {
		case gatewayv1.PathMatchExact:
			pathType = ghost.PathMatchExact
		case gatewayv1.PathMatchPathPrefix:
			pathType = ghost.PathMatchPathPrefix
		case gatewayv1.PathMatchRegularExpression:
			pathType = ghost.PathMatchRegularExpression
		}
	}
	pathValue := "/"
	if p.Value != nil {
		pathValue = *p.Value
	}
	if pathType == ghost.PathMatchPathPrefix && pathValue == "/" {
		return nil
	}
	return &ghost.PathMatch{Type: pathType, Value: pathValue}
}

// ConvertHeaderMatches converts Gateway API header matches to ghost header matches.
func ConvertHeaderMatches(in []gatewayv1.HTTPHeaderMatch) []ghost.HeaderMatch {
	var out []ghost.HeaderMatch
	for _, h := range in {
		matchType := ghost.MatchTypeExact
		if h.Type != nil && *h.Type == gatewayv1.HeaderMatchRegularExpression {
			matchType = ghost.MatchTypeRegularExpression
		}
		out = append(out, ghost.HeaderMatch{Name: string(h.Name), Value: h.Value, Type: matchType})
	}
	return out
}

// ConvertQueryParamMatches converts Gateway API query param matches to ghost query param matches.
func ConvertQueryParamMatches(in []gatewayv1.HTTPQueryParamMatch) []ghost.QueryParamMatch {
	var out []ghost.QueryParamMatch
	for _, qp := range in {
		matchType := ghost.MatchTypeExact
		if qp.Type != nil && *qp.Type == gatewayv1.QueryParamMatchRegularExpression {
			matchType = ghost.MatchTypeRegularExpression
		}
		out = append(out, ghost.QueryParamMatch{Name: string(qp.Name), Value: qp.Value, Type: matchType})
	}
	return out
}
