package safetransport

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const maxConnectResponseHeaderBytes = 32 << 10

type ProxyEndpointPolicy struct {
	AllowPrivate  bool
	AllowLoopback bool
}

type BasicAuth struct {
	Username string
	Password []byte
}

type HTTPConnectOptions struct {
	ID             string
	Endpoint       *url.URL
	EndpointPolicy ProxyEndpointPolicy
	Resolver       Resolver
	DirectDialer   Dialer
	BasicAuth      *BasicAuth
}

type ProxyStage string

const (
	ProxyStageResolve ProxyStage = "proxy_resolve"
	ProxyStageDial    ProxyStage = "proxy_dial"
	ProxyStageTLS     ProxyStage = "proxy_tls"
	ProxyStageWrite   ProxyStage = "connect_write"
	ProxyStageRead    ProxyStage = "connect_read"
	ProxyStageStatus  ProxyStage = "connect_status"
)

// ProxyError exposes only bounded diagnostics. The underlying cause remains
// available to errors.Is/errors.As for retry classification, but is never
// interpolated into Error: proxy responses and network errors can contain
// operator-private endpoints or arbitrary upstream text.
type ProxyError struct {
	ProxyID    string
	Stage      ProxyStage
	StatusCode int
	err        error
}

func (e *ProxyError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("provider egress proxy %q failed at %s with status %d", e.ProxyID, e.Stage, e.StatusCode)
	}
	return fmt.Sprintf("provider egress proxy %q failed at %s", e.ProxyID, e.Stage)
}

func (e *ProxyError) Unwrap() error { return e.err }

// HTTPConnectDialer establishes one CONNECT tunnel per DialContext call. It
// contains the only in-memory copy of the precomputed Basic authorization
// value, and Close makes subsequent dials fail closed before clearing it.
type HTTPConnectDialer struct {
	id             string
	endpoint       url.URL
	endpointPolicy ProxyEndpointPolicy
	resolver       Resolver
	directDialer   Dialer
	mu             sync.RWMutex
	authHeader     []byte
	closed         bool
}

func NewHTTPConnectDialer(options HTTPConnectOptions) (*HTTPConnectDialer, error) {
	if options.ID == "" {
		return nil, errors.New("proxy id is required")
	}
	if options.Endpoint == nil {
		return nil, errors.New("proxy endpoint is required")
	}
	endpoint := *options.Endpoint
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, errors.New("proxy endpoint scheme must be http or https")
	}
	if endpoint.User != nil || endpoint.Hostname() == "" || endpoint.Port() == "" ||
		endpoint.Path != "" || endpoint.RawPath != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("proxy endpoint must contain only scheme, host, and port")
	}
	if _, err := strconv.ParseUint(endpoint.Port(), 10, 16); err != nil || endpoint.Port() == "0" {
		return nil, errors.New("proxy endpoint port is invalid")
	}
	if strings.Contains(endpoint.Hostname(), "%") {
		return nil, errors.New("proxy endpoint IPv6 zone identifiers are not allowed")
	}
	if options.Resolver == nil {
		options.Resolver = net.DefaultResolver
	}
	if options.DirectDialer == nil {
		options.DirectDialer = &net.Dialer{}
	}
	dialer := &HTTPConnectDialer{
		id: options.ID, endpoint: endpoint, endpointPolicy: options.EndpointPolicy,
		resolver: options.Resolver, directDialer: options.DirectDialer,
	}
	if options.BasicAuth != nil {
		if options.BasicAuth.Username == "" || len(options.BasicAuth.Username) > 1024 || strings.Contains(options.BasicAuth.Username, ":") {
			return nil, errors.New("proxy Basic auth username is invalid")
		}
		if len(options.BasicAuth.Password) == 0 || len(options.BasicAuth.Password) > 4096 {
			return nil, errors.New("proxy Basic auth password is invalid")
		}
		plain := make([]byte, 0, len(options.BasicAuth.Username)+1+len(options.BasicAuth.Password))
		plain = append(plain, options.BasicAuth.Username...)
		plain = append(plain, ':')
		plain = append(plain, options.BasicAuth.Password...)
		dialer.authHeader = make([]byte, base64.StdEncoding.EncodedLen(len(plain)))
		base64.StdEncoding.Encode(dialer.authHeader, plain)
		clear(plain)
	}
	return dialer, nil
}

