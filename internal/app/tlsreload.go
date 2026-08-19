package app

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/config"
)

// certificateExpiryWarning is how much life a certificate must have left before
// its remaining validity stops being worth mentioning. One threshold serves the
// reload log, `halro doctor`, and the alert rule shipped in deploy/observability,
// so the three cannot disagree about when an operator should have acted.
const certificateExpiryWarning = 30 * 24 * time.Hour

// unmatchedNameThrottle bounds how often an unmatched server name reaches the
// log. That name is chosen by whoever dialled the port, so an internet-facing
// listener under a scanner could otherwise push every record an operator needs
// out of a size-bounded file. Suppressed occurrences are counted and reported
// with the next record rather than being lost.
const unmatchedNameThrottle = time.Minute

// certificateSource is one configured keypair. The files are read again on every
// reload; the paths themselves are fixed for the life of the process, because
// changing where a certificate lives is a topology change rather than a material
// one and belongs to a restart.
type certificateSource struct {
	certFile string
	keyFile  string
}

// certificateBundle is everything one publication contains. It is built whole
// and replaced whole: a handshake reads exactly one pointer and then sees a set
// of certificates that were all loaded together.
type certificateBundle struct {
	byName   map[string]*tls.Certificate
	fallback *tls.Certificate
	// describe carries what observability needs without holding the certificates
	// themselves, so a metrics reader never touches private key material.
	describe []certificateDescription
}

type certificateDescription struct {
	// Name is taken from the certificate, not from the handshake. A label built
	// from ClientHelloInfo.ServerName would be attacker-controlled and unbounded.
	Name     string
	NotAfter time.Time
	// Fingerprint is a prefix of the leaf's SHA-256, which is what an operator
	// verifying a rotation reads back out of `openssl s_client`. It identifies
	// which file is in force; it discloses nothing, because the certificate it
	// summarizes is sent to every client that connects.
	Fingerprint string
}

// certificateHolder serves the current bundle and swaps it atomically. The read
// path is one atomic load plus a map lookup, which is less work than the slice
// scan crypto/tls does when certificates are pinned to Config.Certificates.
type certificateHolder struct {
	sources []certificateSource
	current atomic.Pointer[certificateBundle]
	logger  *slog.Logger
	// unmatchedLoggedUnixNano and unmatchedSuppressed implement the throttle
	// described above. Both are read from the handshake path, so neither may
	// take a lock.
	unmatchedLoggedUnixNano atomic.Int64
	unmatchedSuppressed     atomic.Uint64
}

func newCertificateHolder(entries []config.TLSCertificate, logger *slog.Logger) (*certificateHolder, error) {
	if len(entries) == 0 {
		return nil, errors.New("no TLS certificates configured")
	}
	holder := &certificateHolder{sources: make([]certificateSource, 0, len(entries)), logger: logger}
	for _, entry := range entries {
		holder.sources = append(holder.sources, certificateSource{certFile: entry.CertFile, keyFile: entry.KeyFile})
	}
	if err := holder.reload(); err != nil {
		return nil, err
	}
	return holder, nil
}

// reload replaces the bundle only when every source loaded. A partial swap would
// leave the process serving a mixture nobody configured, and the failure that
// produced it would be invisible from the outside.
func (h *certificateHolder) reload() error {
	bundle, err := buildCertificateBundle(h.sources)
	if err != nil {
		return err
	}
	h.current.Store(bundle)
	logCertificateLifetimes(h.logger, "serving", bundle.describe, time.Now())
	return nil
}

func (h *certificateHolder) describe() []certificateDescription {
	bundle := h.current.Load()
	if bundle == nil {
		return nil
	}
	return bundle.describe
}

// getCertificate answers a handshake. An unmatched server name is answered with
// the fallback rather than an error: crypto/tls cannot send unrecognized_name,
// so refusing here reaches the client as "tls: internal error" and tells them
// nothing, while the fallback makes their own name check report exactly which
// name was missing. Certificates are public, so handing over one that does not
// match discloses nothing the client could not already fetch.
func (h *certificateHolder) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	bundle := h.current.Load()
	if bundle == nil {
		return nil, errors.New("no TLS certificate is loaded")
	}
	name := normalizeServerName(hello.ServerName)
	if name == "" {
		return bundle.fallback, nil
	}
	if certificate, ok := bundle.byName[name]; ok {
		return certificate, nil
	}
	if wildcard, ok := wildcardForm(name); ok {
		if certificate, ok := bundle.byName[wildcard]; ok {
			return certificate, nil
		}
	}
	h.warnUnmatchedName(name)
	return bundle.fallback, nil
}

