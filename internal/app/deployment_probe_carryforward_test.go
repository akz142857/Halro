package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

// probedDeployment is the part of a deployment these tests reason about: which
// revision is stored, and which revision the last probe was evidence about.
type probedDeployment struct {
	ID               string                      `json:"id"`
	Revision         uint64                      `json:"revision"`
	Region           string                      `json:"region"`
	Enabled          bool                        `json:"enabled"`
	LastTestStatus   domain.DeploymentTestStatus `json:"last_test_status"`
	LastTestRevision uint64                      `json:"last_test_revision"`
}

func (d probedDeployment) validated() bool {
	return d.LastTestStatus == domain.DeploymentTestHealthy && d.LastTestRevision == d.Revision
}

func decodeProbedDeployment(t *testing.T, response *httptest.ResponseRecorder) probedDeployment {
	t.Helper()
	var deployment probedDeployment
	if err := json.Unmarshal(response.Body.Bytes(), &deployment); err != nil {
		t.Fatalf("decode deployment: %v body=%s", err, response.Body.String())
	}
	return deployment
}

// newProbedDeploymentForTest creates a disabled, priced deployment and records a
// healthy probe against its stored revision.
func newProbedDeploymentForTest(t *testing.T, runtime *Runtime, cookie *http.Cookie, csrf, providerID, model string, capabilities map[string]any) probedDeployment {
	t.Helper()
	created := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost, "/admin/api/v1/deployments", "",
		map[string]any{
			"name": model, "provider_id": providerID, "provider_model": model, "target_kind": "model_id",
			"mode": "operator_declared", "capabilities": capabilities,
			"max_concurrency": int64(2), "enabled": false,
		})
	if created.Code != http.StatusCreated {
		t.Fatalf("create deployment %s status=%d body=%s", model, created.Code, created.Body.String())
	}
	deployment := decodeProbedDeployment(t, created)
	createEffectiveMeteredPriceForTest(t, runtime, deployment.ID)
	return probeDeploymentForTest(t, runtime, cookie, csrf, providerID, deployment)
}

