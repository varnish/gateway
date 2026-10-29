package vcl

import (
	"testing"

	"github.com/varnish/gateway/internal/ghost"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestConvertPathMatch(t *testing.T) {
	exact := gatewayv1.PathMatchExact
	prefix := gatewayv1.PathMatchPathPrefix
	re := gatewayv1.PathMatchRegularExpression
	s := func(v string) *string { return &v }

	if got := ConvertPathMatch(nil); got != nil {
		t.Errorf("nil -> %v", got)
	}
	if got := ConvertPathMatch(&gatewayv1.HTTPPathMatch{Value: s("/a")}); got == nil || got.Type != ghost.PathMatchPathPrefix || got.Value != "/a" {
		t.Errorf("nil type -> %+v", got)
	}
	if got := ConvertPathMatch(&gatewayv1.HTTPPathMatch{Type: &prefix, Value: s("/")}); got != nil {
		t.Errorf("PathPrefix / -> %+v, want nil", got)
	}
	if got := ConvertPathMatch(&gatewayv1.HTTPPathMatch{Type: &prefix}); got != nil {
		t.Errorf("PathPrefix nil value -> %+v, want nil", got)
	}
	if got := ConvertPathMatch(&gatewayv1.HTTPPathMatch{Type: &exact, Value: s("/")}); got == nil || got.Type != ghost.PathMatchExact {
		t.Errorf("Exact / -> %+v", got)
	}
	if got := ConvertPathMatch(&gatewayv1.HTTPPathMatch{Type: &re, Value: s("^/x")}); got == nil || got.Type != ghost.PathMatchRegularExpression || got.Value != "^/x" {
		t.Errorf("regex -> %+v", got)
	}
}

func TestConvertHeaderAndQueryMatches(t *testing.T) {
	hre := gatewayv1.HeaderMatchRegularExpression
	qre := gatewayv1.QueryParamMatchRegularExpression
	h := ConvertHeaderMatches([]gatewayv1.HTTPHeaderMatch{{Name: "A", Value: "1"}, {Name: "B", Value: "x", Type: &hre}})
	if len(h) != 2 || h[0].Type != ghost.MatchTypeExact || h[1].Type != ghost.MatchTypeRegularExpression || h[0].Name != "A" {
		t.Errorf("headers = %+v", h)
	}
	if ConvertHeaderMatches(nil) != nil || ConvertQueryParamMatches(nil) != nil {
		t.Error("empty input should give nil")
	}
	q := ConvertQueryParamMatches([]gatewayv1.HTTPQueryParamMatch{{Name: "a", Value: "1"}, {Name: "b", Value: "x", Type: &qre}})
	if len(q) != 2 || q[0].Type != ghost.MatchTypeExact || q[1].Type != ghost.MatchTypeRegularExpression {
		t.Errorf("query = %+v", q)
	}
}
