package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/config"
)

func TestMasterKeyRunbooksAreEmbeddedAndNotCached(t *testing.T) {
	runtime := &Runtime{config: config.Config{Storage: config.Storage{MasterKey: config.MasterKey{Mode: config.MasterKeyModeKeySlots}}}}
	tests := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		want    string
	}{
		{name: "lifecycle", handler: runtime.adminMasterKeyLifecycleRunbook, want: "KEK rewrap"},
		{name: "recovery", handler: runtime.adminMasterKeyRecoveryRunbook, want: "Recovery"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			test.handler(response, httptest.NewRequest(http.MethodGet, "/", nil))
			if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/markdown") ||
				response.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
			}
		})
	}
}

func TestMasterKeyRunbooksAreUnavailableInFileMode(t *testing.T) {
	runtime := &Runtime{config: config.Config{Storage: config.Storage{MasterKey: config.MasterKey{Mode: config.MasterKeyModeFile}}}}
	response := httptest.NewRecorder()
	runtime.adminMasterKeyLifecycleRunbook(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}
