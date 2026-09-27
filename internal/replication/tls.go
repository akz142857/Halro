package replication

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

const TLSExporterLabel = "EXPORTER-Halro-Replication-v1"

type TLSFiles struct {
	CAFile   string
	CertFile string
	KeyFile  string
}

// LoadServerTLSConfig requires a CA-verified client certificate and TLS 1.3.
// The static member SPKI pin is checked after the hello identifies the peer.
func LoadServerTLSConfig(files TLSFiles) (*tls.Config, error) {
	config, err := loadTLSConfig(files)
	if err != nil {
		return nil, err
	}
	config.ClientCAs = config.RootCAs
	config.RootCAs = nil
	config.ClientAuth = tls.RequireAndVerifyClientCert
	return config, nil
}

// LoadClientTLSConfig verifies the server certificate name and CA chain. The
// peer's static SPKI pin remains an additional mandatory post-handshake check.
func LoadClientTLSConfig(files TLSFiles, serverName string) (*tls.Config, error) {
	if strings.TrimSpace(serverName) == "" {
		return nil, errors.New("replication TLS server name is required")
	}
	config, err := loadTLSConfig(files)
	if err != nil {
		return nil, err
	}
	config.ServerName = serverName
	return config, nil
}

func loadTLSConfig(files TLSFiles) (*tls.Config, error) {
	if files.CAFile == "" || files.CertFile == "" || files.KeyFile == "" {
		return nil, errors.New("replication TLS CA, certificate, and key files are required")
	}
	caPEM, err := os.ReadFile(files.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read replication CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("replication CA file contains no certificate")
	}
	certificate, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load replication certificate and key: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		RootCAs:      pool,
	}, nil
}

// VerifyConnectionSPKI is deliberately separate from CA verification: the CA
// establishes the cluster trust domain, while the configured pin binds the
// connection to the exact static member selected by the authenticated hello.
func VerifyConnectionSPKI(state tls.ConnectionState, expected string) error {
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return errors.New("replication TLS connection has no verified peer chain")
	}
	want, err := parseDigestText(expected)
	if err != nil {
		return fmt.Errorf("replication peer SPKI pin: %w", err)
	}
	got := sha256.Sum256(state.VerifiedChains[0][0].RawSubjectPublicKeyInfo)
	if got != want {
		return fmt.Errorf("replication peer SPKI pin mismatch: got sha256:%s", hex.EncodeToString(got[:]))
	}
	return nil
}

func ExportHandshakeKey(state tls.ConnectionState, client, server Hello) ([]byte, error) {
	if state.Version != tls.VersionTLS13 || !state.HandshakeComplete {
		return nil, errors.New("handshake exporter requires a completed TLS 1.3 connection")
	}
	context, err := HandshakeTranscriptHash(client, server)
	if err != nil {
		return nil, err
	}
	exporter, err := state.ExportKeyingMaterial(TLSExporterLabel, context[:], sha256.Size)
	if err != nil {
		return nil, fmt.Errorf("export replication handshake key: %w", err)
	}
	return exporter, nil
}
