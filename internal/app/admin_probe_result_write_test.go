package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/provider"
	"github.com/akz142857/Halro/internal/safelog"
	bbolt "go.etcd.io/bbolt"
)

// passingProbeAdapter reaches its upstream and comes back clean, so the only
// thing left that can fail is recording the result.
type passingProbeAdapter struct{ canaryAdapter }

func (a *passingProbeAdapter) Probe(context.Context, string) error { return nil }

// A connection test that runs and then cannot be written back is the one failure
// in this path that says nothing about the upstream, and it used to leave no
// server-side trace at all: the console showed a refusal, the log showed nothing,
// and the operator had no way to learn that the probe had in fact succeeded.
//
// The record here is stale the way a real one goes stale — a capability name
// enters the dictionary and a record written before it no longer satisfies the
// evidence set — because that is what the store refuses in practice, and reads
// do not validate, so the refusal arrives at the write and nowhere earlier.
func TestProviderTestLogsAResultItCouldNotRecord(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(
		context.Background(), cfg, "admin", []byte("correct horse battery staple"),
	); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	runtime, err := Open(context.Background(), cfg, safelog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := loginAdminForTest(t, runtime)
	credentialResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/credentials", "",
		map[string]any{
			"name": "OpenAI production", "type": "openai",
			"base_url": "https://api.openai.com", "secret": "sk-provider-secret-canary",
		},
	)
	var credential credentialView
	if credentialResponse.Code != http.StatusCreated || json.Unmarshal(credentialResponse.Body.Bytes(), &credential) != nil {
		t.Fatalf("credential status=%d body=%s", credentialResponse.Code, credentialResponse.Body.String())
	}
	providerResponse := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers", "",
		map[string]any{
			"name": "OpenAI", "type": "openai", "base_url": "https://api.openai.com",
			"credential_id": credential.ID, "enabled": true,
			"capabilities": map[string]any{"chat": true},
		},
	)
	var instance struct {
		ID       string `json:"id"`
		Bindings []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"bindings"`
	}
	if providerResponse.Code != http.StatusCreated || json.Unmarshal(providerResponse.Body.Bytes(), &instance) != nil {
		t.Fatalf("provider status=%d body=%s", providerResponse.Code, providerResponse.Body.String())
	}
	runtime.Close()

	dropCapabilityEvidenceMember(t, cfg.MetadataPath(), instance.ID, "provider_executed_tools")

	logs.Reset()
	runtime, err = Open(context.Background(), cfg, safelog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cookie, csrf = loginAdminForTest(t, runtime)
	registry := provider.NewRegistry()
	adapter := &passingProbeAdapter{}
	for _, binding := range instance.Bindings {
		if !binding.Enabled {
			continue
		}
		if err := registry.RegisterBindingAdapter(instance.ID, binding.ID, adapter); err != nil {
			t.Fatal(err)
		}
	}
	runtime.providers.Replace(registry)
	logs.Reset()

	response := performAdminMutation(t, runtime, cookie, csrf,
		http.MethodPost, "/admin/api/v1/providers/"+instance.ID+"/test", "", nil,
	)
	if response.Code < 400 {
		t.Fatalf("a record the store refuses was written anyway: status=%d body=%s", response.Code, response.Body.String())
	}
	logged := logs.String()
	if !strings.Contains(logged, "provider connection test result could not be recorded") {
		t.Fatalf("the write failure left no trace: %s", logged)
	}
	if !strings.Contains(logged, "provider_executed_tools") {
		t.Fatalf("the log did not carry the store's own reason: %s", logged)
	}
	if strings.Contains(logged, "provider connection test failed") {
		t.Fatalf("a probe that succeeded was logged as a failure: %s", logged)
	}
}

// dropCapabilityEvidenceMember removes one evidence member from a stored
// provider, reproducing a record written before that capability existed. It goes
// through bbolt rather than the store because the store is what refuses the
// shape — there is no way to write it through the API that produced it.
func dropCapabilityEvidenceMember(t *testing.T, path, providerID, member string) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte("providers"))
		if bucket == nil {
			t.Fatal("providers bucket is absent")
		}
		raw := bucket.Get([]byte(providerID))
		if raw == nil {
			t.Fatalf("provider %s is absent", providerID)
		}
		var record map[string]json.RawMessage
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		removed := false
		strip := func(object map[string]json.RawMessage) error {
			encoded, ok := object["capability_evidence"]
			if !ok {
				return nil
			}
			var evidence map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &evidence); err != nil {
				return err
			}
			if _, present := evidence[member]; !present {
				return nil
			}
			delete(evidence, member)
			updated, err := json.Marshal(evidence)
			if err != nil {
				return err
			}
			object["capability_evidence"] = updated
			removed = true
			return nil
		}
		if err := strip(record); err != nil {
			return err
		}
		if encoded, ok := record["bindings"]; ok {
			var bindings []map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &bindings); err != nil {
				return err
			}
			for _, binding := range bindings {
				if err := strip(binding); err != nil {
					return err
				}
			}
			updated, err := json.Marshal(bindings)
			if err != nil {
				return err
			}
			record["bindings"] = updated
		}
		// Without this the test would pass against a record it never modified,
		// which is the shape of a check that proves nothing.
		if !removed {
			t.Fatalf("provider %s carried no %s evidence to remove", providerID, member)
		}
		updated, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(providerID), updated)
	}); err != nil {
		t.Fatal(err)
	}
}
