package hahealth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// ClientProbeResult records a stable machine code and the operator-facing
// observation. The probe never invokes a billable Gateway route.
type ClientProbeResult struct {
	Signal Signal
	Code   string
}

// ProbeClientService checks the HTTPS client Service root through fresh
// connections. Replica routes are retried because the Service intentionally
// includes Ready Replicas; an unidentified route can never be hidden by a
// later Primary response.
func ProbeClientService(ctx context.Context, client *http.Client, serviceURL, cluster, bearer string, attempts int) ClientProbeResult {
	if serviceURL == "" || client == nil || attempts < 1 {
		return ClientProbeResult{Signal{Unknown, "client Service probe not configured"}, "not_configured"}
	}
	observedResponse := false
	unexpectedResponse := false
	identityGap := false
	for attempt := 0; attempt < attempts; attempt++ {
		if ctx.Err() != nil {
			break
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, serviceURL, nil)
		if err != nil {
			break
		}
		// Kubernetes Service selection is per connection. Reusing a connection
		// would keep probing one Replica and create a false outage.
		request.Close = true
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := client.Do(request)
		if err != nil {
			continue
		}
		observedResponse = true
		var body struct {
			Role      string `json:"role"`
			ClusterID string `json:"cluster_id"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&body)
		_ = response.Body.Close()
		if response.StatusCode == http.StatusOK && decodeErr == nil && body.ClusterID != "" && body.ClusterID != cluster {
			return ClientProbeResult{Signal{Critical, "client Service reached a different cluster"}, "wrong_cluster"}
		}
		if response.StatusCode == http.StatusOK && (decodeErr != nil || body.ClusterID == "") {
			identityGap = true
			continue
		}
		if response.StatusCode != http.StatusOK {
			unexpectedResponse = true
			continue
		}
		if body.Role == "primary" {
			if identityGap {
				return ClientProbeResult{Signal{Unknown, "client Service returned a route without cluster identity"}, "identity_missing"}
			}
			return ClientProbeResult{Signal{Healthy, "client Service reached Primary"}, "healthy"}
		}
		if body.Role != "replica" {
			unexpectedResponse = true
		}
	}
	if !observedResponse {
		return ClientProbeResult{Signal{Unknown, "client Service probe transport unavailable"}, "transport_unavailable"}
	}
	if unexpectedResponse {
		return ClientProbeResult{Signal{Critical, "client Service returned unexpected responses"}, "unexpected_response"}
	}
	if identityGap {
		return ClientProbeResult{Signal{Unknown, "client Service returned a route without cluster identity"}, "identity_missing"}
	}
	return ClientProbeResult{Signal{Unknown, "client Service returned only Replica routes within probe budget"}, "primary_not_observed"}
}
