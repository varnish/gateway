package varnishstat

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// fetchFunc is the signature for fetching varnishstat data.
type fetchFunc func(ctx context.Context, varnishDir string) ([]Stat, error)

// Collector implements prometheus.Collector. On each Prometheus scrape it runs
// varnishstat and emits all counters as Prometheus metrics.
//
// It uses the unchecked collector pattern (empty Describe) because the set of
// metrics is dynamic — VMODs and Varnish upgrades can add new counters.
type Collector struct {
	varnishDir string
	logger     *slog.Logger
	fetchFunc  fetchFunc

	mu    sync.RWMutex
	descs map[string]*prometheus.Desc
}

// NewCollector creates a Collector that will run varnishstat against the given
// Varnish instance directory. Pass an empty string for the default instance.
func NewCollector(varnishDir string, logger *slog.Logger) *Collector {
	return &Collector{
		varnishDir: varnishDir,
		logger:     logger,
		fetchFunc:  Fetch,
		descs:      make(map[string]*prometheus.Desc),
	}
}

func newCollectorWithFetch(varnishDir string, logger *slog.Logger, fn fetchFunc) *Collector {
	c := NewCollector(varnishDir, logger)
	c.fetchFunc = fn
	return c
}

// Describe sends no descriptors — this is an unchecked collector because the
// metric set is dynamic (VMODs and Varnish upgrades can add new counters).
func (c *Collector) Describe(chan<- *prometheus.Desc) {}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	stats, err := c.fetchFunc(context.Background(), c.varnishDir)
	if err != nil {
		c.logger.Warn("varnishstat fetch failed", "error", err)
		return
	}

	for _, s := range stats {
		desc := c.getOrCreateDesc(s)
		valueType := flagToValueType(s.Flag)
		m, err := prometheus.NewConstMetric(desc, valueType, s.Value)
		if err != nil {
			c.logger.Warn("failed to create metric", "name", s.Name, "error", err)
			continue
		}
		ch <- m
	}
}

func (c *Collector) getOrCreateDesc(s Stat) *prometheus.Desc {
	c.mu.RLock()
	if desc, ok := c.descs[s.Name]; ok {
		c.mu.RUnlock()
		return desc
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	// Double-check after promotion.
	if desc, ok := c.descs[s.Name]; ok {
		return desc
	}
	fqName := toPrometheusName(s.Name)
	desc := prometheus.NewDesc(fqName, s.Description, nil, nil)
	c.descs[s.Name] = desc
	return desc
}

// "MAIN.sess_conn" -> "varnish_main_sess_conn"
func toPrometheusName(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, ".", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return "varnish_" + s
}

func flagToValueType(flag string) prometheus.ValueType {
	if flag == "c" {
		return prometheus.CounterValue
	}
	return prometheus.GaugeValue
}
