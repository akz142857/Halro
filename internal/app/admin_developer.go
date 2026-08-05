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
	Enabled        bool   `json:"enabled"`
}

// The workbench makes the Admin listener carry data-plane traffic, which bypasses network
// controls applied only to the Gateway listener. Deployments can turn it off.
func (r *Runtime) developerWorkbenchEnabled() bool {
	return r.config.Admin.DeveloperWorkbench != "disabled"
}

func (r *Runtime) getAdminDeveloperConfig(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, developerConfigView{
		GatewayBaseURL: r.developerGatewayBaseURL(request),
		Enabled:        r.developerWorkbenchEnabled(),
	})
}

func (r *Runtime) executeAdminDeveloperRequest(writer http.ResponseWriter, request *http.Request) {
	if !r.developerWorkbenchEnabled() {
		writeJSON(writer, http.StatusForbidden, map[string]string{"error": "developer workbench is disabled"})
		return
	}
	endpoint := chi.URLParam(request, "endpoint")
	path, exists := map[string]string{
		"chat-completions": "/v1/chat/completions",
		"responses":        "/v1/responses",
		"embeddings":       "/v1/embeddings",
	}[endpoint]
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
	streaming := strings.Contains(strings.ToLower(request.Header.Get("Accept")), "text/event-stream")
	if streaming {
		upstream.Header.Set("Accept", "text/event-stream")
	} else {
		upstream.Header.Set("Accept", "application/json")
	}
	// Behind a trusted proxy the Gateway requires a forwarded chain and rejects the
	// request without one. The admin caller is the real source, so state it explicitly.
	if forwarded := strings.TrimSpace(request.Header.Get("X-Forwarded-For")); forwarded != "" {
		upstream.Header.Set("X-Forwarded-For", forwarded)
	} else if host, _, splitErr := net.SplitHostPort(request.RemoteAddr); splitErr == nil {
		upstream.Header.Set("X-Forwarded-For", host)
	}
	// This request spends real money against a real project, so it has to leave the same
	// audit trail as every other admin mutation. Capture what the Gateway answered first.
	recorder := &developerExecutionRecorder{ResponseWriter: writer}
	r.gatewayHandler().ServeHTTP(recorder, upstream)
	r.auditDeveloperExecution(request, endpoint, streaming, recorder)
}

func (r *Runtime) auditDeveloperExecution(
	request *http.Request, endpoint string, streaming bool, recorder *developerExecutionRecorder,
) {
	admin, ok := request.Context().Value(adminContextKey{}).(adminRequestContext)
	if !ok {
		return
	}
	outcome := "success"
	if recorder.status >= http.StatusBadRequest {
		outcome = "failure"
	}
	metadata := map[string]any{
		"endpoint":    endpoint,
		"http_status": recorder.status,
		"streaming":   streaming,
	}
	if requestID := recorder.Header().Get("X-Request-ID"); requestID != "" {
		metadata["request_id"] = requestID
	}
	if err := r.appendAdminAuditWithMetadata(
		"admin_user", admin.session.Username, "developer.execute", "developer_endpoint", endpoint,
		outcome, "", metadata,
	); err != nil {
		r.logger.Warn("developer execution audit failed", "error", err)
	}
}

// developerExecutionRecorder captures the Gateway status for the audit record. It must
// forward Flush so streamed responses still reach the browser incrementally.
type developerExecutionRecorder struct {
	http.ResponseWriter
	status int
}

func (recorder *developerExecutionRecorder) WriteHeader(status int) {
	if recorder.status == 0 {
		recorder.status = status
	}
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *developerExecutionRecorder) Write(payload []byte) (int, error) {
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	return recorder.ResponseWriter.Write(payload)
}

func (recorder *developerExecutionRecorder) Flush() {
	if flusher, ok := recorder.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (recorder *developerExecutionRecorder) Unwrap() http.ResponseWriter {
	return recorder.ResponseWriter
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
	// A loopback listener is only reachable as loopback: substituting the admin host would
	// hand out an address the caller cannot connect to. Only unspecified hosts get resolved.
	configuredIP := net.ParseIP(host)
	if host == "" || configuredIP != nil && configuredIP.IsUnspecified() {
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
