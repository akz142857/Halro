// Package diagnostics provides an opt-in, process-scoped profiling listener.
package diagnostics

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"runtime"
	"time"
)

// Start binds synchronously so a bad address fails before data recovery starts.
// Empty disables profiling. Only literal loopback addresses are accepted: this
// listener has no authentication and profiles may contain sensitive material.
// The returned function closes active connections and waits for Serve to exit.
func Start(address string, logger *slog.Logger) (func(), error) {
	if address == "" {
		return func() {}, nil
	}
	endpoint, err := netip.ParseAddrPort(address)
	if err != nil || !endpoint.Addr().IsLoopback() || endpoint.Port() == 0 {
		return nil, fmt.Errorf("HALRO_PPROF_LISTEN must be a literal loopback address with a nonzero port")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("bind diagnostics: %w", err)
	}
	server := &http.Server{Handler: handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Error("diagnostics listener failed", "error", err)
		}
	}()
	logger.Warn("local profiling enabled", "listen", listener.Addr().String())
	return func() { _ = server.Close(); <-done }, nil
}

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("GET /debug/runtime", func(w http.ResponseWriter, r *http.Request) {
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Memory     runtime.MemStats `json:"memory"`
			Goroutines int              `json:"goroutines"`
		}{memory, runtime.NumGoroutine()})
	})
	return mux
}
