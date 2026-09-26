package replication

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestCertificateChain(t *testing.T) (TLSFiles, *x509.Certificate) {
	t.Helper()
	directory := t.TempDir()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Halro test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "halro-1"}, DNSNames: []string{"halro-1.internal"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	writePEM := func(name, blockType string, contents []byte) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: contents}), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return TLSFiles{
		CAFile: writePEM("ca.pem", "CERTIFICATE", caDER), CertFile: writePEM("leaf.pem", "CERTIFICATE", leafDER),
		KeyFile: writePEM("leaf.key", "PRIVATE KEY", keyDER),
	}, leaf
}

func TestReplicationTLSConfigRequiresMutualTLS13(t *testing.T) {
	files, _ := writeTestCertificateChain(t)
	server, err := LoadServerTLSConfig(files)
	if err != nil {
		t.Fatal(err)
	}
	if server.MinVersion != tls.VersionTLS13 || server.MaxVersion != tls.VersionTLS13 || server.ClientAuth != tls.RequireAndVerifyClientCert || server.ClientCAs == nil || server.RootCAs != nil {
		t.Fatalf("server TLS config=%#v", server)
	}
	client, err := LoadClientTLSConfig(files, "halro-1.internal")
	if err != nil {
		t.Fatal(err)
	}
	if client.ServerName != "halro-1.internal" || client.RootCAs == nil || client.MinVersion != tls.VersionTLS13 || client.MaxVersion != tls.VersionTLS13 {
		t.Fatalf("client TLS config=%#v", client)
	}
	if _, err := LoadClientTLSConfig(files, ""); err == nil || !strings.Contains(err.Error(), "server name") {
		t.Fatalf("empty server-name error=%v", err)
	}
}

func TestReplicationTLSRefusesCAValidWrongSPKIPin(t *testing.T) {
	_, leaf := writeTestCertificateChain(t)
	state := tlsStateForCertificate(leaf)
	pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	if err := VerifyConnectionSPKI(state, digestText(pin)); err != nil {
		t.Fatal(err)
	}
	wrong := sha256.Sum256([]byte("different member"))
	if err := VerifyConnectionSPKI(state, digestText(wrong)); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("wrong pin error=%v", err)
	}
}

func TestReplicationTLSHandshakeProducesSharedTranscriptExporter(t *testing.T) {
	files, leaf := writeTestCertificateChain(t)
	serverConfig, err := LoadServerTLSConfig(files)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, err := LoadClientTLSConfig(files, "halro-1.internal")
	if err != nil {
		t.Fatal(err)
	}
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()
	client := tls.Client(clientSide, clientConfig)
	server := tls.Server(serverSide, serverConfig)
	serverResult := make(chan error, 1)
	go func() { serverResult <- server.Handshake() }()
	if err := client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	if err := VerifyConnectionSPKI(client.ConnectionState(), digestText(pin)); err != nil {
		t.Fatal(err)
	}
	if err := VerifyConnectionSPKI(server.ConnectionState(), digestText(pin)); err != nil {
		t.Fatal(err)
	}
	clientHello := testHello("halro-0", RolePrimary, 1)
	serverHello := testHello("halro-1", RoleReplica, 101)
	clientExporter, err := ExportHandshakeKey(client.ConnectionState(), clientHello, serverHello)
	if err != nil {
		t.Fatal(err)
	}
	serverExporter, err := ExportHandshakeKey(server.ConnectionState(), clientHello, serverHello)
	if err != nil {
		t.Fatal(err)
	}
	if string(clientExporter) != string(serverExporter) {
		t.Fatal("TLS endpoints derived different replication exporters")
	}
}

func tlsStateForCertificate(certificate *x509.Certificate) tls.ConnectionState {
	return tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{certificate}}}
}
