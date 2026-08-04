package app

import (
	"net/http/httptest"
	"testing"

	"github.com/akz142857/Heimdall/internal/config"
)

func TestDeveloperGatewayBaseURLUsesGatewayListenerNotAdminOrigin(t *testing.T) {
	runtime := &Runtime{config: config.Config{Server: config.Server{GatewayListen: "127.0.0.1:8080", AdminListen: "127.0.0.1:8081"}}}
	request := httptest.NewRequest("GET", "http://gateway.example:8081/admin/api/v1/developer/config", nil)
	if got := runtime.developerGatewayBaseURL(request); got != "http://gateway.example:8080" {
		t.Fatalf("gateway base URL=%q", got)
	}
	runtime.config.TLS.Enabled = true
	if got := runtime.developerGatewayBaseURL(request); got != "https://gateway.example:8080" {
		t.Fatalf("TLS gateway base URL=%q", got)
	}
}
