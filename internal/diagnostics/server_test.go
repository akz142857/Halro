package diagnostics

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListenerBoundary(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	closeServer, err := Start("", logger)
	if err != nil {
		t.Fatal(err)
	}
	closeServer()
	for _, address := range []string{":6060", "0.0.0.0:6060", "[::]:6060", "localhost:6060", "192.168.1.1:6060", "127.0.0.1:0"} {
		if _, err := Start(address, logger); err == nil {
			t.Fatalf("accepted %q", address)
		}
	}
	// Reserve a port to verify both synchronous bind failure and release on close.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if _, err := Start(address, logger); err == nil {
		t.Fatal("accepted busy port")
	}
	listener.Close()
	closeServer, err = Start(address, logger)
	if err != nil {
		t.Fatal(err)
	}
	closeServer()
	closeServer() // cleanup is safe if called again
	listener, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("listener still held after close: %v", err)
	}
	listener.Close()
}

func TestDiagnosticsProfilesAndRuntime(t *testing.T) {
	for _, path := range []string{"/debug/pprof/heap", "/debug/pprof/allocs", "/debug/pprof/goroutine"} {
		response := httptest.NewRecorder()
		handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Body.Len() == 0 {
			t.Fatalf("%s: %d", path, response.Code)
		}
	}
	response := httptest.NewRecorder()
	handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/debug/runtime", nil))
	var data struct {
		Memory     struct{ Sys uint64 }
		Goroutines int
	}
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.Memory.Sys == 0 || data.Goroutines < 1 {
		t.Fatalf("missing runtime data: %+v", data)
	}
	response = httptest.NewRecorder()
	handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/debug/pprof/heap", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST accepted: %d", response.Code)
	}
}
