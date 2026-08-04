package app

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
)

type developerConfigView struct {
	GatewayBaseURL string `json:"gateway_base_url"`
}

func (r *Runtime) getAdminDeveloperConfig(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, developerConfigView{GatewayBaseURL: r.developerGatewayBaseURL(request)})
}

func (r *Runtime) executeAdminDeveloperRequest(writer http.ResponseWriter, request *http.Request) {
	path, exists := map[string]string{
		"chat-completions": "/v1/chat/completions",
		"responses":        "/v1/responses",
		"embeddings":       "/v1/embeddings",
	}[chi.URLParam(request, "endpoint")]
	if !exists {
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "unsupported developer endpoint"})
		return
	}
	upstream, err := http.NewRequestWithContext(withoutContextValues{request.Context()}, http.MethodPost, path, request.Body)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid developer request"})
		return
	}
	upstream.RemoteAddr = request.RemoteAddr
	upstream.Header.Set("Authorization", request.Header.Get("Authorization"))
	upstream.Header.Set("Content-Type", "application/json")
	if strings.Contains(strings.ToLower(request.Header.Get("Accept")), "text/event-stream") {
		upstream.Header.Set("Accept", "text/event-stream")
	} else {
		upstream.Header.Set("Accept", "application/json")
	}
	r.gatewayRouter().ServeHTTP(writer, upstream)
}

// withoutContextValues preserves cancellation and deadlines while preventing
// the internally constructed Gateway request from inheriting Admin session data.
type withoutContextValues struct{ context.Context }

func (withoutContextValues) Value(any) any { return nil }

func (r *Runtime) developerGatewayBaseURL(request *http.Request) string {
	host, port, err := net.SplitHostPort(r.config.Server.GatewayListen)
	if err != nil {
		return ""
	}
	configuredIP := net.ParseIP(host)
	if host == "" || configuredIP != nil && (configuredIP.IsLoopback() || configuredIP.IsUnspecified()) {
		requestHost := request.Host
		if parsedHost, _, splitErr := net.SplitHostPort(requestHost); splitErr == nil {
			requestHost = parsedHost
		}
		requestHost = strings.Trim(requestHost, "[]")
		if requestHost != "" {
			host = requestHost
		}
	}
	scheme := "http"
	if r.config.TLS.Enabled {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port)}).String()
}
