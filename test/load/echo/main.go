// Echo reflector: returns the request details as JSON, logs to stdout, and
// POSTs a record to the ledger collector. Used as the backend for k6 load
// tests so misroutes and duplicates can be detected end-to-end.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/varnish/gateway/test/load/ledger"
)

type config struct {
	listen       string
	pod          string
	service      string
	namespace    string
	collectorURL string
}

func loadConfig() config {
	c := config{
		listen:       getenv("LISTEN", ":8080"),
		pod:          getenv("POD_NAME", "unknown"),
		service:      getenv("SERVICE_NAME", "unknown"),
		namespace:    getenv("NAMESPACE", "default"),
		collectorURL: os.Getenv("COLLECTOR_URL"),
	}
	return c
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type server struct {
	cfg    config
	log    *slog.Logger
	client *http.Client

	mu    sync.Mutex
	batch []ledger.Record
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	traceID := r.Header.Get("X-Trace-ID")
	rec := ledger.Record{
		Source:    ledger.SourceEcho,
		TraceID:   traceID,
		TS:        time.Now().UnixMilli(),
		Pod:       s.cfg.pod,
		Service:   s.cfg.service,
		Namespace: s.cfg.namespace,
		SeenHost:  r.Host,
		SeenPath:  r.URL.Path,
	}

	s.log.Info("req",
		"trace_id", traceID,
		"host", r.Host,
		"path", r.URL.Path,
		"service", s.cfg.service,
		"pod", s.cfg.pod)

	s.enqueue(rec)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Echo-Pod", s.cfg.pod)
	w.Header().Set("X-Echo-Service", s.cfg.service)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(rec)
}

func (s *server) enqueue(rec ledger.Record) {
	if s.cfg.collectorURL == "" {
		return
	}
	s.mu.Lock()
	s.batch = append(s.batch, rec)
	s.mu.Unlock()
}

func (s *server) flushLoop(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flush()
			return
		case <-t.C:
			s.flush()
		}
	}
}

func (s *server) flush() {
	s.mu.Lock()
	if len(s.batch) == 0 {
		s.mu.Unlock()
		return
	}
	batch := s.batch
	s.batch = nil
	s.mu.Unlock()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range batch {
		if err := enc.Encode(r); err != nil {
			s.log.Error("encode", "err", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.collectorURL+"/ingest", &buf)
	if err != nil {
		s.log.Error("build request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := s.client.Do(req)
	if err != nil {
		s.log.Error("post ledger", "err", err, "batch_size", len(batch))
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		s.log.Error("post ledger status", "status", resp.StatusCode, "batch_size", len(batch))
	}
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := loadConfig()
	s := &server{
		cfg:    cfg,
		log:    log,
		client: &http.Client{Timeout: 5 * time.Second},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go s.flushLoop(ctx)

	go func() {
		log.Info("echo listening", "addr", cfg.listen, "pod", cfg.pod, "service", cfg.service)
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
	fmt.Fprintln(os.Stderr, "echo stopped")
}
