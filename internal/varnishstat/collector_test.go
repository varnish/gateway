package varnishstat

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestCollectorCollect(t *testing.T) {
	canned := []Stat{
		{Name: "MAIN.sess_conn", Value: 100, Flag: "c", Description: "Sessions accepted"},
		{Name: "MAIN.n_object", Value: 42, Flag: "g", Description: "object structs made"},
		{Name: "VBE.boot.default.happy", Value: 1, Flag: "b", Description: "Happy health probes"},
	}

	fn := func(_ context.Context, _ string) ([]Stat, error) {
		return canned, nil
	}

	c := newCollectorWithFetch("", slog.Default(), fn)

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		byName[f.GetName()] = f
	}

	tests := []struct {
		promName string
		value    float64
		mtype    dto.MetricType
	}{
		{"varnish_main_sess_conn", 100, dto.MetricType_COUNTER},
		{"varnish_main_n_object", 42, dto.MetricType_GAUGE},
		{"varnish_vbe_boot_default_happy", 1, dto.MetricType_GAUGE},
	}

	for _, tt := range tests {
		t.Run(tt.promName, func(t *testing.T) {
			f, ok := byName[tt.promName]
			if !ok {
				t.Fatalf("metric %q not found in gathered families", tt.promName)
			}
			if f.GetType() != tt.mtype {
				t.Errorf("type = %v, want %v", f.GetType(), tt.mtype)
			}
			metrics := f.GetMetric()
			if len(metrics) != 1 {
				t.Fatalf("got %d metrics, want 1", len(metrics))
			}
			var got float64
			if tt.mtype == dto.MetricType_COUNTER {
				got = metrics[0].GetCounter().GetValue()
			} else {
				got = metrics[0].GetGauge().GetValue()
			}
			if got != tt.value {
				t.Errorf("value = %v, want %v", got, tt.value)
			}
		})
	}
}

func TestCollectorFetchError(t *testing.T) {
	fn := func(_ context.Context, _ string) ([]Stat, error) {
		return nil, errors.New("varnish not running")
	}

	c := newCollectorWithFetch("", slog.Default(), fn)

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	if len(families) != 0 {
		t.Errorf("expected 0 metric families on error, got %d", len(families))
	}
}

func TestToPrometheusName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"MAIN.sess_conn", "varnish_main_sess_conn"},
		{"VBE.boot.default.happy", "varnish_vbe_boot_default_happy"},
		{"SMA.s0.g_bytes", "varnish_sma_s0_g_bytes"},
		{"MAIN.n_object", "varnish_main_n_object"},
		{"MGT.child-start", "varnish_mgt_child_start"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := toPrometheusName(tt.input)
			if got != tt.want {
				t.Errorf("toPrometheusName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFlagToValueType(t *testing.T) {
	if flagToValueType("c") != prometheus.CounterValue {
		t.Error("flag 'c' should map to CounterValue")
	}
	if flagToValueType("g") != prometheus.GaugeValue {
		t.Error("flag 'g' should map to GaugeValue")
	}
	if flagToValueType("b") != prometheus.GaugeValue {
		t.Error("flag 'b' should map to GaugeValue")
	}
}

func TestCollectorDescCaching(t *testing.T) {
	callCount := 0
	fn := func(_ context.Context, _ string) ([]Stat, error) {
		callCount++
		return []Stat{
			{Name: "MAIN.sess_conn", Value: float64(callCount), Flag: "c", Description: "Sessions accepted"},
		}, nil
	}

	c := newCollectorWithFetch("", slog.Default(), fn)

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)

	// Gather twice to verify desc caching doesn't break anything.
	for i := 0; i < 2; i++ {
		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("Gather #%d: %v", i+1, err)
		}
		if len(families) != 1 {
			t.Fatalf("Gather #%d: got %d families, want 1", i+1, len(families))
		}
	}

	if callCount != 2 {
		t.Errorf("fetchFunc called %d times, want 2", callCount)
	}

	// Verify the desc was cached (only one entry in the map).
	c.mu.Lock()
	descCount := len(c.descs)
	c.mu.Unlock()
	if descCount != 1 {
		t.Errorf("cached %d descs, want 1", descCount)
	}
}