// probeDeploymentForTest runs the validation endpoint against a stub adapter.
// The registry is replaced on every call because each admin mutation reloads it
// from the stored providers, which have no adapter that can be probed offline.
func probeDeploymentForTest(t *testing.T, runtime *Runtime, cookie *http.Cookie, csrf, providerID string, deployment probedDeployment) probedDeployment {
	t.Helper()
	registry := provider.NewRegistry()
	if err := registry.RegisterAdapter(providerID, &adminProbeAdapter{}); err != nil {
		t.Fatal(err)
	}
	runtime.providers.Replace(registry)
	tested := performAdminMutation(t, runtime, cookie, csrf, http.MethodPost,
		"/admin/api/v1/deployments/"+deployment.ID+"/test", "", nil)
	if tested.Code != http.StatusOK {
		t.Fatalf("deployment test status=%d body=%s", tested.Code, tested.Body.String())
	}
	// The validation endpoint answers with the probe result, not the record, so
	// what was stored is read back.
	probed := decodeProbedDeployment(t, authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/deployments/"+deployment.ID))
	if !probed.validated() {
		t.Fatalf("probe did not land on the stored revision: %#v", probed)
	}
	return probed
}

func updateDeploymentForTest(t *testing.T, runtime *Runtime, cookie *http.Cookie, csrf string, deployment probedDeployment, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return performAdminMutation(t, runtime, cookie, csrf, http.MethodPut,
		"/admin/api/v1/deployments/"+deployment.ID, `"`+strconv.FormatUint(deployment.Revision, 10)+`"`, body)
}

// A stored probe result is evidence about one revision of one target. Every
// update advances the revision, so the question each of these asks is whether
// the result still says anything about what is being stored — carrying it
// forward where it does not is a fail-open that lets an unvalidated deployment
// into routing.
func TestDeploymentUpdateCarriesAProbeForwardOnlyWhenNothingItValidatedChanged(t *testing.T) {
	t.Run("widening capabilities invalidates the probe and blocks re-enable", func(t *testing.T) {
		runtime, bootstrap, _ := openBootstrappedRuntime(t)
		cookie, csrf := loginAdminForTest(t, runtime)
		deployment := newProbedDeploymentForTest(t, runtime, cookie, csrf, bootstrap.ProviderID, "gpt-widening",
			map[string]any{"chat": true})

		widened := updateDeploymentForTest(t, runtime, cookie, csrf, deployment, map[string]any{
			"name": "gpt-widening", "provider_id": bootstrap.ProviderID, "provider_model": "gpt-widening",
			"target_kind": "model_id", "mode": "operator_declared",
			"capabilities":    map[string]any{"chat": true, "streaming": true},
			"max_concurrency": int64(2), "enabled": false,
		})
		if widened.Code != http.StatusOK {
			t.Fatalf("widen status=%d body=%s", widened.Code, widened.Body.String())
		}
		after := decodeProbedDeployment(t, widened)
		if after.Enabled || after.LastTestRevision == after.Revision {
			t.Fatalf("widening left the probe current: %#v", after)
		}
		enable := updateDeploymentForTest(t, runtime, cookie, csrf, after, map[string]any{
			"name": "gpt-widening", "provider_id": bootstrap.ProviderID, "provider_model": "gpt-widening",
			"max_concurrency": int64(2), "enabled": true,
		})
		if enable.Code != http.StatusConflict {
			t.Fatalf("enable after widening status=%d body=%s", enable.Code, enable.Body.String())
		}
	})

	t.Run("region is part of the target and cannot be edited under a probe", func(t *testing.T) {
		runtime, bootstrap, _ := openBootstrappedRuntime(t)
		cookie, csrf := loginAdminForTest(t, runtime)
		deployment := newProbedDeploymentForTest(t, runtime, cookie, csrf, bootstrap.ProviderID, "gpt-region",
			map[string]any{"chat": true})
		if deployment.Region != "" {
			t.Fatalf("fixture already carries a region: %#v", deployment)
		}
		moved := updateDeploymentForTest(t, runtime, cookie, csrf, deployment, map[string]any{
			"name": "gpt-region", "provider_id": bootstrap.ProviderID, "provider_model": "gpt-region",
			"region": "us-east-1", "max_concurrency": int64(2), "enabled": false,
		})
		if moved.Code != http.StatusConflict {
			t.Fatalf("region edit status=%d body=%s", moved.Code, moved.Body.String())
		}
		stored := authenticatedAdminGet(t, runtime, cookie, "/admin/api/v1/deployments/"+deployment.ID)
		unchanged := decodeProbedDeployment(t, stored)
		if unchanged.Region != "" || unchanged.Revision != deployment.Revision {
			t.Fatalf("rejected region edit still changed the deployment: %#v", unchanged)
		}
	})

	t.Run("disabling invalidates the probe so re-enabling has to retest", func(t *testing.T) {
		runtime, bootstrap, _ := openBootstrappedRuntime(t)
		cookie, csrf := loginAdminForTest(t, runtime)
		deployment := newProbedDeploymentForTest(t, runtime, cookie, csrf, bootstrap.ProviderID, "gpt-cycle",
			map[string]any{"chat": true})
		operational := map[string]any{
			"name": "gpt-cycle", "provider_id": bootstrap.ProviderID, "provider_model": "gpt-cycle",
			"max_concurrency": int64(2),
		}
		body := func(enabled bool) map[string]any {
			next := map[string]any{"enabled": enabled}
			for key, value := range operational {
				next[key] = value
			}
			return next
		}
		enabled := updateDeploymentForTest(t, runtime, cookie, csrf, deployment, body(true))
		if enabled.Code != http.StatusOK {
			t.Fatalf("enable status=%d body=%s", enabled.Code, enabled.Body.String())
		}
		// Enabling presented a current probe to the gate, so the result still
		// describes what was stored. Re-enabling must not become free by way of
		// enabling making its own evidence stale.
		live := decodeProbedDeployment(t, enabled)
		if !live.validated() {
			t.Fatalf("enabling made its own validation stale: %#v", live)
		}
		disabled := updateDeploymentForTest(t, runtime, cookie, csrf, live, body(false))
		if disabled.Code != http.StatusOK {
			t.Fatalf("disable status=%d body=%s", disabled.Code, disabled.Body.String())
		}
		offline := decodeProbedDeployment(t, disabled)
		if offline.LastTestRevision == offline.Revision {
			t.Fatalf("taking the deployment out of service kept its probe current: %#v", offline)
		}
		reEnabled := updateDeploymentForTest(t, runtime, cookie, csrf, offline, body(true))
		if reEnabled.Code != http.StatusConflict {
			t.Fatalf("re-enable without a fresh probe status=%d body=%s", reEnabled.Code, reEnabled.Body.String())
		}
	})

	t.Run("an operational edit keeps the probe current", func(t *testing.T) {
		runtime, bootstrap, _ := openBootstrappedRuntime(t)
		cookie, csrf := loginAdminForTest(t, runtime)
		deployment := newProbedDeploymentForTest(t, runtime, cookie, csrf, bootstrap.ProviderID, "gpt-operational",
			map[string]any{"chat": true})
		renamed := updateDeploymentForTest(t, runtime, cookie, csrf, deployment, map[string]any{
			"name": "Renamed", "provider_id": bootstrap.ProviderID, "provider_model": "gpt-operational",
			"max_concurrency": int64(5), "enabled": false,
		})
		if renamed.Code != http.StatusOK {
			t.Fatalf("operational edit status=%d body=%s", renamed.Code, renamed.Body.String())
		}
		after := decodeProbedDeployment(t, renamed)
		if !after.validated() {
			t.Fatalf("a rename made the probe stale: %#v", after)
		}
	})
}
