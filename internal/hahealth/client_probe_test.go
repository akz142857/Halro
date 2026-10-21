package hahealth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestProbeClientServiceRetriesOnFreshHTTP2Connection(t *testing.T) {
	var mu sync.Mutex
	firstPeer := ""
	peers := make(map[string]bool)
	requests := 0
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/" || r.Method != http.MethodGet || r.ProtoMajor != 2 {
			t.Errorf("probe request method=%s path=%s proto=%s", r.Method, r.URL.Path, r.Proto)
		}
		requests++
		if firstPeer == "" {
			firstPeer = r.RemoteAddr
		}
		peers[r.RemoteAddr] = true
		role := "replica"
		if r.RemoteAddr != firstPeer {
			role = "primary"
		}
		_, _ = fmt.Fprintf(w, `{"role":%q,"cluster_id":"test-ha"}`, role)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = true
	client := &http.Client{Transport: transport}
	defer client.CloseIdleConnections()

	result := ProbeClientService(context.Background(), client, server.URL+"/", "test-ha", "", 3)
	if result.Signal.Level != Healthy || result.Code != "healthy" {
		t.Fatalf("HTTP/2 Service probe = %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 2 || len(peers) != 2 {
		t.Fatalf("Replica→Primary used %d requests over %d connections", requests, len(peers))
	}
}
