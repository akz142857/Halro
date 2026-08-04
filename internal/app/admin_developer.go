package app

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

type developerConfigView struct {
	GatewayBaseURL string `json:"gateway_base_url"`
}

func (r *Runtime) getAdminDeveloperConfig(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, developerConfigView{GatewayBaseURL: r.developerGatewayBaseURL(request)})
}

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
