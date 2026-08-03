package observability_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Heimdall/internal/deadman"
	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"
)

func TestDeadmanConfigSchemaMatchesRuntimeForSharedCases(t *testing.T) {
	schemaData, err := os.ReadFile("external-probe/config.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(schemaData, &schema); err != nil {
		t.Fatalf("decode config schema: %v", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve config schema: %v", err)
	}

	exampleData, err := os.ReadFile("external-probe/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var example map[string]any
	if err := yaml.Unmarshal(exampleData, &example); err != nil {
		t.Fatalf("decode example config: %v", err)
	}

	tests := []struct {
		name   string
		valid  bool
		mutate func(map[string]any)
	}{
		{name: "example", valid: true, mutate: func(map[string]any) {}},
		{name: "compound duration", valid: true, mutate: func(config map[string]any) {
			config["interval"] = "1m30s"
			config["timeout"] = "1.5s"
			config["heartbeat_ttl"] = "3m"
		}},
		{name: "empty optional mtls fields with bearer", valid: true, mutate: func(config map[string]any) {
			candidate := target(config, "heimdall")
			candidate["tls"].(map[string]any)["client_cert_file"] = ""
			candidate["tls"].(map[string]any)["client_key_file"] = ""
		}},
		{name: "missing target kind", mutate: func(config map[string]any) {
			candidate := target(config, "alertmanager")
			candidate["kind"] = "heimdall"
		}},
		{name: "prometheus without freshness", mutate: func(config map[string]any) {
			delete(target(config, "prometheus"), "freshness")
		}},
		{name: "target without authentication", mutate: func(config map[string]any) {
			candidate := target(config, "heimdall")
			delete(candidate, "bearer_token_file")
			tls := candidate["tls"].(map[string]any)
			delete(tls, "client_cert_file")
			delete(tls, "client_key_file")
		}},
		{name: "unpaired client certificate", mutate: func(config map[string]any) {
			delete(target(config, "alertmanager")["tls"].(map[string]any), "client_key_file")
		}},
		{name: "notification without authentication", mutate: func(config map[string]any) {
			notification := config["notification"].(map[string]any)
			delete(notification, "bearer_token_file")
			tls := notification["tls"].(map[string]any)
			delete(tls, "client_cert_file")
			delete(tls, "client_key_file")
		}},
		{name: "malformed duration", mutate: func(config map[string]any) {
			config["interval"] = "soon"
		}},
		{name: "URL without host", mutate: func(config map[string]any) {
			target(config, "heimdall")["url"] = "https://"
		}},
		{name: "URL with userinfo", mutate: func(config map[string]any) {
			target(config, "heimdall")["url"] = "https://credential@example.invalid/ready"
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := clone(t, example)
			test.mutate(config)
			schemaErr := resolved.Validate(config)
			runtimeErr := validateRuntime(t, config)
			if (schemaErr == nil) != test.valid {
				t.Fatalf("schema validity = %v, want %v: %v", schemaErr == nil, test.valid, schemaErr)
			}
			if (runtimeErr == nil) != test.valid {
				t.Fatalf("runtime validity = %v, want %v: %v", runtimeErr == nil, test.valid, runtimeErr)
			}
		})
	}
}

func clone(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func target(config map[string]any, kind string) map[string]any {
	for _, raw := range config["targets"].([]any) {
		candidate := raw.(map[string]any)
		if candidate["kind"] == kind {
			return candidate
		}
	}
	panic("target fixture not found: " + kind)
}

func validateRuntime(t *testing.T, config map[string]any) error {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = deadman.LoadConfig(path)
	return err
}
