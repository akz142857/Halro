package safetransport

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type dialFunc func(context.Context, string, string) (net.Conn, error)

func (f dialFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

func TestHTTPConnectDialerSendsValidatedIPAndPreservesBufferedTunnelBytes(t *testing.T) {
	endpoint, err := url.Parse("http://proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	var request string
	var requestMu sync.Mutex
	dialer, err := NewHTTPConnectDialer(HTTPConnectOptions{
		ID: "proxy-a", Endpoint: endpoint,
		Resolver: staticResolver{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.20")}},
		DirectDialer: dialFunc(func(_ context.Context, _, address string) (net.Conn, error) {
			if address != "203.0.113.20:8080" {
				t.Errorf("proxy dial address=%q", address)
			}
			return clientSide, nil
		}),
		BasicAuth: &BasicAuth{Username: "halro", Password: []byte("secret")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dialer.Close()
	go func() {
		reader := bufio.NewReader(serverSide)
		var builder strings.Builder
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			builder.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		requestMu.Lock()
		request = builder.String()
		requestMu.Unlock()
		_, _ = io.WriteString(serverSide, "HTTP/1.1 200 Connection Established\r\nX-Proxy: ok\r\n\r\nhello")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", "203.0.113.10:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload := make([]byte, 5)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatal(err)
	}
	if string(payload) != "hello" {
		t.Fatalf("buffered tunnel payload=%q", payload)
	}
	requestMu.Lock()
	captured := request
	requestMu.Unlock()
	if !strings.HasPrefix(captured, "CONNECT 203.0.113.10:443 HTTP/1.1\r\nHost: 203.0.113.10:443\r\n") {
		t.Fatalf("CONNECT did not use the validated literal target: %q", captured)
	}
	if strings.Contains(captured, "api.example") || !strings.Contains(captured, "Proxy-Authorization: Basic aGFscm86c2VjcmV0") {
		t.Fatalf("CONNECT request has wrong authority or auth: %q", captured)
	}
}

func TestHTTPConnectDialerRejectsMixedProxyDNSBeforeDial(t *testing.T) {
	endpoint, _ := url.Parse("http://proxy.example:8080")
	called := false
	dialer, err := NewHTTPConnectDialer(HTTPConnectOptions{
		ID: "proxy-a", Endpoint: endpoint,
		Resolver: staticResolver{addresses: []netip.Addr{
			netip.MustParseAddr("203.0.113.20"), netip.MustParseAddr("127.0.0.1"),
		}},
		DirectDialer: dialFunc(func(context.Context, string, string) (net.Conn, error) {
			called = true
			return nil, errors.New("unexpected dial")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dialer.Close()
	_, err = dialer.DialContext(context.Background(), "tcp", "203.0.113.10:443")
	if err == nil || !errors.Is(err, ErrRefusedBeforeSend) {
		t.Fatalf("mixed proxy DNS err=%v", err)
	}
	if called {
		t.Fatal("proxy socket was opened before all DNS answers were validated")
	}
}

func TestHTTPConnectDialerReturnsSafeStructuredStatusError(t *testing.T) {
	endpoint, _ := url.Parse("http://127.0.0.1:8080")
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	dialer, err := NewHTTPConnectDialer(HTTPConnectOptions{
		ID: "proxy-a", Endpoint: endpoint,
		EndpointPolicy: ProxyEndpointPolicy{AllowLoopback: true},
		DirectDialer:   dialFunc(func(context.Context, string, string) (net.Conn, error) { return clientSide, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dialer.Close()
	go func() {
		reader := bufio.NewReader(serverSide)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil || line == "\r\n" {
				break
			}
		}
		_, _ = io.WriteString(serverSide, "HTTP/1.1 407 secret-reason-canary\r\nContent-Length: 18\r\n\r\nsecret-body-canary")
	}()
	_, err = dialer.DialContext(context.Background(), "tcp", "203.0.113.10:443")
	var proxyErr *ProxyError
	if !errors.As(err, &proxyErr) || proxyErr.Stage != ProxyStageStatus || proxyErr.StatusCode != 407 {
		t.Fatalf("structured proxy error=%#v (%v)", proxyErr, err)
	}
	if !errors.Is(err, ErrRefusedBeforeSend) {
		t.Fatalf("CONNECT refusal was not marked unsent: %v", err)
	}
	if strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("proxy error leaked response or endpoint: %q", err)
	}
}

func TestHTTPConnectDialerCloseClearsAndFailsClosed(t *testing.T) {
	endpoint, _ := url.Parse("http://proxy.example:8080")
	dialer, err := NewHTTPConnectDialer(HTTPConnectOptions{
		ID: "proxy-a", Endpoint: endpoint,
		Resolver:  staticResolver{addresses: []netip.Addr{netip.MustParseAddr("203.0.113.20")}},
		BasicAuth: &BasicAuth{Username: "halro", Password: []byte("secret")},
	})
	if err != nil {
		t.Fatal(err)
	}
	dialer.Close()
	if len(dialer.authHeader) != 0 {
		t.Fatal("Close retained proxy authentication material")
	}
	if _, err := dialer.DialContext(context.Background(), "tcp", "203.0.113.10:443"); err == nil || !errors.Is(err, ErrRefusedBeforeSend) {
		t.Fatalf("closed connector err=%v", err)
	}
}

func TestHTTPConnectDialerFormatsIPv6LiteralAuthority(t *testing.T) {
	endpoint, _ := url.Parse("http://127.0.0.1:8080")
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	dialer, err := NewHTTPConnectDialer(HTTPConnectOptions{
		ID: "proxy-a", Endpoint: endpoint,
		EndpointPolicy: ProxyEndpointPolicy{AllowLoopback: true},
		DirectDialer:   dialFunc(func(context.Context, string, string) (net.Conn, error) { return clientSide, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dialer.Close()
	requestLine := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(serverSide)
		line, _ := reader.ReadString('\n')
		requestLine <- line
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil || line == "\r\n" {
				break
			}
		}
		_, _ = io.WriteString(serverSide, "HTTP/1.1 200 Connection Established\r\n\r\n")
	}()
	conn, err := dialer.DialContext(context.Background(), "tcp", "[2001:db8::10]:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if line := <-requestLine; line != "CONNECT [2001:db8::10]:443 HTTP/1.1\r\n" {
		t.Fatalf("IPv6 CONNECT line=%q", line)
	}
}

func TestHTTPConnectDialerRejectsOversizedResponseHeaders(t *testing.T) {
	endpoint, _ := url.Parse("http://127.0.0.1:8080")
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	dialer, err := NewHTTPConnectDialer(HTTPConnectOptions{
		ID: "proxy-a", Endpoint: endpoint,
		EndpointPolicy: ProxyEndpointPolicy{AllowLoopback: true},
		DirectDialer:   dialFunc(func(context.Context, string, string) (net.Conn, error) { return clientSide, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dialer.Close()
	go func() {
		reader := bufio.NewReader(serverSide)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil || line == "\r\n" {
				break
			}
		}
		_, _ = io.WriteString(serverSide, "HTTP/1.1 200 Connection Established\r\nX-Fill: "+strings.Repeat("a", maxConnectResponseHeaderBytes)+"\r\n\r\n")
	}()
	_, err = dialer.DialContext(context.Background(), "tcp", "203.0.113.10:443")
	var proxyErr *ProxyError
	if !errors.As(err, &proxyErr) || proxyErr.Stage != ProxyStageRead || !errors.Is(err, ErrRefusedBeforeSend) {
		t.Fatalf("oversized response err=%v", err)
	}
}