// warnUnmatchedName records that a client asked for a name nothing declares.
// The name goes to the log and never to a metric label: it is the one value
// here an outsider chooses.
func (h *certificateHolder) warnUnmatchedName(name string) {
	if h.logger == nil {
		return
	}
	now := time.Now().UnixNano()
	previous := h.unmatchedLoggedUnixNano.Load()
	// Two handshakes may pass the window check together; the compare-and-swap
	// decides which of them writes, and the loser is counted like any other
	// suppressed occurrence.
	if now-previous < int64(unmatchedNameThrottle) || !h.unmatchedLoggedUnixNano.CompareAndSwap(previous, now) {
		h.unmatchedSuppressed.Add(1)
		return
	}
	h.logger.Warn("no certificate declares the requested server name; answering with the first entry",
		"server_name", name, "suppressed_since_last", h.unmatchedSuppressed.Swap(0))
}

func buildCertificateBundle(sources []certificateSource) (*certificateBundle, error) {
	if len(sources) == 0 {
		return nil, errors.New("no TLS certificates configured")
	}
	bundle := &certificateBundle{
		byName:   make(map[string]*tls.Certificate, len(sources)),
		describe: make([]certificateDescription, 0, len(sources)),
	}
	for index, source := range sources {
		certificate, err := tls.LoadX509KeyPair(source.certFile, source.keyFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS certificate %d (%s): %w", index, source.certFile, err)
		}
		// LoadX509KeyPair has populated Leaf since Go 1.23, but a nil check costs
		// nothing and keeps the dereference below honest if that ever changes.
		leaf := certificate.Leaf
		if leaf == nil {
			parsed, parseErr := x509.ParseCertificate(certificate.Certificate[0])
			if parseErr != nil {
				return nil, fmt.Errorf("parse TLS certificate %d (%s): %w", index, source.certFile, parseErr)
			}
			leaf = parsed
			certificate.Leaf = parsed
		}
		names := certificateNames(leaf)
		if len(names) == 0 && index > 0 {
			// Only the first entry is reachable without a name. A later entry
			// declaring none can never be selected, so accepting it would mean
			// silently ignoring a certificate the operator meant to serve.
			return nil, fmt.Errorf("TLS certificate %d (%s) declares no DNS name and could never be selected", index, source.certFile)
		}
		// certificate is declared inside the loop, so each iteration addresses a
		// distinct value and the map does not end up with every name pointing at
		// the last keypair read.
		for _, name := range names {
			if _, exists := bundle.byName[name]; exists {
				return nil, fmt.Errorf("TLS certificate %d (%s) claims %q, which another certificate already claims", index, source.certFile, name)
			}
			bundle.byName[name] = &certificate
		}
		if index == 0 {
			bundle.fallback = &certificate
		}
		bundle.describe = append(bundle.describe, certificateDescription{
			Name: describeName(names, index), NotAfter: leaf.NotAfter,
			Fingerprint: certificateFingerprint(leaf),
		})
	}
	return bundle, nil
}