func (d *HTTPConnectDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, d.failure(ProxyStageDial, 0, fmt.Errorf("%w: invalid target authority", ErrRefusedBeforeSend))
	}
	target, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil || !target.IsValid() {
		return nil, d.failure(ProxyStageDial, 0, fmt.Errorf("%w: CONNECT target must be a literal IP", ErrRefusedBeforeSend))
	}
	authority := net.JoinHostPort(target.Unmap().String(), port)

	d.mu.RLock()
	if d.closed {
		d.mu.RUnlock()
		return nil, d.failure(ProxyStageDial, 0, fmt.Errorf("%w: proxy connector is closed", ErrRefusedBeforeSend))
	}
	authHeader := append([]byte(nil), d.authHeader...)
	d.mu.RUnlock()
	defer clear(authHeader)

	proxyAddress, err := d.resolveProxy(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := d.directDialer.DialContext(ctx, network, proxyAddress)
	if err != nil {
		return nil, d.failure(ProxyStageDial, 0, errors.Join(ErrRefusedBeforeSend, err))
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = conn.Close()
		}
	}()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, d.failure(ProxyStageDial, 0, errors.Join(ErrRefusedBeforeSend, err))
		}
	}

	if d.endpoint.Scheme == "https" {
		proxyTLS := tls.Client(conn, &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: d.endpoint.Hostname(),
			NextProtos: []string{"http/1.1"},
		})
		if err := proxyTLS.HandshakeContext(ctx); err != nil {
			return nil, d.failure(ProxyStageTLS, 0, errors.Join(ErrRefusedBeforeSend, err))
		}
		if protocol := proxyTLS.ConnectionState().NegotiatedProtocol; protocol != "" && protocol != "http/1.1" {
			return nil, d.failure(ProxyStageTLS, 0, fmt.Errorf("%w: proxy negotiated an unsupported protocol", ErrRefusedBeforeSend))
		}
		conn = proxyTLS
	}

	request := make([]byte, 0, len(authority)*2+128+len(authHeader))
	request = append(request, "CONNECT "...)
	request = append(request, authority...)
	request = append(request, " HTTP/1.1\r\nHost: "...)
	request = append(request, authority...)
	request = append(request, "\r\n"...)
	if len(authHeader) > 0 {
		request = append(request, "Proxy-Authorization: Basic "...)
		request = append(request, authHeader...)
		request = append(request, "\r\n"...)
	}
	request = append(request, "Connection: keep-alive\r\n\r\n"...)
	if err := writeAll(conn, request); err != nil {
		clear(request)
		return nil, d.failure(ProxyStageWrite, 0, errors.Join(ErrRefusedBeforeSend, err))
	}
	clear(request)

	buffered, status, err := readConnectResponse(conn)
	if err != nil {
		return nil, d.failure(ProxyStageRead, 0, errors.Join(ErrRefusedBeforeSend, err))
	}
	if status < 200 || status > 299 {
		return nil, d.failure(ProxyStageStatus, status, ErrRefusedBeforeSend)
	}
	if err := conn.SetDeadline(noDeadline); err != nil {
		return nil, d.failure(ProxyStageDial, 0, errors.Join(ErrRefusedBeforeSend, err))
	}
	succeeded = true
	return buffered, nil
}

func (d *HTTPConnectDialer) resolveProxy(ctx context.Context) (string, error) {
	host := normalizeHost(d.endpoint.Hostname())
	var addresses []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{literal}
	} else {
		resolved, err := d.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return "", d.failure(ProxyStageResolve, 0, errors.Join(ErrRefusedBeforeSend, err))
		}
		addresses = resolved
	}
	if len(addresses) == 0 {
		return "", d.failure(ProxyStageResolve, 0, fmt.Errorf("%w: proxy resolved to no addresses", ErrRefusedBeforeSend))
	}
	for _, address := range addresses {
		if err := validateProxyAddress(address, d.endpointPolicy); err != nil {
			return "", d.failure(ProxyStageResolve, 0, errors.Join(ErrRefusedBeforeSend, err))
		}
	}
	return net.JoinHostPort(addresses[0].Unmap().String(), d.endpoint.Port()), nil
}

func (d *HTTPConnectDialer) failure(stage ProxyStage, status int, err error) error {
	return &ProxyError{ProxyID: d.id, Stage: stage, StatusCode: status, err: err}
}

func (d *HTTPConnectDialer) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	clear(d.authHeader)
	d.authHeader = nil
}

func validateProxyAddress(address netip.Addr, policy ProxyEndpointPolicy) error {
	address = address.Unmap()
	if address.IsLoopback() {
		if policy.AllowLoopback {
			return nil
		}
		return fmt.Errorf("proxy loopback address %s is not allowed", address)
	}
	return validateAddress(address, policy.AllowPrivate)
}

func readConnectResponse(conn net.Conn) (net.Conn, int, error) {
	limited := &io.LimitedReader{R: conn, N: maxConnectResponseHeaderBytes + 1}
	reader := bufio.NewReaderSize(limited, 4096)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		return nil, 0, err
	}
	if limited.N <= 0 {
		return nil, 0, errors.New("proxy response headers exceed the limit")
	}
	statusLine = strings.TrimSuffix(strings.TrimSuffix(statusLine, "\n"), "\r")
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 || parts[0] != "HTTP/1.1" {
		return nil, 0, errors.New("proxy response status line is invalid")
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil || status < 100 || status > 999 {
		return nil, 0, errors.New("proxy response status is invalid")
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, 0, err
		}
		if limited.N <= 0 {
			return nil, 0, errors.New("proxy response headers exceed the limit")
		}
		if line == "\r\n" {
			break
		}
		if !strings.HasSuffix(line, "\r\n") || !strings.Contains(line, ":") {
			return nil, 0, errors.New("proxy response header is invalid")
		}
	}
	return &bufferedConn{Conn: conn, reader: reader}, status, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(target []byte) (int, error) {
	if c.reader != nil && c.reader.Buffered() > 0 {
		return c.reader.Read(target)
	}
	return c.Conn.Read(target)
}

func writeAll(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		written, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrUnexpectedEOF
		}
		payload = payload[written:]
	}
	return nil
}
