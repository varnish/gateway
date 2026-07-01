// Package metrics provides operator-level Prometheus collectors for the
// Gateway API objects the operator manages. These are domain/inventory
// metrics — how many Gateways and HTTPRoutes exist and their Accepted /
// Programmed status — which complement (and do not duplicate) the
// controller-runtime and client-go metrics already exposed on the same
// metrics endpoint.
package metrics

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// gatewayCollector is a prometheus.Collector that snapshots Gateway API
// object state at scrape time. Snapshotting (rather than set-on-reconcile)
// means deleted objects drop out of the metrics automatically, with no
// stale-series bookkeeping — the same model gateway-api-state-metrics uses.
//
// Reads go through a cache-backed client (the manager's informer cache), so
// Collect does no live API calls.
type gatewayCollector struct {
	reader         client.Reader
	controllerName string
	version        string
	logger         *slog.Logger

	gatewaysDesc           *prometheus.Desc
	gwAcceptedDesc         *prometheus.Desc
	gwProgrammedDesc       *prometheus.Desc
	gwListenersDesc        *prometheus.Desc
	gwAttachedRoutesDesc   *prometheus.Desc
	httproutesDesc         *prometheus.Desc
	httproutesAcceptedDesc *prometheus.Desc
	infoDesc               *prometheus.Desc
}

// RegisterGatewayMetrics builds the Gateway API inventory collector and
// registers it on reg. reader should be the manager's cache-backed client,
// controllerName the operator's GatewayClass controller name (used to filter
// to objects we manage), and version the operator build version.
func RegisterGatewayMetrics(reg prometheus.Registerer, reader client.Reader, controllerName, version string, logger *slog.Logger) {
	c := &gatewayCollector{
		reader:         reader,
		controllerName: controllerName,
		version:        version,
		logger:         logger,
		gatewaysDesc: prometheus.NewDesc(
			"varnish_gateway_gateways",
			"Number of Gateways managed by this operator, by GatewayClass.",
			[]string{"gatewayclass"}, nil,
		),
		gwAcceptedDesc: prometheus.NewDesc(
			"varnish_gateway_gateway_accepted",
			"Whether a managed Gateway's Accepted condition is true (1) or not (0).",
			[]string{"namespace", "name"}, nil,
		),
		gwProgrammedDesc: prometheus.NewDesc(
			"varnish_gateway_gateway_programmed",
			"Whether a managed Gateway's Programmed condition is true (1) or not (0).",
			[]string{"namespace", "name"}, nil,
		),
		gwListenersDesc: prometheus.NewDesc(
			"varnish_gateway_gateway_listeners",
			"Number of listeners configured on a managed Gateway.",
			[]string{"namespace", "name"}, nil,
		),
		gwAttachedRoutesDesc: prometheus.NewDesc(
			"varnish_gateway_gateway_attached_routes",
			"Total AttachedRoutes across all listeners of a managed Gateway.",
			[]string{"namespace", "name"}, nil,
		),
		httproutesDesc: prometheus.NewDesc(
			"varnish_gateway_httproutes",
			"Number of HTTPRoutes attached to a Gateway managed by this operator.",
			nil, nil,
		),
		httproutesAcceptedDesc: prometheus.NewDesc(
			"varnish_gateway_httproutes_accepted",
			"Number of managed HTTPRoutes Accepted on at least one parent.",
			nil, nil,
		),
		infoDesc: prometheus.NewDesc(
			"varnish_gateway_info",
			"Operator build info; constant 1 with the version label.",
			[]string{"version"}, nil,
		),
	}
	reg.MustRegister(c)
}

// Describe implements prometheus.Collector.
func (c *gatewayCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.gatewaysDesc
	ch <- c.gwAcceptedDesc
	ch <- c.gwProgrammedDesc
	ch <- c.gwListenersDesc
	ch <- c.gwAttachedRoutesDesc
	ch <- c.httproutesDesc
	ch <- c.httproutesAcceptedDesc
	ch <- c.infoDesc
}

