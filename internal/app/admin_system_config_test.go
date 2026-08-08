package app

import (
	"testing"

	"github.com/akz142857/Halro/internal/config"
)

func TestSummarizeSystemConfigUsesEffectiveTypedValues(t *testing.T) {
	cfg := config.Config{
		Version: 1,
		Server: config.Server{
			GatewayListen: "127.0.0.1:8080",
			AdminListen:   "127.0.0.1:8081",
			MetricsListen: "127.0.0.1:9090",
		},
		TLS: config.TLS{Enabled: true, CertFile: "/etc/halro/tls.crt"},
		Storage: config.Storage{
			DataDir:      "/var/lib/halro",
			MetadataFile: "halro.db",
			MasterKey:    config.MasterKey{Mode: config.MasterKeyModeKeySlots},
		},
		Security: config.Security{AllowPrivateWebhooks: true},
		Metrics:  config.Metrics{Enabled: true, RequireAuth: true},
	}

	sections := summarizeSystemConfig(cfg)
	if len(sections) != 5 {
		t.Fatalf("sections=%d, want 5", len(sections))
	}
	want := map[string]string{
		"gateway_listen":       "127.0.0.1:8080",
		"tls_enabled":          "true",
		"tls_cert_file":        "/etc/halro/tls.crt",
		"master_key_mode":      config.MasterKeyModeKeySlots,
		"private_webhooks":     "true",
		"metrics_require_auth": "true",
	}
	for _, section := range sections {
		for _, fact := range section.Facts {
			if expected, ok := want[fact.ID]; ok {
				if fact.Value != expected {
					t.Errorf("%s=%q, want %q", fact.ID, fact.Value, expected)
				}
				delete(want, fact.ID)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing summary facts: %#v", want)
	}
}
