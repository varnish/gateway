// Ledger collector: accepts NDJSON records from k6, echo, and chaos runners,
// appends to a rotating file on a PVC. Exposes /ingest, /healthz, /metrics,
// and /download for pulling the current ledger file.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/varnish/gateway/test/load/ledger"
)

type config struct {
	listen  string
	dataDir string
	// Bytes before rotating to a new segment. 0 = no rotation.
	rotateBytes int64
}

func loadConfig() (config, error) {
	c := config{
		listen:  getenv("LISTEN", ":8080"),
		dataDir: getenv("DATA_DIR", "/data"),
	}
	if v := os.Getenv("ROTATE_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return c, fmt.Errorf("parse ROTATE_BYTES: %w", err)
		}
		c.rotateBytes = n
	}
	if err := os.MkdirAll(c.dataDir, 0o755); err != nil {
		return c, fmt.Errorf("mkdir %s: %w", c.dataDir, err)
	}
	return c, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type collector struct {
	cfg    config
	log    *slog.Logger
	reg    *prometheus.Registry
	counts *prometheus.CounterVec

	mu      sync.Mutex
	file    *os.File
	written int64

	totalRecords atomic.Int64
}

func newCollector(cfg config, log *slog.Logger) (*collector, error) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	counts := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ledger_records_total",
		Help: "Ledger records ingested, by source.",
	}, []string{"source"})
	reg.MustRegister(counts)

	c := &collector{cfg: cfg, log: log, reg: reg, counts: counts}
	if err := c.rotate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *collector) rotate() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file != nil {
		if err := c.file.Close(); err != nil {
			c.log.Error("close current segment", "err", err)
		}
	}
	name := fmt.Sprintf("ledger-%s.ndjson", time.Now().UTC().Format("20060102T150405Z"))
	path := filepath.Join(c.cfg.dataDir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	c.file = f
	c.written = 0
	// Maintain a stable "current" symlink for the downloader.
	cur := filepath.Join(c.cfg.dataDir, "current.ndjson")
	_ = os.Remove(cur)
	if err := os.Symlink(name, cur); err != nil {
		c.log.Warn("symlink current", "err", err)
	}
	c.log.Info("rotated", "path", path)
	return nil
}

func (c *collector) ingest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	br := bufio.NewReader(r.Body)
	var buf []byte
	n := 0
	bySource := map[string]int{}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			// Validate minimally so garbage doesn't end up on disk.
			var rec ledger.Record
			if jerr := json.Unmarshal(line, &rec); jerr != nil {
				http.Error(w, fmt.Sprintf("bad json: %v", jerr), http.StatusBadRequest)
				return
			}
			if rec.TS == 0 {
				rec.TS = time.Now().UnixMilli()
				line, _ = json.Marshal(rec)
				line = append(line, '\n')
			}
			buf = append(buf, line...)
			n++
			bySource[string(rec.Source)]++
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("read: %v", err), http.StatusBadRequest)
			return
		}
	}
	if n == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	c.mu.Lock()
	written, err := c.file.Write(buf)
	if err == nil {
		c.written += int64(written)
	}
	rotateNeeded := c.cfg.rotateBytes > 0 && c.written >= c.cfg.rotateBytes
	c.mu.Unlock()

	if err != nil {
		http.Error(w, fmt.Sprintf("write: %v", err), http.StatusInternalServerError)
		return
	}

	for src, cnt := range bySource {
		c.counts.WithLabelValues(src).Add(float64(cnt))
	}
	c.totalRecords.Add(int64(n))

	if rotateNeeded {
		if err := c.rotate(); err != nil {
			c.log.Error("rotate", "err", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (c *collector) download(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	if c.file != nil {
		_ = c.file.Sync()
	}
	c.mu.Unlock()
	// Merge all segments in directory order.
	entries, err := os.ReadDir(c.cfg.dataDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) < len("ledger-") || name[:len("ledger-")] != "ledger-" {
			continue
		}
		f, err := os.Open(filepath.Join(c.cfg.dataDir, name))
		if err != nil {
			c.log.Error("open segment", "name", name, "err", err)
			continue
		}
		_, _ = io.Copy(w, f)
		_ = f.Close()
	}
}

func (c *collector) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file == nil {
		return nil
	}
	return c.file.Close()
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := loadConfig()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	c, err := newCollector(cfg, log)
	if err != nil {
		log.Error("collector", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ingest", c.ingest)
	mux.HandleFunc("/download", c.download)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/metrics", promhttp.HandlerFor(c.reg, promhttp.HandlerOpts{}))

	srv := &http.Server{Addr: cfg.listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		log.Info("collector listening", "addr", cfg.listen, "data_dir", cfg.dataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
	if err := c.close(); err != nil {
		log.Error("close file", "err", err)
	}
}