// Collect implements prometheus.Collector.
func (c *gatewayCollector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()

	ch <- prometheus.MustNewConstMetric(c.infoDesc, prometheus.GaugeValue, 1, c.version)

	c.collectGateways(ctx, ch)
	c.collectHTTPRoutes(ctx, ch)
}

func (c *gatewayCollector) collectGateways(ctx context.Context, ch chan<- prometheus.Metric) {
	var gwList gatewayv1.GatewayList
	if err := c.reader.List(ctx, &gwList); err != nil {
		c.warn("list Gateways", err)
		return
	}

	// Memoize the GatewayClass -> ours? lookup so a cluster with many
	// Gateways on one class does a single Get per class per scrape.
	classIsOurs := make(map[string]bool)
	classCounts := make(map[string]int)

	for i := range gwList.Items {
		gw := &gwList.Items[i]
		className := string(gw.Spec.GatewayClassName)
		ours, seen := classIsOurs[className]
		if !seen {
			ours = c.isOurClass(ctx, className)
			classIsOurs[className] = ours
		}
		if !ours {
			continue
		}
		classCounts[className]++

		ns, name := gw.Namespace, gw.Name
		ch <- prometheus.MustNewConstMetric(c.gwAcceptedDesc, prometheus.GaugeValue,
			boolToFloat(apimeta.IsStatusConditionTrue(gw.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))), ns, name)
		ch <- prometheus.MustNewConstMetric(c.gwProgrammedDesc, prometheus.GaugeValue,
			boolToFloat(apimeta.IsStatusConditionTrue(gw.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))), ns, name)
		ch <- prometheus.MustNewConstMetric(c.gwListenersDesc, prometheus.GaugeValue,
			float64(len(gw.Spec.Listeners)), ns, name)

		var attached int32
		for _, l := range gw.Status.Listeners {
			attached += l.AttachedRoutes
		}
		ch <- prometheus.MustNewConstMetric(c.gwAttachedRoutesDesc, prometheus.GaugeValue, float64(attached), ns, name)
	}

	for className, count := range classCounts {
		ch <- prometheus.MustNewConstMetric(c.gatewaysDesc, prometheus.GaugeValue, float64(count), className)
	}
}

func (c *gatewayCollector) collectHTTPRoutes(ctx context.Context, ch chan<- prometheus.Metric) {
	var rtList gatewayv1.HTTPRouteList
	if err := c.reader.List(ctx, &rtList); err != nil {
		c.warn("list HTTPRoutes", err)
		return
	}

	// A route is "ours" when it carries a parent status written under our
	// controller name; that's the authoritative signal that this operator
	// reconciled it, independent of the attachment-derivation logic in the
	// controller package.
	var total, accepted int
	for i := range rtList.Items {
		rt := &rtList.Items[i]
		var mine, acc bool
		for _, p := range rt.Status.Parents {
			if string(p.ControllerName) != c.controllerName {
				continue
			}
			mine = true
			if apimeta.IsStatusConditionTrue(p.Conditions, string(gatewayv1.RouteConditionAccepted)) {
				acc = true
			}
		}
		if mine {
			total++
			if acc {
				accepted++
			}
		}
	}

	ch <- prometheus.MustNewConstMetric(c.httproutesDesc, prometheus.GaugeValue, float64(total))
	ch <- prometheus.MustNewConstMetric(c.httproutesAcceptedDesc, prometheus.GaugeValue, float64(accepted))
}

// isOurClass reports whether className is a GatewayClass with our controllerName.
func (c *gatewayCollector) isOurClass(ctx context.Context, className string) bool {
	if className == "" {
		return false
	}
	var gc gatewayv1.GatewayClass
	if err := c.reader.Get(ctx, client.ObjectKey{Name: className}, &gc); err != nil {
		return false
	}
	return string(gc.Spec.ControllerName) == c.controllerName
}

func (c *gatewayCollector) warn(msg string, err error) {
	if c.logger != nil {
		c.logger.Warn("gateway metrics collection failed", "step", msg, "error", err)
	}
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
