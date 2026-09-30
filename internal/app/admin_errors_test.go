package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/replication"
)

func TestAdminMutationReportsAmbiguousReplicationFailure(t *testing.T) {
	response := httptest.NewRecorder()
	adminMutationError(response, fmt.Errorf("metadata commit: %w: private peer detail", replication.ErrReplicationUnavailable))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", response.Code)
	}
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "replication_unavailable" || !strings.Contains(body.Error, "read resource state") ||
		strings.Contains(response.Body.String(), "private peer detail") {
		t.Fatalf("replication failure response=%s", response.Body.String())
	}
}
