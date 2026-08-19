package app

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
)

// The listeners stopped pinning certificates to tls.Config.Certificates and now
// resolve them per handshake. net/http assembles HTTP/2 on both Serve and
// ServeTLS, so that move is not supposed to cost the protocol — but "supposed
// to" is how a silent downgrade to HTTP/1.1 survives a review. These tests dial
// the real listener and read back what was negotiated.

func TestServingListenerNegotiatesHTTP2(t *testing.T) {
	directory := t.TempDir()
	entry, _ := writeKeypair(t, directory, "serving", time.Now().Add(time.Hour), "halro.example.com")
	holder, err := newCertificateHolder([]config.TLSCertificate{entry}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: testConfig(t),
		reload: reloadRuntime{serving: holder},
	}
	runtime.config.TLS.Enabled = true

	address := serveForTest(t, runtime, "gateway")
	client := &http.Client{Transport: &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    poolFromCertificateFile(t, entry.CertFile),
			ServerName: "halro.example.com",
		},
	}}
	defer client.CloseIdleConnections()

	response, err := client.Get("https://" + address + "/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer response.Body.Close()
	if response.Proto != "HTTP/2.0" {
		t.Fatalf("the serving listener negotiated %s, not HTTP/2.0", response.Proto)
	}
}

func TestMetricsListenerNegotiatesHTTP2(t *testing.T) {
	directory := t.TempDir()
	caCertificate, caKey := createCertificate(t, nil, nil, true, "metrics-ca")
	serverCertificate, serverKey := createCertificate(t, caCertificate, caKey, false, "metrics-server")
	caPath := filepath.Join(directory, "ca.pem")
	certPath := filepath.Join(directory, "server.pem")
	keyPath := filepath.Join(directory, "server-key.pem")
	writePEM(t, caPath, "CERTIFICATE", caCertificate.Raw)
	writePEM(t, certPath, "CERTIFICATE", serverCertificate.Raw)
	keyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, keyPath, "PRIVATE KEY", keyDER)

	holder, err := newMetricsTLSHolder(config.MetricsTLS{
		Enabled: true, CertFile: certPath, KeyFile: keyPath, ClientCAFile: caPath,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		config: testConfig(t),
		reload: reloadRuntime{metricsTLS: holder},
	}

	address := serveForTest(t, runtime, "metrics")
	clientCertificate, clientKey := createClientCertificate(t, caCertificate, caKey)
	roots := x509.NewCertPool()
	roots.AddCert(caCertificate)
	client := &http.Client{Transport: &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			ServerName: "metrics-server",
			Certificates: []tls.Certificate{{
				Certificate: [][]byte{clientCertificate.Raw}, PrivateKey: clientKey, Leaf: clientCertificate,
			}},
		},
	}}
	defer client.CloseIdleConnections()

	response, err := client.Get("https://" + address + "/")
	if err != nil {
		t.Fatalf("mutually authenticated request failed: %v", err)
	}
	defer response.Body.Close()
	// GetConfigForClient returns a whole configuration for the connection, so
	// its NextProtos is the one that decides ALPN here — not the outer config
	// net/http added "h2" to.
	if response.Proto != "HTTP/2.0" {
		t.Fatalf("the Metrics listener negotiated %s, not HTTP/2.0", response.Proto)
	}
}

// serveForTest binds a real listener through the same server construction the
// runtime uses, so the TLS wiring under test is the wiring that ships.
func serveForTest(t *testing.T, runtime *Runtime, name string) string {
	t.Helper()
	server := runtime.server(name, "127.0.0.1:0", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	if server.TLSConfig == nil {
		t.Fatalf("listener %q was built without TLS", name)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if err := server.ServeTLS(listener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve %s: %v", name, err)
		}
	}()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String()
}

func poolFromCertificateFile(t *testing.T, path string) *x509.CertPool {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(payload) {
		t.Fatalf("%s contains no certificate", path)
	}
	return pool
}
