package safetransport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type Policy struct {
	RequireHTTPS bool
	AllowPrivate bool
	AllowedHosts []string
}

type Options struct {
	Policy                Policy
	Resolver              Resolver
	Dialer                Dialer
	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	MaxConnsPerHost       int
}

func NewClient(options Options) (*http.Client, error) {
	if options.Resolver == nil {
		options.Resolver = net.DefaultResolver
	}
	if options.Dialer == nil {
		options.Dialer = &net.Dialer{Timeout: options.ConnectTimeout, KeepAlive: 30 * time.Second}
	}
	if options.ConnectTimeout <= 0 {
		return nil, errors.New("connect timeout must be positive")
	}
	if options.ResponseHeaderTimeout <= 0 {
		return nil, errors.New("response header timeout must be positive")
	}
	allowedHosts := make([]string, 0, len(options.Policy.AllowedHosts))
	for _, host := range options.Policy.AllowedHosts {
		normalized := normalizeHost(host)
		if normalized == "" {
			return nil, errors.New("allowed host cannot be empty")
		}
		allowedHosts = append(allowedHosts, normalized)
	}
	slices.Sort(allowedHosts)
	allowedHosts = slices.Compact(allowedHosts)
	options.Policy.AllowedHosts = allowedHosts

	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: options.ResponseHeaderTimeout,
		MaxIdleConns:          valueOrDefault(options.MaxIdleConns, 100),
		MaxIdleConnsPerHost:   valueOrDefault(options.MaxIdleConnsPerHost, 10),
		MaxConnsPerHost:       options.MaxConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
	}
	transport.DialContext = pinnedDialContext(options.Policy, options.Resolver, options.Dialer)

	return &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func ValidateURL(raw string, policy Policy) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("endpoint scheme must be http or https")
	}
	if policy.RequireHTTPS && parsed.Scheme != "https" {
		return nil, errors.New("endpoint must use https")
	}
	if parsed.User != nil {
		return nil, errors.New("endpoint userinfo is not allowed")
	}
	host := normalizeHost(parsed.Hostname())
	if host == "" {
		return nil, errors.New("endpoint host is required")
	}
	if strings.Contains(host, "%") {
		return nil, errors.New("IPv6 zone identifiers are not allowed")
	}
	if len(policy.AllowedHosts) > 0 && !hostAllowed(host, policy.AllowedHosts) {
		return nil, fmt.Errorf("endpoint host %q is not in the allowlist", host)
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if err := validateAddress(address, policy.AllowPrivate); err != nil {
			return nil, err
		}
	}
	parsed.Host = net.JoinHostPort(host, effectivePort(parsed))
	return parsed, nil
}

func Audience(raw, semantic string) (string, error) {
	return AudienceWithPolicy(raw, semantic, Policy{})
}

func AudienceWithPolicy(raw, semantic string, policy Policy) (string, error) {
	parsed, err := ValidateURL(raw, policy)
	if err != nil {
		return "", err
	}
	audience := parsed.Scheme + "://" + parsed.Host
	if semantic != "" {
		audience += ":" + semantic
	}
	return audience, nil
}

func pinnedDialContext(policy Policy, resolver Resolver, dialer Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("split outbound address: %w", err)
		}
		host = normalizeHost(host)
		if len(policy.AllowedHosts) > 0 && !hostAllowed(host, policy.AllowedHosts) {
			return nil, fmt.Errorf("outbound host %q is not in the allowlist", host)
		}
		var addresses []netip.Addr
		if literal, err := netip.ParseAddr(host); err == nil {
			addresses = []netip.Addr{literal}
		} else {
			addresses, err = resolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("resolve outbound host: %w", err)
			}
		}
		if len(addresses) == 0 {
			return nil, errors.New("outbound host resolved to no addresses")
		}
		for _, candidate := range addresses {
			if err := validateAddress(candidate, policy.AllowPrivate); err != nil {
				return nil, fmt.Errorf("outbound host %q: %w", host, err)
			}
		}
		// Dial exactly one of the addresses that was validated in this call.
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].Unmap().String(), port))
	}
}

func validateAddress(address netip.Addr, allowPrivate bool) error {
	address = address.Unmap()
	if !address.IsValid() {
		return errors.New("invalid outbound address")
	}
	if address.IsUnspecified() || address.IsLoopback() || address.IsMulticast() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
		return fmt.Errorf("address %s is not allowed", address)
	}
	if address.IsPrivate() && !allowPrivate {
		return fmt.Errorf("private address %s is not allowed", address)
	}
	if !address.IsGlobalUnicast() {
		return fmt.Errorf("non-global address %s is not allowed", address)
	}
	return nil
}

func effectivePort(parsed *url.URL) string {
	if parsed.Port() != "" {
		return parsed.Port()
	}
	if parsed.Scheme == "https" {
		return "443"
	}
	return "80"
}

func hostAllowed(host string, allowed []string) bool {
	host = normalizeHost(host)
	for _, candidate := range allowed {
		if host == normalizeHost(candidate) {
			return true
		}
	}
	return false
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
}

func valueOrDefault(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}
