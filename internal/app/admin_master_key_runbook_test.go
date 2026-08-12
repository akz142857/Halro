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

// The stale-configuration procedure is needed in whichever mode the instance
// happens to run, and the moment it is needed is the moment the data plane is
// returning 503 — so it ships in the binary rather than living only in the
// repository. The file-mode rotation runbook is the mirror image: it applies to
// the default custody mode and is hidden under key_slots, which has its own
// pair.
func TestOperationalRunbooksAreServedFromTheBinary(t *testing.T) {
	for _, mode := range []string{config.MasterKeyModeFile, config.MasterKeyModeKeySlots} {
		runtime := &Runtime{config: config.Config{Storage: config.Storage{MasterKey: config.MasterKey{Mode: mode}}}}

		stale := httptest.NewRecorder()
		runtime.adminConfigurationStaleRunbook(stale, httptest.NewRequest(http.MethodGet, "/", nil))
		if stale.Code != http.StatusOK {
			t.Fatalf("mode %q: configuration-stale status=%d", mode, stale.Code)
		}
		for _, claim := range []string{"configuration_stale", "halro_activation_stale"} {
			if !strings.Contains(stale.Body.String(), claim) {
				t.Fatalf("mode %q: the stale runbook no longer states %q", mode, claim)
			}
		}

		rotation := httptest.NewRecorder()
		runtime.adminFileMasterKeyRotationRunbook(rotation, httptest.NewRequest(http.MethodGet, "/", nil))
		wantCode := http.StatusOK
		if mode == config.MasterKeyModeKeySlots {
			wantCode = http.StatusNotFound
		}
		if rotation.Code != wantCode {
			t.Fatalf("mode %q: file-rotation status=%d want=%d", mode, rotation.Code, wantCode)
		}
	}
}

// A leaked Gateway Key is possible under every Master Key custody mode, so
// unlike the two above this runbook must not be gated on one. A procedure that
// is missing exactly when it is needed is worse than no procedure.
func TestGatewayKeyCompromiseRunbookIsServedInEveryCustodyMode(t *testing.T) {
	for _, mode := range []string{config.MasterKeyModeFile, config.MasterKeyModeKeySlots} {
		runtime := &Runtime{config: config.Config{Storage: config.Storage{MasterKey: config.MasterKey{Mode: mode}}}}
		response := httptest.NewRecorder()
		runtime.adminGatewayKeyCompromiseRunbook(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("mode %q: status=%d", mode, response.Code)
		}
		if !strings.HasPrefix(response.Header().Get("Content-Type"), "text/markdown") ||
			response.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("mode %q: headers=%v", mode, response.Header())
		}
		// The runbook's load-bearing claims. If the embed goes stale or the file
		// is renamed, this is what says so.
		//
		// The last three are the corrections: a 503 from the delete leaves the
		// tombstone durable either way, so the runbook has to name both failure
		// strings and say which one leaves the key live. gateway_key.disable is
		// here because the CLI break-glass path audits under a different action
		// than the API one, and an operator verifying revocation by the wrong
		// name concludes there is no record.
		for _, claim := range []string{
			"invalid_api_key", "墓碑",
			"metadata unavailable", "audit unavailable", "gateway_key.disable",
		} {
			if !strings.Contains(response.Body.String(), claim) {
				t.Fatalf("mode %q: the runbook no longer states %q", mode, claim)
			}
		}
	}
}