// certificateNames returns the lowercase DNS names a certificate serves.
// Subject CommonName is deliberately ignored: it has not been a valid source of
// identity for name matching in any current client.
func certificateNames(leaf *x509.Certificate) []string {
	names := make([]string, 0, len(leaf.DNSNames))
	seen := make(map[string]struct{}, len(leaf.DNSNames))
	for _, raw := range leaf.DNSNames {
		name := normalizeServerName(raw)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

func describeName(names []string, index int) string {
	if len(names) > 0 {
		return names[0]
	}
	return fmt.Sprintf("certificate-%d", index)
}

func normalizeServerName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// wildcardForm turns host.example.com into *.example.com. Wildcards match a
// single label, so example.com itself has no wildcard form.
func wildcardForm(name string) (string, bool) {
	index := strings.IndexByte(name, '.')
	if index < 0 || index+1 >= len(name) {
		return "", false
	}
	return "*" + name[index:], true
}

// metricsTLSHolder publishes the Metrics listener's server certificate and the
// client CA pool together. They are one decision — which scrapers may connect
// and as whom — so a reload that changed one without the other would create a
// combination the operator never wrote down.
type metricsTLSHolder struct {
	certFile     string
	keyFile      string
	clientCAFile string
	logger       *slog.Logger
	current      atomic.Pointer[tls.Config]
	description  atomic.Pointer[certificateDescription]
}

func newMetricsTLSHolder(cfg config.MetricsTLS, logger *slog.Logger) (*metricsTLSHolder, error) {
	holder := &metricsTLSHolder{
		certFile: cfg.CertFile, keyFile: cfg.KeyFile, clientCAFile: cfg.ClientCAFile, logger: logger,
	}
	if err := holder.reload(); err != nil {
		return nil, err
	}
	return holder, nil
}

func (h *metricsTLSHolder) reload() error {
	certificate, err := tls.LoadX509KeyPair(h.certFile, h.keyFile)
	if err != nil {
		return fmt.Errorf("load metrics TLS keypair: %w", err)
	}
	caPayload, err := os.ReadFile(h.clientCAFile)
	if err != nil {
		return fmt.Errorf("read metrics client CA: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPayload) {
		return errors.New("metrics client CA contains no certificates")
	}
	leaf := certificate.Leaf
	if leaf == nil {
		leaf, err = x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			return fmt.Errorf("parse metrics TLS certificate: %w", err)
		}
		certificate.Leaf = leaf
	}
	h.current.Store(&tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		NextProtos:   []string{"h2", "http/1.1"},
	})
	description := certificateDescription{
		Name: describeName(certificateNames(leaf), 0), NotAfter: leaf.NotAfter,
		Fingerprint: certificateFingerprint(leaf),
	}
	h.description.Store(&description)
	logCertificateLifetimes(h.logger, "metrics", []certificateDescription{description}, time.Now())
	return nil
}

// serverConfig is what the listener installs. Its GetConfigForClient is read
// once per handshake, so a connection completes against a single published
// configuration and an in-flight scrape is never handed half of a rotation.
func (h *metricsTLSHolder) serverConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return h.current.Load(), nil
		},
	}
}

func (h *metricsTLSHolder) describe() []certificateDescription {
	description := h.description.Load()
	if description == nil {
		return nil
	}
	return []certificateDescription{*description}
}

// certificateFingerprint is the leading bytes of the leaf's SHA-256, rendered
// the way `openssl x509 -fingerprint` renders the whole digest. A prefix is
// enough to tell two files apart during a rotation and short enough to read
// out of a log line.
func certificateFingerprint(leaf *x509.Certificate) string {
	sum := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(sum[:8])
}

// logCertificateLifetimes reports what was just published. An expired
// certificate is still accepted — refusing it would take away the one mechanism
// an operator has to replace material without a restart, and whether it is
// usable is the client's decision to make — but it is never accepted silently,
// because the reload would otherwise look like it worked.
//
// Only the name, the expiry, and a digest prefix are written. Private key bytes
// are never logged, and neither is the file path's content.
func logCertificateLifetimes(logger *slog.Logger, scope string, descriptions []certificateDescription, now time.Time) {
	if logger == nil {
		return
	}
	for _, description := range descriptions {
		attributes := []any{
			"scope", scope, "name", description.Name,
			"not_after", description.NotAfter.UTC().Format(time.RFC3339),
			"fingerprint", description.Fingerprint,
		}
		remaining := description.NotAfter.Sub(now)
		switch {
		case remaining <= 0:
			logger.Error("TLS certificate is expired and is being served anyway", attributes...)
		case remaining < certificateExpiryWarning:
			logger.Warn("TLS certificate is close to expiry",
				append(attributes, "days_left", int(remaining.Hours()/24))...)
		default:
			logger.Info("TLS certificate loaded", attributes...)
		}
	}
}
