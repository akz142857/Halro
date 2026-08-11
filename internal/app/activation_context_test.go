package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

func activationTestRuntime(t *testing.T) (*Runtime, BootstrapResult, loggedInAdmin) {
	t.Helper()
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapAdmin(context.Background(), cfg, "admin", []byte(stepUpTestPassword)); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := Bootstrap(context.Background(), cfg, BootstrapOptions{
		ProviderName: "OpenAI", ProviderType: domain.ProviderOpenAI,
		ProviderBaseURL: "https://api.openai.com", ProviderModel: "gpt-test", PublicModel: "chat",
		ProjectName: "Activation", BillingMode: domain.BillingModeFree,
	}, []byte("provider-secret"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	return runtime, bootstrap, loginTestAdmin(t, runtime, "admin", stepUpTestPassword)
}

// canceledRequest is an Admin request whose client has gone away: a disconnect, a
// proxy that gave up, or an expired deadline. The store refuses reads on such a
// context, which is what used to make the difference between a revocation being
// durable and it being in force.
func canceledRequest(t *testing.T, session loggedInAdmin) *http.Request {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return adminMutationRequest(t, http.MethodDelete, "/admin/api/v1/irrelevant", session, nil).WithContext(ctx)
}

// The revoke commits, then the client disappears before the auth snapshot is
// rebuilt. Activation used to run on the request's context, so it read a
// canceled context, returned early, and left the previous snapshot in place:
// bbolt said the key was revoked while the data plane went on accepting it,
// until some later mutation happened to succeed or the process restarted.
//
// Decomposed the way the handler runs it — commit, then activate — because the
// window is between the write's context check and activation's, and driving it
// through the handler would only reproduce it by luck.
func TestRevokedKeyStopsAuthenticatingWhenTheAdminClientDisconnects(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	if _, err := runtime.auth.Authenticate(bootstrap.GatewayKey, runtime.now()); err != nil {
		t.Fatalf("the bootstrapped key does not authenticate to begin with: %v", err)
	}

	key, err := runtime.store.GetGatewayKey(context.Background(), bootstrap.KeyID)
	if err != nil {
		t.Fatal(err)
	}
	key.Enabled = false
	if _, err := runtime.store.PutGatewayKey(context.Background(), key, key.Revision, nil); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	if !runtime.refreshAdminAuth(recorder, canceledRequest(t, session)) {
		t.Fatalf("activation gave up because the client had gone: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := runtime.auth.Authenticate(bootstrap.GatewayKey, runtime.now()); err == nil {
		t.Fatal("a revoked gateway key still authenticates after activation")
	}
}

// Same for a project: disabling it must take every one of its keys out of
// service whether or not the Admin client waited for the reply.
func TestDisabledProjectStopsAuthorizingWhenTheAdminClientDisconnects(t *testing.T) {
	runtime, bootstrap, session := activationTestRuntime(t)
	project, err := runtime.store.GetProject(context.Background(), bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	project.Enabled = false
	if _, err := runtime.store.PutProject(context.Background(), project, project.Revision, nil); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	if !runtime.refreshAdminAuth(recorder, canceledRequest(t, session)) {
		t.Fatalf("activation gave up because the client had gone: status=%d", recorder.Code)
	}
	if _, err := runtime.auth.Authenticate(bootstrap.GatewayKey, runtime.now()); err == nil {
		t.Fatal("a key belonging to a disabled project still authenticates")
	}
}

// The topology half is enforced by activateTopology accepting no context at all,
// so no handler can hand it the client's. This pins the consequence it buys: a
// route tombstoned in the store is out of the routing candidates once activation
// has run, and activation runs on the runtime's own context.
func TestDeletedRouteStopsRoutingOnRuntimeOwnedActivation(t *testing.T) {
	runtime, bootstrap, _ := activationTestRuntime(t)
	if targets := runtime.providers.ResolveCandidatesForEvidence("chat", provider.OperationChat, domain.EvidenceDeclared); len(targets) != 1 {
		t.Fatalf("expected the bootstrapped route to be routable: %+v", targets)
	}

	route, err := runtime.store.GetRoute(context.Background(), bootstrap.RouteID)
	if err != nil {
		t.Fatal(err)
	}
	now := runtime.now().UTC()
	route.Enabled, route.DeletedAt = false, &now
	if _, err := runtime.store.PutRoute(context.Background(), route, route.Revision, nil); err != nil {
		t.Fatal(err)
	}

	if err := runtime.activateTopology(); err != nil {
		t.Fatalf("activateTopology: %v", err)
	}
	if targets := runtime.providers.ResolveCandidatesForEvidence("chat", provider.OperationChat, domain.EvidenceDeclared); len(targets) != 0 {
		t.Fatalf("a deleted route is still a routing candidate: %+v", targets)
	}
}
