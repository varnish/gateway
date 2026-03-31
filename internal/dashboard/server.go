package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

//go:embed dashboard.html
var dashboardFS embed.FS

// Server serves the real-time dashboard UI and SSE stream.
type Server struct {
	state      *StateTracker
	bus        *EventBus
	addr       string
	logger     *slog.Logger
	varnishDir string
	activeSessions atomic.Int32
}

// NewServer creates a dashboard server.
// varnishDir is the Varnish instance directory (passed as -n to varnishlog); empty means default.
func NewServer(addr string, state *StateTracker, bus *EventBus, logger *slog.Logger, varnishDir string) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		state:      state,
		bus:        bus,
		addr:       addr,
		logger:     logger,
		varnishDir: varnishDir,
	}
}

// Run starts the dashboard HTTP server. It blocks until the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/events", s.handleSSE)
	mux.HandleFunc("/api/varnishlog", s.handleVarnishlog)

	srv := &http.Server{
		Addr:    s.addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	s.logger.Info("dashboard server starting", "addr", s.addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("dashboard server: %w", err)
	}
	return nil
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data, err := dashboardFS.ReadFile("dashboard.html")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	snap := s.state.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(snap)
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	flusher.Flush()

	ch := s.bus.Subscribe()
	defer s.bus.Unsubscribe(ch)

	ctx := r.Context()

	// Heartbeat ticker
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
			flusher.Flush()
		case <-ticker.C:
			// Send heartbeat with current state summary
			snap := s.state.Snapshot()
			data, _ := json.Marshal(snap)
			fmt.Fprintf(w, "event: heartbeat\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}
