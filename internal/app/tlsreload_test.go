package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/config"
)

// writeKeypair writes a self-signed certificate for the given names and returns
// the two paths plus the fingerprint a handshake would expose.
func writeKeypair(t *testing.T, directory, base string, notAfter time.Time, names ...string) (config.TLSCertificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	subject := base
	if len(names) > 0 {
		subject = names[0]
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: subject},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              notAfter,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              names,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	entry := config.TLSCertificate{
		CertFile: filepath.Join(directory, base+".pem"),
		KeyFile:  filepath.Join(directory, base+"-key.pem"),
	}
	writePEMFile(t, entry.CertFile, "CERTIFICATE", der)
	writePEMFile(t, entry.KeyFile, "PRIVATE KEY", keyDER)
	sum := sha256.Sum256(der)
	return entry, hex.EncodeToString(sum[:])
}

// writePEMFile truncates rather than requiring exclusivity: reload tests
// deliberately write over a path the process is already serving from.
func writePEMFile(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	payload := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func servedFingerprint(t *testing.T, holder *certificateHolder, serverName string) string {
	t.Helper()
	certificate, err := holder.getCertificate(&tls.ClientHelloInfo{ServerName: serverName})
	if err != nil {
		t.Fatalf("server name %q was refused: %v", serverName, err)
	}
	if certificate == nil {
		t.Fatalf("server name %q produced no certificate", serverName)
	}
	sum := sha256.Sum256(certificate.Certificate[0])
	return hex.EncodeToString(sum[:])
}

func TestCertificateHolderSelectsBySNI(t *testing.T) {
	directory := t.TempDir()
	expiry := time.Now().Add(time.Hour)
	first, firstPrint := writeKeypair(t, directory, "api", expiry, "api.example.com")
	second, secondPrint := writeKeypair(t, directory, "console", expiry, "console.example.com")
	third, thirdPrint := writeKeypair(t, directory, "apps", expiry, "*.apps.example.com")

	holder, err := newCertificateHolder([]config.TLSCertificate{first, second, third}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"api.example.com":      firstPrint,
		"API.EXAMPLE.COM":      firstPrint,
		"api.example.com.":     firstPrint,
		"console.example.com":  secondPrint,
		"one.apps.example.com": thirdPrint,
		// No SNI is what a probe dialling the address rather than the name
		// sends, and it must be answered rather than refused.
		"": firstPrint,
		// An unmatched name deliberately gets the fallback so the client's own
		// name verification reports which name was missing.
		"unknown.example.com": firstPrint,
		// Wildcards match one label only.
		"deep.one.apps.example.com": firstPrint,
	}
	for serverName, want := range cases {
		if got := servedFingerprint(t, holder, serverName); got != want {
			t.Fatalf("server name %q served the wrong certificate", serverName)
		}
	}
}

func TestCertificateHolderReloadReplacesTheServedCertificate(t *testing.T) {
	directory := t.TempDir()
	expiry := time.Now().Add(time.Hour)
	entry, firstPrint := writeKeypair(t, directory, "serving", expiry, "halro.example.com")
	holder, err := newCertificateHolder([]config.TLSCertificate{entry}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := servedFingerprint(t, holder, "halro.example.com"); got != firstPrint {
		t.Fatal("the initial certificate was not served")
	}
	_, secondPrint := writeKeypair(t, directory, "serving", expiry, "halro.example.com")
	if secondPrint == firstPrint {
		t.Fatal("the replacement certificate is identical; the test proves nothing")
	}
	if err := holder.reload(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if got := servedFingerprint(t, holder, "halro.example.com"); got != secondPrint {
		t.Fatal("reload did not replace the served certificate")
	}
}

func TestCertificateHolderKeepsTheOldBundleWhenReloadFails(t *testing.T) {
	directory := t.TempDir()
	expiry := time.Now().Add(time.Hour)
	first, firstPrint := writeKeypair(t, directory, "a", expiry, "a.example.com")
	second, _ := writeKeypair(t, directory, "b", expiry, "b.example.com")
	holder, err := newCertificateHolder([]config.TLSCertificate{first, second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt only the second entry. A partial publication would leave the
	// process serving a mixture nobody configured, so the whole bundle must be
	// held back.
	if err := os.WriteFile(second.KeyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := holder.reload(); err == nil {
		t.Fatal("a broken keypair was accepted")
	}
	if got := servedFingerprint(t, holder, "a.example.com"); got != firstPrint {
		t.Fatal("a failed reload disturbed the certificate that was already serving")
	}
	if got := servedFingerprint(t, holder, "b.example.com"); got == firstPrint {
		t.Fatal("the second name lost its certificate after a failed reload")
	}
}

func TestCertificateHolderRefusesAmbiguousAndUnreachableEntries(t *testing.T) {
	directory := t.TempDir()
	expiry := time.Now().Add(time.Hour)
	first, _ := writeKeypair(t, directory, "first", expiry, "halro.example.com")
	duplicate, _ := writeKeypair(t, directory, "duplicate", expiry, "halro.example.com")
	if _, err := newCertificateHolder([]config.TLSCertificate{first, duplicate}, nil); err == nil {
		t.Fatal("two certificates claiming the same name were accepted")
	}

	anonymous, _ := writeKeypair(t, directory, "anonymous", expiry)
	if _, err := newCertificateHolder([]config.TLSCertificate{first, anonymous}, nil); err == nil {
		t.Fatal("a later certificate with no DNS name was accepted despite being unselectable")
	}
	// The same certificate is fine as the first entry: that slot is reached
	// without a name.
	if _, err := newCertificateHolder([]config.TLSCertificate{anonymous}, nil); err != nil {
		t.Fatalf("a single nameless certificate was refused as the fallback: %v", err)
	}
}

func TestCertificateHolderDescribesExpiryFromTheCertificate(t *testing.T) {
	directory := t.TempDir()
	expiry := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	entry, _ := writeKeypair(t, directory, "serving", expiry, "halro.example.com")
	holder, err := newCertificateHolder([]config.TLSCertificate{entry}, nil)
	if err != nil {
		t.Fatal(err)
	}
	descriptions := holder.describe()
	if len(descriptions) != 1 {
		t.Fatalf("descriptions=%d", len(descriptions))
	}
	if descriptions[0].Name != "halro.example.com" {
		t.Fatalf("description name=%q", descriptions[0].Name)
	}
	if !descriptions[0].NotAfter.Equal(expiry.UTC()) {
		t.Fatalf("description expiry=%s want %s", descriptions[0].NotAfter, expiry.UTC())
	}
}

// TestMetricsTLSHolderRotatesCertificateAndClientCATogether walks the two-phase
// rotation an operator has to perform: publish a CA bundle trusting both old and
// new, move the scraper, then drop the old CA. The point being proven is that
// each SIGHUP publishes one coherent pair, so a scraper is never met with a
// server certificate from one rotation and a CA pool from another.
func TestMetricsTLSHolderRotatesCertificateAndClientCATogether(t *testing.T) {
	directory := t.TempDir()
	oldCA, oldCAKey := createCertificate(t, nil, nil, true, "old-ca")
	newCA, newCAKey := createCertificate(t, nil, nil, true, "new-ca")
	serverCertificate, serverKey := createCertificate(t, oldCA, oldCAKey, false, "metrics-server")

	caPath := filepath.Join(directory, "clients-ca.pem")
	certPath := filepath.Join(directory, "server.pem")
	keyPath := filepath.Join(directory, "server-key.pem")
	writePEMFile(t, certPath, "CERTIFICATE", serverCertificate.Raw)
	serverKeyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	writePEMFile(t, keyPath, "PRIVATE KEY", serverKeyDER)
	writePEMFile(t, caPath, "CERTIFICATE", oldCA.Raw)

	holder, err := newMetricsTLSHolder(config.MetricsTLS{
		Enabled: true, CertFile: certPath, KeyFile: keyPath, ClientCAFile: caPath,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := holder.serverConfig()

	oldClient := metricsClient(t, oldCA, oldCAKey, serverCertificate)
	newClient := metricsClient(t, newCA, newCAKey, serverCertificate)

	if err := runTLSHandshake(serverConfig, oldClient); err != nil {
		t.Fatalf("the existing scraper was refused before any rotation: %v", err)
	}
	if err := runTLSHandshake(serverConfig, newClient); err == nil {
		t.Fatal("a client from an untrusted CA was accepted before that CA was published")
	}

	// Phase one: both CAs trusted. Neither scraper is interrupted.
	bothCAs := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: oldCA.Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: newCA.Raw})...,
	)
	if err := os.WriteFile(caPath, bothCAs, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := holder.reload(); err != nil {
		t.Fatalf("reload with both CAs failed: %v", err)
	}
	if err := runTLSHandshake(serverConfig, oldClient); err != nil {
		t.Fatalf("the existing scraper was interrupted by phase one: %v", err)
	}
	if err := runTLSHandshake(serverConfig, newClient); err != nil {
		t.Fatalf("the new scraper was refused after its CA was published: %v", err)
	}

	// Phase two: the old CA is withdrawn.
	writePEMFile(t, caPath, "CERTIFICATE", newCA.Raw)
	if err := holder.reload(); err != nil {
		t.Fatalf("reload with only the new CA failed: %v", err)
	}
	if err := runTLSHandshake(serverConfig, oldClient); err == nil {
		t.Fatal("a client from the withdrawn CA was still accepted")
	}
	if err := runTLSHandshake(serverConfig, newClient); err != nil {
		t.Fatalf("the new scraper was refused after the old CA was withdrawn: %v", err)
	}
}

func metricsClient(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, serverCertificate *x509.Certificate) *tls.Config {
	t.Helper()
	clientCertificate, clientKey := createClientCertificate(t, ca, caKey)
	roots := x509.NewCertPool()
	roots.AddCert(serverCertificate)
	// The server certificate is self-issued from the old CA in this test, so the
	// client trusts it directly; what is under test is the server's client-CA
	// decision, not the client's server-CA decision.
	roots.AddCert(ca)
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: "metrics-server",
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{clientCertificate.Raw}, PrivateKey: clientKey, Leaf: clientCertificate,
		}},
	}
}

func TestChangedOutsideReloadableIgnoresTheLogLevel(t *testing.T) {
	current := config.Default()
	next := config.Default()
	next.Logging.Level = "debug"
	if changed := changedOutsideReloadable(current, next); len(changed) != 0 {
		t.Fatalf("a log-level change was reported as unreloadable: %v", changed)
	}
	next.Server.GatewayListen = "127.0.0.1:19090"
	next.Admin.ExternalOrigin = "https://halro.example.com"
	changed := changedOutsideReloadable(current, next)
	if len(changed) != 2 || !strings.Contains(strings.Join(changed, ","), "server") ||
		!strings.Contains(strings.Join(changed, ","), "admin") {
		t.Fatalf("unreloadable sections were not reported: %v", changed)
	}
}

// TestMetricsListenerFallsBackToTheServerCertificate pins a regression found by
// running the real binary: with tls enabled and metrics.tls disabled, the
// Metrics listener must still serve TLS using the ordinary server certificate.
// Serving it in the clear there would contradict both the listener validation
// that allowed the address and `halro stats`, which addresses /metrics as https
// whenever tls.enabled is set.
func TestMetricsListenerFallsBackToTheServerCertificate(t *testing.T) {
	directory := t.TempDir()
	entry, print := writeKeypair(t, directory, "serving", time.Now().Add(time.Hour), "halro.example.com")
	holder, err := newCertificateHolder([]config.TLSCertificate{entry}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{reload: reloadRuntime{serving: holder}}

	metricsConfig := runtime.listenerTLSConfig("metrics")
	if metricsConfig == nil {
		t.Fatal("the Metrics listener was left in the clear while TLS was enabled")
	}
	certificate, err := metricsConfig.GetCertificate(&tls.ClientHelloInfo{ServerName: "halro.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(certificate.Certificate[0])
	if hex.EncodeToString(sum[:]) != print {
		t.Fatal("the Metrics listener served a certificate other than the configured one")
	}

	// A plaintext instance still gets no TLS configuration, which is what the
	// listener startup reads to choose Serve over ServeTLS.
	plaintext := &Runtime{}
	for _, name := range []string{"gateway", "admin", "metrics"} {
		if plaintext.listenerTLSConfig(name) != nil {
			t.Fatalf("listener %q was given TLS with nothing configured", name)
		}
	}
}

// TestCertificateHolderIsSafeUnderConcurrentHandshakes exercises the claim the
// whole design rests on: a reload publishes one pointer, and a handshake reads
// one pointer. Run under -race, a torn publication or an unsynchronised read
// shows up here rather than in production at rotation time.
func TestCertificateHolderIsSafeUnderConcurrentHandshakes(t *testing.T) {
	directory := t.TempDir()
	expiry := time.Now().Add(time.Hour)
	entry, _ := writeKeypair(t, directory, "serving", expiry, "halro.example.com")
	// A real logger, because the unmatched-name path below carries its own
	// throttle state and a nil logger would return before ever touching it.
	holder, err := newCertificateHolder([]config.TLSCertificate{entry},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, name := range []string{"halro.example.com", "", "unknown.example.com"} {
					certificate, err := holder.getCertificate(&tls.ClientHelloInfo{ServerName: name})
					if err != nil || certificate == nil || len(certificate.Certificate) == 0 {
						t.Errorf("handshake for %q got certificate=%v err=%v", name, certificate, err)
						return
					}
				}
			}
		}()
	}
	for range 20 {
		writeKeypair(t, directory, "serving", expiry, "halro.example.com")
		if err := holder.reload(); err != nil {
			t.Errorf("reload failed: %v", err)
			break
		}
	}
	close(stop)
	readers.Wait()
}

// TestCertificateHolderWarnsOnceForAnUnmatchedName covers the diagnostic that
// makes the fallback answer readable — an operator whose client reports a name
// mismatch needs the server side to agree — and the bound that keeps it from
// becoming a denial of service. The name is chosen by whoever dialled the port.
func TestCertificateHolderWarnsOnceForAnUnmatchedName(t *testing.T) {
	directory := t.TempDir()
	entry, _ := writeKeypair(t, directory, "api", time.Now().Add(time.Hour), "api.example.com")
	var buffer safeBuffer
	holder, err := newCertificateHolder([]config.TLSCertificate{entry},
		slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := holder.getCertificate(&tls.ClientHelloInfo{ServerName: "scan-target.example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	// A declared name and a connection with no SNI at all are both answered
	// exactly, so neither may produce the warning.
	if _, err := holder.getCertificate(&tls.ClientHelloInfo{ServerName: "api.example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.getCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Fatal(err)
	}
	records := strings.Count(buffer.String(), "no certificate declares")
	if records != 1 {
		t.Fatalf("five unmatched handshakes produced %d records, want 1: %q", records, buffer.String())
	}
	if !strings.Contains(buffer.String(), "server_name=scan-target.example.com") {
		t.Fatalf("the record does not name what was asked for: %q", buffer.String())
	}

	// Simulate the throttle window elapsing. The occurrences held back in the
	// meantime must be reported rather than dropped, or the log would understate
	// a scan as a single stray connection.
	holder.unmatchedLoggedUnixNano.Store(0)
	if _, err := holder.getCertificate(&tls.ClientHelloInfo{ServerName: "scan-target.example.com"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buffer.String(), "suppressed_since_last=4") {
		t.Fatalf("the suppressed occurrences were not carried forward: %q", buffer.String())
	}
}

// TestCertificateLifetimesAreReportedWhenPublished pins the failure semantics:
// an expired keypair is still served — refusing it would remove the one way to
// replace material without a restart — but it never publishes quietly.
func TestCertificateLifetimesAreReportedWhenPublished(t *testing.T) {
	cases := []struct {
		name     string
		notAfter time.Time
		want     string
	}{
		{"expired", time.Now().Add(-time.Hour), "level=ERROR"},
		{"close to expiry", time.Now().Add(10 * 24 * time.Hour), "level=WARN"},
		{"healthy", time.Now().Add(90 * 24 * time.Hour), "level=INFO"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			directory := t.TempDir()
			entry, fingerprint := writeKeypair(t, directory, "api", testCase.notAfter, "api.example.com")
			var buffer safeBuffer
			holder, err := newCertificateHolder([]config.TLSCertificate{entry},
				slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
			if err != nil {
				t.Fatalf("an expiring certificate must still load: %v", err)
			}
			if got := servedFingerprint(t, holder, "api.example.com"); got != fingerprint {
				t.Fatal("the certificate was not published")
			}
			if !strings.Contains(buffer.String(), testCase.want) {
				t.Fatalf("expected a %s record, got %q", testCase.want, buffer.String())
			}
			if !strings.Contains(buffer.String(), "name=api.example.com") {
				t.Fatalf("the record does not identify the certificate: %q", buffer.String())
			}
			// The digest prefix is what an operator compares against
			// `openssl s_client` to confirm which file is in force.
			if !strings.Contains(buffer.String(), "fingerprint="+fingerprint[:16]) {
				t.Fatalf("the record does not carry the fingerprint: %q", buffer.String())
			}
		})
	}
}
