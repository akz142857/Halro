package safetransport

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type staticResolver struct {
	addresses []netip.Addr
}

func (r staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r.addresses, nil
}

type recordingDialer struct {
	address string
	called  bool
}

func (d *recordingDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.called = true
	d.address = address
	return nil, errors.New("dial stopped by test")
}

func TestMixedPublicPrivateDNSAnswerIsRejectedBeforeDial(t *testing.T) {
	dialer := &recordingDialer{}
	client, err := NewClient(Options{
		Policy: Policy{RequireHTTPS: true, AllowedHosts: []string{"api.example.com"}},
		Resolver: staticResolver{addresses: []netip.Addr{
			netip.MustParseAddr("203.0.113.10"),
			netip.MustParseAddr("127.0.0.1"),
		}},
		Dialer:                dialer,
		ConnectTimeout:        time.Second,
		ResponseHeaderTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.Get("https://api.example.com/v1/models")
	if request != nil {
		request.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected address rejection, got %v", err)
	}
	if dialer.called {
		t.Fatal("dialer must not be called when any DNS answer is forbidden")
	}
}

func TestDialUsesValidatedIP(t *testing.T) {
	dialer := &recordingDialer{}
	client, err := NewClient(Options{
		Policy:                Policy{RequireHTTPS: true, AllowedHosts: []string{"api.example.com"}},
		Resolver:              staticResolver{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.10")}},
		Dialer:                dialer,
		ConnectTimeout:        time.Second,
		ResponseHeaderTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.Get("https://api.example.com/v1/models")
	if !dialer.called || dialer.address != "203.0.113.10:443" {
		t.Fatalf("dial did not use validated address: called=%v address=%q", dialer.called, dialer.address)
	}
}

func TestValidateURLPolicy(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "plaintext", url: "http://api.example.com"},
		{name: "userinfo", url: "https://user:pass@api.example.com"},
		{name: "loopback", url: "https://127.0.0.1"},
		{name: "metadata", url: "https://169.254.169.254"},
		{name: "wrong host", url: "https://evil.example.com"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ValidateURL(test.url, Policy{
				RequireHTTPS: true,
				AllowedHosts: []string{"api.example.com"},
			}); err == nil {
				t.Fatalf("expected %s to be rejected", test.url)
			}
		})
	}
}

func TestAudienceIsCanonical(t *testing.T) {
	audience, err := Audience("https://API.Example.com/v1", "account=one")
	if err != nil {
		t.Fatal(err)
	}
	if audience != "https://api.example.com:443:account=one" {
		t.Fatalf("unexpected audience: %q", audience)
	}
}

// The address classes Go exposes miss several ranges that are never an upstream
// provider and are reachable enough to matter: carrier-grade NAT carries
// Alibaba Cloud's metadata service, and the IPv6 tunnel prefixes re-encode an
// arbitrary IPv4 address inside an address that passes every other check.
func TestReservedAndTunnelAddressesAreRefused(t *testing.T) {
	refused := []struct {
		address string
		reason  string
	}{
		{"100.100.100.200", "Alibaba Cloud metadata"},
		{"169.254.169.254", "EC2 and GCE metadata"},
		{"100.64.0.1", "carrier-grade NAT"},
		{"0.1.2.3", "this network"},
		{"192.0.0.8", "IETF protocol assignments"},
		{"198.18.0.1", "benchmarking"},
		{"240.0.0.1", "reserved"},
		{"255.255.255.255", "broadcast"},
		{"64:ff9b::7f00:1", "NAT64 well-known"},
		{"64:ff9b:1::1", "NAT64 local-use"},
		{"2002:7f00:1::", "6to4 wrapping 127.0.0.1"},
		{"2001::1", "Teredo"},
	}
	for _, item := range refused {
		if err := validateAddress(netip.MustParseAddr(item.address), false); err == nil {
			t.Errorf("%s (%s) was allowed", item.address, item.reason)
		}
	}

	// AllowPrivate is how an operator points Halro at an internal provider,
	// and a Kubernetes pod in 100.64/10 is a real place for one to live. The
	// metadata address inside that range is not, and stays refused.
	if err := validateAddress(netip.MustParseAddr("100.64.0.1"), true); err != nil {
		t.Errorf("carrier-grade NAT refused even with private addresses allowed: %v", err)
	}
	if err := validateAddress(netip.MustParseAddr("100.100.100.200"), true); err == nil {
		t.Error("cloud metadata was allowed once private addresses were")
	}

	for _, allowed := range []string{"203.0.113.10", "8.8.8.8", "2606:4700:4700::1111"} {
		if err := validateAddress(netip.MustParseAddr(allowed), false); err != nil {
			t.Errorf("ordinary public address %s was refused: %v", allowed, err)
		}
	}
}
