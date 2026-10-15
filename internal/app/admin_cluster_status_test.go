package app

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/replication"
)

func TestAdminClusterStatusDistinguishesStandaloneAndLocalHAState(t *testing.T) {
	standalone := httptest.NewRecorder()
	(&Runtime{}).adminClusterStatus(standalone, httptest.NewRequest(http.MethodGet, "/admin/api/v1/cluster/status", nil))
	if standalone.Code != http.StatusOK || standalone.Body.String() != "{\"mode\":\"standalone\"}\n" {
		t.Fatalf("standalone status=%d body=%s", standalone.Code, standalone.Body.String())
	}

	state := replication.MemberState{
		Version: replication.StateVersion, ClusterID: "test-cluster", Incarnation: "inc_01", NodeID: "halro-0",
		Role: replication.RolePrimary, Term: 3, PromisedTerm: 3, DurableIndex: 9, ConfirmedIndex: 8, AppliedIndex: 8,
		OrderingHeadMAC: sha256.Sum256([]byte("ordering")),
		Projection:      replication.ProjectionState{Index: 8, MetadataEpoch: 1, MetadataSequence: 6},
		Peers:           []replication.StatePeer{{Name: "halro-1", Address: "private.invalid:9910", SPKISHA256: "sha256:" + strings.Repeat("a", 64)}},
	}
	publisher, err := replication.NewStatePublisher(filepath.Join(t.TempDir(), "state.json"), make([]byte, 32), state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(publisher.Close)
	runtime := &Runtime{replication: &replicationRuntime{role: replication.RolePrimary, publisher: publisher}}
	runtime.replication.startupReady.Store(true)
	response := httptest.NewRecorder()
	runtime.adminClusterStatus(response, httptest.NewRequest(http.MethodGet, "/admin/api/v1/cluster/status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("HA status=%d body=%s", response.Code, response.Body.String())
	}
	var view struct {
		Mode           string `json:"mode"`
		ClusterID      string `json:"cluster_id"`
		Role           string `json:"role"`
		StartupReady   bool   `json:"startup_ready"`
		DurableIndex   uint64 `json:"durable_index"`
		ConfirmedIndex uint64 `json:"confirmed_index"`
		Peers          []struct {
			NodeID    string `json:"node_id"`
			Connected bool   `json:"connected"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Mode != "ha" || view.ClusterID != "test-cluster" || view.Role != "primary" || !view.StartupReady || view.DurableIndex != 9 || view.ConfirmedIndex != 8 || len(view.Peers) != 1 || view.Peers[0].NodeID != "halro-1" || view.Peers[0].Connected {
		t.Fatalf("unexpected HA view: %+v", view)
	}
	if strings.Contains(response.Body.String(), "private.invalid") || strings.Contains(response.Body.String(), "sha256:") || strings.Contains(response.Body.String(), "ordering_head_mac") {
		t.Fatalf("status leaked peer connection details: %s", response.Body.String())
	}
}

func TestAdminClusterStatusRequiresAuthenticationOnBothRouters(t *testing.T) {
	for name, router := range map[string]http.Handler{
		"primary_or_standalone": (&Runtime{}).adminRouter(),
		"replica":               (&Runtime{}).replicaAdminRouter(),
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin/api/v1/cluster/status", nil))
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated cluster status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
