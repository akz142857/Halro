package deadman

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type clientRootTransport func(*http.Request) (*http.Response, error)

func (f clientRootTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestHAClientRootProbeRetriesReplicaAndChecksCluster(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("probe-only-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, first, second, want string
		calls                     int
	}{
		{"replica then Primary", `{"role":"replica","cluster_id":"ha"}`, `{"role":"primary","cluster_id":"ha"}`, "", 2},
		{"wrong cluster", `{"role":"replica","cluster_id":"other"}`, "", "ha_client_wrong_cluster", 1},
		{"identity gap", `{"role":"replica"}`, `{"role":"primary","cluster_id":"ha"}`, "ha_client_identity_missing", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: clientRootTransport(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Path != "/" || request.Method != http.MethodGet || !request.Close ||
					request.Header.Get("Authorization") != "Bearer probe-only-token" {
					t.Fatalf("unsafe HA client root request: %+v", request)
				}
				body := tc.first
				if calls > 1 {
					body = tc.second
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			cfg := Config{Cluster: "ha", Timeout: Duration(time.Second)}
			target := TargetConfig{Kind: "halro", Mode: "ha_client_root", URL: "https://service.invalid/", BearerTokenFile: tokenFile}
			_, reason := checkTargetWithClient(context.Background(), cfg, target, client)
			if reason != tc.want || calls != tc.calls {
				t.Fatalf("reason=%q calls=%d, want %q/%d", reason, calls, tc.want, tc.calls)
			}
		})
	}
}

func TestHAClientRootProbeNeverCallsProviderAndFailsAfterOnlyReplicas(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: clientRootTransport(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Path != "/" || request.Method != http.MethodGet || !request.Close {
			t.Fatalf("probe left the Service root: %s %s", request.Method, request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"role":"replica","cluster_id":"ha"}`)), Header: make(http.Header)}, nil
	})}
	cfg := Config{Cluster: "ha", Timeout: Duration(time.Second)}
	target := TargetConfig{Kind: "halro", Mode: "ha_client_root", URL: "https://service.invalid/"}
	_, reason := checkTargetWithClient(context.Background(), cfg, target, client)
	if reason != "ha_client_primary_not_observed" || calls != 12 {
		t.Fatalf("only Replica routes reason=%q calls=%d", reason, calls)
	}
}

func TestHAClientRootModeRequiresHalroServiceRoot(t *testing.T) {
	cfg := validConfig(t.TempDir(), "https://notify.invalid/v1/events", "ca.pem", "token")
	cfg.Targets[0].Mode = "ha_client_root"
	cfg.Targets[0].URL = "https://service.invalid/"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid HA client root rejected: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*TargetConfig)
		want   string
	}{
		"wrong kind":   {func(target *TargetConfig) { target.Kind = "alertmanager" }, "unsupported probe mode"},
		"wrong path":   {func(target *TargetConfig) { target.URL = "https://service.invalid/health/ready" }, "must use the HTTPS Service root"},
		"query":        {func(target *TargetConfig) { target.URL += "?debug=1" }, "must use the HTTPS Service root"},
		"anchor":       {func(target *TargetConfig) { target.AnchorURL = "https://service.invalid/anchors" }, "cannot use freshness or anchor_url"},
		"freshness":    {func(target *TargetConfig) { target.Freshness = &FreshnessConfig{} }, "cannot use freshness or anchor_url"},
		"unknown mode": {func(target *TargetConfig) { target.Mode = "anything" }, "unsupported probe mode"},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := cfg
			invalid.Targets = append([]TargetConfig(nil), cfg.Targets...)
			tc.mutate(&invalid.Targets[0])
			if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invalid HA client root mode not rejected for %q: %v", tc.want, err)
			}
		})
	}
}
