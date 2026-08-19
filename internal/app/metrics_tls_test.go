package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	halroconfig "github.com/akz142857/Halro/internal/config"
)

func TestMetricsTLSConfigRequiresAndVerifiesClientCertificates(t *testing.T) {
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

	holder, err := newMetricsTLSHolder(halroconfig.MetricsTLS{
		Enabled: true, CertFile: certPath, KeyFile: keyPath, ClientCAFile: caPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The listener installs a configuration that resolves per handshake, so the
	// policy assertions below read what a client would actually be met with.
	config := holder.serverConfig()
	published, err := config.GetConfigForClient(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if published.MinVersion != tls.VersionTLS12 || published.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("unexpected Metrics TLS policy: %#v", published)
	}
	if len(published.ClientCAs.Subjects()) != 1 {
		t.Fatalf("client CA subjects=%d", len(published.ClientCAs.Subjects()))
	}

	clientCertificate, clientKey := createClientCertificate(t, caCertificate, caKey)
	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(caCertificate)
	validClient := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    clientRoots,
		ServerName: "metrics-server",
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{clientCertificate.Raw}, PrivateKey: clientKey, Leaf: clientCertificate,
		}},
	}
	if err := runTLSHandshake(config, validClient); err != nil {
		t.Fatalf("valid mTLS handshake failed: %v", err)
	}
	missingClientIdentity := validClient.Clone()
	missingClientIdentity.Certificates = nil
	if err := runTLSHandshake(config, missingClientIdentity); err == nil {
		t.Fatal("mTLS handshake without a client certificate succeeded")
	}
	rogueCA, rogueKey := createCertificate(t, nil, nil, true, "rogue-ca")
	rogueClientCertificate, rogueClientKey := createClientCertificate(t, rogueCA, rogueKey)
	rogueClient := validClient.Clone()
	rogueClient.Certificates = []tls.Certificate{{
		Certificate: [][]byte{rogueClientCertificate.Raw}, PrivateKey: rogueClientKey, Leaf: rogueClientCertificate,
	}}
	if err := runTLSHandshake(config, rogueClient); err == nil {
		t.Fatal("mTLS handshake with an untrusted client CA succeeded")
	}
	expiredCertificate, expiredKey := createClientCertificateWithValidity(
		t, caCertificate, caKey, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour),
	)
	expiredClient := validClient.Clone()
	expiredClient.Certificates = []tls.Certificate{{
		Certificate: [][]byte{expiredCertificate.Raw}, PrivateKey: expiredKey, Leaf: expiredCertificate,
	}}
	if err := runTLSHandshake(config, expiredClient); err == nil {
		t.Fatal("mTLS handshake with an expired client certificate succeeded")
	}
}

func runTLSHandshake(serverConfig, clientConfig *tls.Config) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	serverResult := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverResult <- acceptErr
			return
		}
		defer connection.Close()
		tlsConnection := tls.Server(connection, serverConfig.Clone())
		_ = tlsConnection.SetDeadline(time.Now().Add(2 * time.Second))
		serverResult <- tlsConnection.Handshake()
	}()
	clientConnection, clientErr := tls.Dial("tcp", listener.Addr().String(), clientConfig)
	if clientErr == nil {
		clientErr = clientConnection.Close()
	}
	serverErr := <-serverResult
	return errors.Join(clientErr, serverErr)
}

func createClientCertificate(t *testing.T, issuer *x509.Certificate, issuerKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	return createClientCertificateWithValidity(t, issuer, issuerKey, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
}

func createClientCertificateWithValidity(t *testing.T, issuer *x509.Certificate, issuerKey *ecdsa.PrivateKey, notBefore, notAfter time.Time) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "prometheus"},
		NotBefore:    notBefore, NotAfter: notAfter,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

func createCertificate(
	t *testing.T,
	issuer *x509.Certificate,
	issuerKey *ecdsa.PrivateKey,
	isCA bool,
	commonName string,
) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: isCA, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	if isCA {
		template.KeyUsage |= x509.KeyUsageCertSign
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		template.DNSNames = []string{"metrics-server"}
	}
	if issuer == nil {
		issuer, issuerKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(file, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
