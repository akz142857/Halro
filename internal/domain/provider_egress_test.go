package domain

import (
	"strings"
	"testing"
	"time"
)

func TestProviderEgressProxyIDSyntax(t *testing.T) {
	for _, value := range []string{"", "mihomo-aws", "proxy_1", "egress.prod"} {
		if !validEgressProxyID(value) {
			t.Errorf("valid egress proxy ID %q was refused", value)
		}
	}
	for _, value := range []string{"Proxy", "-proxy", "proxy/path", "proxy host", strings.Repeat("a", 64)} {
		if validEgressProxyID(value) {
			t.Errorf("invalid egress proxy ID %q was accepted", value)
		}
	}
}

func TestProviderEgressProxyValidation(t *testing.T) {
	now := time.Now().UTC()
	valid := ProviderEgressProxy{
		ID: "corp-egress", Name: "Corporate egress", Kind: ProviderEgressProxyKindHTTPConnect,
		Endpoint: "https://proxy.example.com:8443", CreatedAt: now, UpdatedAt: now,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid proxy: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*ProviderEgressProxy)
	}{
		{"empty id", func(p *ProviderEgressProxy) { p.ID = "" }},
		{"userinfo", func(p *ProviderEgressProxy) { p.Endpoint = "https://user:secret@proxy.example.com:8443" }},
		{"missing port", func(p *ProviderEgressProxy) { p.Endpoint = "https://proxy.example.com" }},
		{"path", func(p *ProviderEgressProxy) { p.Endpoint = "https://proxy.example.com:8443/connect" }},
		{"unsupported kind", func(p *ProviderEgressProxy) { p.Kind = "socks5" }},
		{"cleartext auth without opt in", func(p *ProviderEgressProxy) {
			p.Endpoint = "http://proxy.example.com:8080"
			p.BasicAuthCredentialID = "cred_auth"
		}},
		{"https carrying stale cleartext opt in", func(p *ProviderEgressProxy) {
			p.BasicAuthCredentialID = "cred_auth"
			p.AllowCleartextBasicAuth = true
		}},
		{"unused cleartext opt in", func(p *ProviderEgressProxy) { p.AllowCleartextBasicAuth = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatalf("invalid proxy accepted: %#v", candidate)
			}
		})
	}
}
