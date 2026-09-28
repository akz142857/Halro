package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemotePrometheusRequiresAuthenticatedHTTPS(t *testing.T) {
	for _, tc := range []struct {
		address string
		cfg     config
		want    string
	}{
		{"http://prometheus.internal:9090", config{}, "requires HTTPS"},
		{"https://prometheus.internal:9090", config{}, "client certificate/key"},
		{"https://prometheus.internal:9090", config{prometheusCA: "ca", prometheusClientCert: "cert"}, "client certificate/key"},
		{"http://127.0.0.1:9090", config{prometheusCA: "ca"}, "require a remote HTTPS"},
	} {
		base, err := url.Parse(tc.address)
		if err != nil {
			t.Fatal(err)
		}
		_, err = newPrometheusClient(tc.cfg, base, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("address=%s error=%v want=%q", tc.address, err, tc.want)
		}
	}
}

func TestRemotePrometheusUsesPinnedCAAndClientCertificate(t *testing.T) {
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Prometheus test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(serial int64, usage x509.ExtKeyUsage) ([]byte, []byte) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "Prometheus test"},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{usage}, DNSNames: []string{"prometheus.internal"}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		privateDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	}
	serverPEM, serverKey := issue(2, x509.ExtKeyUsageServerAuth)
	clientPEM, clientKey := issue(3, x509.ExtKeyUsageClientAuth)
	serverCertificate, err := tls.X509KeyPair(serverPEM, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" || r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{serverCertificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
	server.StartTLS()
	defer server.Close()
	directory := t.TempDir()
	write := func(name string, bytes []byte) string {
		t.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, bytes, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	caPath := write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	certPath := write("client.pem", clientPEM)
	keyPath := write("client.key", clientKey)
	base, err := url.Parse("https://prometheus.internal:9090")
	if err != nil {
		t.Fatal(err)
	}
	client, err := newPrometheusClient(config{prometheusCA: caPath, prometheusClientCert: certPath, prometheusClientKey: keyPath}, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	response, err := client.Get(base.String() + "/api/v1/query")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	withoutCertificate := client.Transport.(*http.Transport).Clone()
	withoutCertificate.TLSClientConfig = withoutCertificate.TLSClientConfig.Clone()
	withoutCertificate.TLSClientConfig.Certificates = nil
	withoutCertificateClient := &http.Client{Transport: withoutCertificate, Timeout: time.Second}
	if response, err := withoutCertificateClient.Get(base.String() + "/api/v1/query"); err == nil {
		response.Body.Close()
		t.Fatal("Prometheus accepted a client without its certificate")
	}
	wrongCAKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongCADER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(4), Subject: pkix.Name{CommonName: "wrong CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}, &x509.Certificate{
		SerialNumber: big.NewInt(4), Subject: pkix.Name{CommonName: "wrong CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}, &wrongCAKey.PublicKey, wrongCAKey)
	if err != nil {
		t.Fatal(err)
	}
	wrongCAPath := write("wrong-ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: wrongCADER}))
	wrongCAClient, err := newPrometheusClient(config{prometheusCA: wrongCAPath, prometheusClientCert: certPath, prometheusClientKey: keyPath}, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrongCAClient.Transport.(*http.Transport).DialContext = transport.DialContext
	if response, err := wrongCAClient.Get(base.String() + "/api/v1/query"); err == nil {
		response.Body.Close()
		t.Fatal("Prometheus server with an untrusted certificate was accepted")
	}
	if err := os.Chmod(keyPath, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := newPrometheusClient(config{prometheusCA: caPath, prometheusClientCert: certPath, prometheusClientKey: keyPath}, base, nil); err == nil || !strings.Contains(err.Error(), "private regular file") {
		t.Fatalf("world-readable key accepted: %v", err)
	}
}
