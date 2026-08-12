package safetransport

import (
	"context"
	"errors"
	"net"
	"net/http"
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
	audience, err := AudienceWithPolicy("https://API.Example.com/v1", "account=one", Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if audience != "https://api.example.com:443:account=one" {
		t.Fatalf("unexpected audience: %q", audience)
	}
}

func TestClientIgnoresEnvironmentProxyAndRefusesRedirects(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9999")
	client, err := NewClient(Options{
		Policy:   Policy{RequireHTTPS: true, AllowedHosts: []string{"api.example.com"}},
		Resolver: staticResolver{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.10")}},
		Dialer:   &recordingDialer{}, ConnectTimeout: time.Second, ResponseHeaderTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("SafeTransport must not consult environment proxies: %#v", client.Transport)
	}
	request, err := http.NewRequest(http.MethodGet, "https://evil.example/redirect", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(request, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy err=%v", err)
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

type failingResolver struct{ err error }

func (r failingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, r.err
}

// Every refusal this package makes in the dialer happens with no connection
// open, so it must say so. Without the marker these arrive at the caller as
// plain errors and get classified as "the provider may already have served it",
// which settles the attempt at its full reservation and blocks failover — the
// one case with total certainty treated as the uncertain one.
func TestRefusalsInTheDialerSayNothingWasSent(t *testing.T) {
	denied := staticResolver{addresses: []netip.Addr{netip.MustParseAddr("169.254.169.254")}}
	empty := staticResolver{}
	tests := []struct {
		name     string
		policy   Policy
		resolver Resolver
		url      string
	}{
		{
			name:     "address is in a denied range",
			policy:   Policy{RequireHTTPS: true, AllowedHosts: []string{"api.example.com"}},
			resolver: denied,
			url:      "https://api.example.com/v1/models",
		},
		{
			name:     "resolver answers with no addresses",
			policy:   Policy{RequireHTTPS: true, AllowedHosts: []string{"api.example.com"}},
			resolver: empty,
			url:      "https://api.example.com/v1/models",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dialer := &recordingDialer{}
			client, err := NewClient(Options{
				Policy: test.policy, Resolver: test.resolver, Dialer: dialer,
				ConnectTimeout: time.Second, ResponseHeaderTimeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Get(test.url)
			if response != nil {
				response.Body.Close()
			}
			if !errors.Is(err, ErrRefusedBeforeSend) {
				t.Fatalf("refusal did not report that nothing was sent: %v", err)
			}
			if dialer.called {
				t.Fatal("the dialer ran for a request this package refused")
			}
		})
	}
}

// The allowlist check inside the dialer is a second gate: ValidateURL already
// refuses an unlisted host at configuration time, so this one only fires if a
// caller reaches the transport another way. It must carry the marker too.
func TestDialerAllowlistRefusalSaysNothingWasSent(t *testing.T) {
	dialer := &recordingDialer{}
	dial := pinnedDialContext(
		Policy{RequireHTTPS: true, AllowedHosts: []string{"api.example.com"}},
		staticResolver{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.10")}},
		dialer,
	)
	_, err := dial(context.Background(), "tcp", "elsewhere.example.net:443")
	if !errors.Is(err, ErrRefusedBeforeSend) {
		t.Fatalf("allowlist refusal did not report that nothing was sent: %v", err)
	}
	if _, err := dial(context.Background(), "tcp", "no-port-here"); !errors.Is(err, ErrRefusedBeforeSend) {
		t.Fatalf("malformed address refusal did not report that nothing was sent: %v", err)
	}
	if dialer.called {
		t.Fatal("the dialer ran for a refused address")
	}
}

// A resolver failure is the network's answer rather than this package's refusal.
// It stays unmarked because it already carries *net.DNSError, which callers
// recognise on its own — marking it would claim this package decided something
// it did not.
func TestResolverFailureIsNotMarkedAsOurRefusal(t *testing.T) {
	dial := pinnedDialContext(
		Policy{RequireHTTPS: true},
		failingResolver{err: &net.DNSError{Err: "no such host", Name: "api.example.com", IsNotFound: true}},
		&recordingDialer{},
	)
	_, err := dial(context.Background(), "tcp", "api.example.com:443")
	if errors.Is(err, ErrRefusedBeforeSend) {
		t.Fatalf("a resolver failure was reported as this package's refusal: %v", err)
	}
	var resolution *net.DNSError
	if !errors.As(err, &resolution) {
		t.Fatalf("a resolver failure lost its DNS error: %v", err)
	}
}
