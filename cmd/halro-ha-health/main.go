// halro-ha-health serves a read-only HA view from independently scraped metrics.
// It must run outside the Primary member's failure domain.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/buildinfo"
	halroconfig "github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/hahealth"
	"github.com/akz142857/Halro/internal/hostsecurity"
)

//go:embed ui/index.html ui/app.js
var ui embed.FS

type config struct {
	listen, prometheus, environment, cluster, members, cert, key, clientCA, clientURL, probeCA string
	prometheusCA, prometheusClientCert, prometheusClientKey                                    string
	statusManifest                                                                             string
	eventJournal                                                                               string
	durableTransitionJournal                                                                   string
	verifyDurableSnapshot                                                                      string
	verifyArchiveReadback                                                                      string
	compareMemberSnapshotReports                                                               string
	verifyMemberSnapshotManifest                                                               string
	preflightReseedHandoff                                                                     string
	commitReseedHandoff                                                                        string
	reconcileRetiredMemberSnapshot, retiredMemberConfig                                        string
	clientFinalManifest                                                                        string
	runbookBase                                                                                string
}

type prometheusResult struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string   `json:"metric"`
			Value  []json.RawMessage   `json:"value"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

type currentAlert struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Severity string `json:"severity"`
	Instance string `json:"instance,omitempty"`
	Started  string `json:"started"`
	Summary  string `json:"summary"`
	Runbook  string `json:"runbook"`
	Value    string `json:"value,omitempty"`
}

type alertRuleEvidence struct {
	Status         string  `json:"status"`
	Query          string  `json:"query,omitempty"`
	ForSeconds     float64 `json:"for_seconds"`
	Health         string  `json:"health,omitempty"`
	LastEvaluation string  `json:"last_evaluation,omitempty"`
}

type confirmationEvidence struct {
	Status         string    `json:"status"`
	Source         string    `json:"source"`
	Instance       string    `json:"instance,omitempty"`
	Increase5m     *float64  `json:"increase_5m,omitempty"`
	EvaluatedAt    time.Time `json:"evaluated_at,omitempty"`
	SampledAt      time.Time `json:"sampled_at,omitempty"`
	EpochSampledAt time.Time `json:"epoch_sampled_at,omitempty"`
}

type server struct {
	client               *http.Client
	probeHTTP            *http.Client
	base                 *url.URL
	environment, cluster string
	members              []string
	clientURL            string
	runbookBase          string
	statusCollectors     map[string]statusCollector
	archive              *eventArchive
	durableArchive       *durableArchive
	clientFinal          *clientFinalManifest
}

type auditWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

func main() {
	var cfg config
	var version bool
	flag.BoolVar(&version, "version", false, "print build identity and exit")
	flag.StringVar(&cfg.listen, "listen", "127.0.0.1:9105", "HTTPS listener for an authenticated operator proxy")
	flag.StringVar(&cfg.prometheus, "prometheus", "http://127.0.0.1:9091", "loopback Prometheus URL or an explicitly authenticated HTTPS Prometheus service")
	flag.StringVar(&cfg.prometheusCA, "prometheus-ca", "", "CA for a remote HTTPS Prometheus service")
	flag.StringVar(&cfg.prometheusClientCert, "prometheus-client-cert", "", "client certificate for a remote HTTPS Prometheus service")
	flag.StringVar(&cfg.prometheusClientKey, "prometheus-client-key", "", "private client key for a remote HTTPS Prometheus service")
	flag.StringVar(&cfg.environment, "environment", "", "Prometheus environment label")
	flag.StringVar(&cfg.cluster, "cluster", "", "Prometheus cluster label")
	flag.StringVar(&cfg.members, "members", "", "comma-separated expected instance labels")
	flag.StringVar(&cfg.cert, "tls-cert", "", "server certificate path")
	flag.StringVar(&cfg.key, "tls-key", "", "server private key path")
	flag.StringVar(&cfg.clientCA, "client-ca", "", "CA for operator proxy client certificates")
	flag.StringVar(&cfg.clientURL, "client-url", "", "optional HTTPS client Service root URL for non-billable routing probe")
	flag.StringVar(&cfg.probeCA, "probe-ca", "", "optional CA for the HTTPS client Service probe")
	flag.StringVar(&cfg.statusManifest, "member-status-manifest", "", "optional JSON inventory for machine-only member status reads")
	flag.StringVar(&cfg.eventJournal, "event-journal", "", "optional private durable file for independently polled member events")
	flag.StringVar(&cfg.durableTransitionJournal, "durable-transition-journal", "", "optional private file for verified member transition pages")
	flag.StringVar(&cfg.verifyDurableSnapshot, "verify-durable-snapshot", "", "verify a frozen collector manifest and its closed segments, then print a file-hash inventory")
	flag.StringVar(&cfg.verifyArchiveReadback, "verify-archive-readback", "", "separately re-verify a retrieved frozen collector copy against -verify-durable-snapshot; local byte equality only")
	flag.StringVar(&cfg.compareMemberSnapshotReports, "compare-member-snapshot-reports", "", "private manifest of already verified member snapshot reports to compare with the frozen collector head")
	flag.StringVar(&cfg.verifyMemberSnapshotManifest, "verify-member-snapshot-manifest", "", "private manifest of frozen member data directories and matching configs to authenticate and compare directly")
	flag.StringVar(&cfg.preflightReseedHandoff, "preflight-reseed-handoff", "", "private manifest of frozen collector, retired member, seed source and replacement snapshots to verify before a journal-generation handoff")
	flag.StringVar(&cfg.commitReseedHandoff, "commit-reseed-handoff", "", "private preflight manifest to commit an authenticated new member generation into a stopped collector")
	flag.StringVar(&cfg.reconcileRetiredMemberSnapshot, "reconcile-retired-member-snapshot", "", "frozen retired member directory whose authenticated old journal tail must be imported into a stopped collector")
	flag.StringVar(&cfg.retiredMemberConfig, "retired-member-config", "", "configuration and Master Key for the frozen retired member copy")
	flag.StringVar(&cfg.clientFinalManifest, "client-final-manifest", "", "optional accepted client-final observer inventory")
	flag.StringVar(&cfg.runbookBase, "runbook-base-url", "", "optional HTTPS documentation root serving /docs runbook paths")
	flag.Parse()
	if version {
		_ = json.NewEncoder(os.Stdout).Encode(buildinfo.Current())
		return
	}
	if cfg.commitReseedHandoff != "" {
		if cfg.preflightReseedHandoff != "" || cfg.verifyDurableSnapshot != "" || cfg.compareMemberSnapshotReports != "" ||
			cfg.verifyArchiveReadback != "" || cfg.verifyMemberSnapshotManifest != "" || cfg.reconcileRetiredMemberSnapshot != "" || cfg.retiredMemberConfig != "" ||
			cfg.durableTransitionJournal == "" {
			log.Fatal("reseed handoff commit requires a stopped collector journal and cannot run with another offline mode")
		}
		if _, err := hostsecurity.Harden(); err != nil {
			log.Fatalf("apply host hardening before unlocking frozen member Master Keys: %v", err)
		}
		report, err := commitReseedHandoff(context.Background(), cfg.commitReseedHandoff, cfg.durableTransitionJournal)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			log.Fatal(err)
		}
		return
	}
	if cfg.preflightReseedHandoff != "" {
		if cfg.verifyDurableSnapshot != "" || cfg.compareMemberSnapshotReports != "" || cfg.verifyMemberSnapshotManifest != "" ||
			cfg.verifyArchiveReadback != "" || cfg.reconcileRetiredMemberSnapshot != "" || cfg.retiredMemberConfig != "" {
			log.Fatal("reseed handoff preflight cannot run with another offline mode")
		}
		if _, err := hostsecurity.Harden(); err != nil {
			log.Fatalf("apply host hardening before unlocking frozen member Master Keys: %v", err)
		}
		report, err := preflightReseedHandoff(context.Background(), cfg.preflightReseedHandoff)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			log.Fatal(err)
		}
		return
	}
	if cfg.reconcileRetiredMemberSnapshot != "" || cfg.retiredMemberConfig != "" {
		if cfg.reconcileRetiredMemberSnapshot == "" || cfg.retiredMemberConfig == "" || cfg.durableTransitionJournal == "" ||
			cfg.verifyDurableSnapshot != "" || cfg.verifyArchiveReadback != "" || cfg.compareMemberSnapshotReports != "" || cfg.verifyMemberSnapshotManifest != "" {
			log.Fatal("retired member reconciliation requires its frozen directory, member config and collector journal, without another offline mode")
		}
		members, err := parseExpectedMembers(cfg.members)
		if err != nil {
			log.Fatal(err)
		}
		if _, err := hostsecurity.Harden(); err != nil {
			log.Fatalf("apply host hardening before unlocking retired member Master Key: %v", err)
		}
		memberConfig, err := halroconfig.Load(cfg.retiredMemberConfig, halroconfig.LoadOptions{SkipListenerValidation: true})
		if err != nil {
			log.Fatal(err)
		}
		report, err := reconcileRetiredMemberSnapshot(context.Background(), cfg.durableTransitionJournal,
			cfg.environment, cfg.cluster, members, memberConfig, cfg.reconcileRetiredMemberSnapshot)
		if err != nil {
			log.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			log.Fatal(err)
		}
		return
	}
	if cfg.verifyDurableSnapshot != "" {
		modes := 0
		for _, selected := range []string{cfg.verifyArchiveReadback, cfg.compareMemberSnapshotReports, cfg.verifyMemberSnapshotManifest} {
			if selected != "" {
				modes++
			}
		}
		if modes > 1 {
			log.Fatal("choose one snapshot comparison or archive readback mode")
		}
		members, err := parseExpectedMembers(cfg.members)
		if err != nil {
			log.Fatal(err)
		}
		if cfg.verifyArchiveReadback != "" {
			readback, err := verifyArchiveReadback(cfg.verifyDurableSnapshot, cfg.verifyArchiveReadback, cfg.environment, cfg.cluster, members)
			if err != nil {
				log.Fatal(err)
			}
			if err := json.NewEncoder(os.Stdout).Encode(readback); err != nil {
				log.Fatal(err)
			}
			return
		}
		report, err := verifyDurableSnapshot(cfg.verifyDurableSnapshot, cfg.environment, cfg.cluster, members)
		if err != nil {
			log.Fatal(err)
		}
		if cfg.compareMemberSnapshotReports != "" {
			comparison, err := compareMemberSnapshotReports(report, cfg.compareMemberSnapshotReports)
			if err != nil {
				log.Fatal(err)
			}
			if err := json.NewEncoder(os.Stdout).Encode(comparison); err != nil {
				log.Fatal(err)
			}
			return
		}
		if cfg.verifyMemberSnapshotManifest != "" {
			if _, err := hostsecurity.Harden(); err != nil {
				log.Fatalf("apply host hardening before unlocking member Master Keys: %v", err)
			}
			comparison, err := verifyMemberSnapshotHeads(context.Background(), report, cfg.verifyMemberSnapshotManifest)
			if err != nil {
				log.Fatal(err)
			}
			if err := json.NewEncoder(os.Stdout).Encode(comparison); err != nil {
				log.Fatal(err)
			}
			return
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			log.Fatal(err)
		}
		return
	}
	if cfg.verifyArchiveReadback != "" || cfg.compareMemberSnapshotReports != "" || cfg.verifyMemberSnapshotManifest != "" {
		log.Fatal("archive readback or member snapshot comparison requires -verify-durable-snapshot")
	}
	s, tlsConfig, err := newServer(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if s.archive != nil {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.pollArchive(ctx)
	}
	if s.durableArchive != nil {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go s.pollDurableArchive(ctx)
	}
	httpServer := &http.Server{
		Addr: cfg.listen, Handler: s.routes(), TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second,
	}
	log.Printf("HA health view listening on %s", cfg.listen)
	log.Fatal(httpServer.ListenAndServeTLS(cfg.cert, cfg.key))
}

func newServer(cfg config) (*server, *tls.Config, error) {
	if cfg.environment == "" || cfg.cluster == "" || cfg.members == "" || cfg.cert == "" || cfg.key == "" || cfg.clientCA == "" {
		return nil, nil, errors.New("environment, cluster, members, tls-cert, tls-key and client-ca are required")
	}
	base, err := url.Parse(cfg.prometheus)
	if err != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") ||
		base.Hostname() == "" || base.User != nil || (base.Path != "" && base.Path != "/") ||
		base.RawPath != "" || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || base.Opaque != "" {
		return nil, nil, errors.New("prometheus must be an HTTP(S) origin URL without credentials, path, query or fragment")
	}
	if cfg.clientURL != "" {
		probeURL, err := url.Parse(cfg.clientURL)
		if err != nil || probeURL == nil || probeURL.Scheme != "https" || probeURL.Host == "" || probeURL.User != nil || probeURL.RawQuery != "" || probeURL.Fragment != "" || probeURL.Path != "/" {
			return nil, nil, errors.New("client-url must be an HTTPS Service root URL")
		}
	}
	if cfg.runbookBase != "" && !validRunbookBaseURL(cfg.runbookBase) {
		return nil, nil, errors.New("runbook-base-url must be an HTTPS origin root without credentials, query or fragment")
	}
	members, err := parseExpectedMembers(cfg.members)
	if err != nil {
		return nil, nil, err
	}
	caPEM, err := os.ReadFile(cfg.clientCA)
	if err != nil {
		return nil, nil, fmt.Errorf("read client CA: %w", err)
	}
	ca := x509.NewCertPool()
	if !ca.AppendCertsFromPEM(caPEM) {
		return nil, nil, errors.New("client CA has no valid certificate")
	}
	if _, err := tls.LoadX509KeyPair(cfg.cert, cfg.key); err != nil {
		return nil, nil, fmt.Errorf("load serving certificate: %w", err)
	}
	probeRoots, err := x509.SystemCertPool()
	if err != nil {
		return nil, nil, fmt.Errorf("load system roots: %w", err)
	}
	if cfg.probeCA != "" {
		probePEM, err := os.ReadFile(cfg.probeCA)
		if err != nil || !probeRoots.AppendCertsFromPEM(probePEM) {
			return nil, nil, errors.New("probe CA has no valid certificate")
		}
	}
	prometheusClient, err := newPrometheusClient(cfg, base, probeRoots)
	if err != nil {
		return nil, nil, err
	}
	statusCollectors, err := loadStatusCollectors(cfg.statusManifest, members)
	if err != nil {
		return nil, nil, err
	}
	clientFinal, err := loadClientFinalManifest(cfg.clientFinalManifest)
	if err != nil {
		return nil, nil, err
	}
	var archive *eventArchive
	if cfg.eventJournal != "" {
		if len(statusCollectors) != len(members) {
			return nil, nil, errors.New("event-journal requires an exact member status manifest")
		}
		for _, collector := range statusCollectors {
			if !collector.config.RequireLiveTransitions || !collector.config.RequireAvailabilityTransitions || !collector.config.RequireReplicaStageTransitions {
				return nil, nil, errors.New("event-journal requires live, availability and replica stage transitions from every member")
			}
		}
		archive, err = openEventArchive(cfg.eventJournal, cfg.environment, cfg.cluster, members)
		if err != nil {
			return nil, nil, err
		}
	}
	var durableArchive *durableArchive
	if cfg.durableTransitionJournal != "" {
		if len(statusCollectors) != len(members) {
			return nil, nil, errors.New("durable-transition-journal requires an exact member status manifest")
		}
		durableArchive, err = openDurableArchive(cfg.durableTransitionJournal, cfg.environment, cfg.cluster, members)
		if err != nil {
			if archive != nil {
				_ = archive.Close()
			}
			return nil, nil, err
		}
	}
	return &server{
		client: prometheusClient,
		probeHTTP: &http.Client{
			Timeout:       4 * time.Second,
			Transport:     &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: probeRoots}},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		base: base, environment: cfg.environment, cluster: cfg.cluster,
		members: members, clientURL: cfg.clientURL, runbookBase: cfg.runbookBase, statusCollectors: statusCollectors, archive: archive, durableArchive: durableArchive, clientFinal: clientFinal,
	}, &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: ca}, nil
}

func newPrometheusClient(cfg config, base *url.URL, loopbackRoots *x509.CertPool) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: loopbackRoots}
	configuredCredential := cfg.prometheusCA != "" || cfg.prometheusClientCert != "" || cfg.prometheusClientKey != ""
	if isLoopback(base.Hostname()) {
		if configuredCredential {
			return nil, errors.New("Prometheus mTLS flags require a remote HTTPS origin")
		}
	} else {
		if base.Scheme != "https" || cfg.prometheusCA == "" || cfg.prometheusClientCert == "" || cfg.prometheusClientKey == "" {
			return nil, errors.New("remote Prometheus requires HTTPS, a CA and a client certificate/key")
		}
		caPEM, err := os.ReadFile(cfg.prometheusCA)
		if err != nil {
			return nil, fmt.Errorf("read Prometheus CA: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("Prometheus CA has no valid certificate")
		}
		keyInfo, err := os.Lstat(cfg.prometheusClientKey)
		if err != nil || !keyInfo.Mode().IsRegular() || keyInfo.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("Prometheus client key must be a private regular file")
		}
		certificate, err := tls.LoadX509KeyPair(cfg.prometheusClientCert, cfg.prometheusClientKey)
		if err != nil {
			return nil, fmt.Errorf("load Prometheus client certificate: %w", err)
		}
		tlsConfig.RootCAs = roots
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	return &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{Proxy: nil, TLSClientConfig: tlsConfig},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func parseExpectedMembers(raw string) ([]string, error) {
	members := strings.Split(raw, ",")
	seen := map[string]bool{}
	for _, member := range members {
		if member == "" || strings.TrimSpace(member) != member || seen[member] {
			return nil, errors.New("members must be unique nonempty instance labels")
		}
		seen[member] = true
	}
	if len(members) < 2 || len(members) > 3 {
		return nil, errors.New("HA health requires exactly two or three expected members")
	}
	return members, nil
}

func validRunbookBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u != nil && u.Scheme == "https" && u.Hostname() != "" && u.Path == "/" &&
		!strings.Contains(raw, "#") && u.RawPath == "" &&
		u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Opaque == ""
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *server) selector(metric string) string {
	return metric + `{environment=` + strconv.Quote(s.environment) + `,cluster=` + strconv.Quote(s.cluster) + `}`
}

func (s *server) selectorWith(metric, label string) string {
	selected := s.selector(metric)
	return selected[:len(selected)-1] + `,` + label + `}`
}

func (s *server) memberSelector(metric string) string {
	return s.selectorWith(metric, `job="halro",expected_target="true"`)
}

func (s *server) memberSelectorWith(metric, label string) string {
	return s.selectorWith(metric, `job="halro",expected_target="true",`+label)
}

func (s *server) query(ctx context.Context, expression string, rangeMinutes int, at ...time.Time) (prometheusResult, error) {
	u := *s.base
	if rangeMinutes == 0 {
		u.Path = "/api/v1/query"
	} else {
		u.Path = "/api/v1/query_range"
	}
	params := url.Values{"query": {expression}}
	if rangeMinutes == 0 && len(at) > 0 {
		params.Set("time", strconv.FormatFloat(float64(at[0].UnixNano())/1e9, 'f', 3, 64))
	}
	if rangeMinutes != 0 {
		now := time.Now().UTC()
		params.Set("start", strconv.FormatInt(now.Add(-time.Duration(rangeMinutes)*time.Minute).Unix(), 10))
		params.Set("end", strconv.FormatInt(now.Unix(), 10))
		params.Set("step", "30")
	}
	u.RawQuery = params.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return prometheusResult{}, err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return prometheusResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return prometheusResult{}, fmt.Errorf("Prometheus query status %d", response.StatusCode)
	}
	var result prometheusResult
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&result); err != nil {
		return prometheusResult{}, err
	}
	if result.Status != "success" {
		return prometheusResult{}, errors.New("Prometheus query failed")
	}
	return result, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		content, _ := ui.ReadFile("ui/index.html")
		content = []byte(strings.Replace(string(content), "__HALRO_RUNBOOK_BASE__", html.EscapeString(s.runbookBase), 1))
		_, _ = w.Write(content)
	})
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		content, _ := ui.ReadFile("ui/app.js")
		_, _ = w.Write(content)
	})
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/history", s.history)
	mux.HandleFunc("GET /api/latency", s.latency)
	mux.HandleFunc("GET /api/impact", s.impact)
	mux.HandleFunc("GET /api/client-final", s.clientFinalResponse)
	mux.HandleFunc("GET /api/alerts", s.alerts)
	mux.HandleFunc("GET /api/alert-rule", s.alertRule)
	mux.HandleFunc("GET /api/evidence", s.evidence)
	mux.HandleFunc("GET /api/event-archive", s.eventArchiveResponse)
	mux.HandleFunc("GET /api/durable-transitions", s.durableArchiveResponse)
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"live"}`))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writer := &auditWriter{ResponseWriter: w}
		mux.ServeHTTP(writer, r)
		if writer.status == 0 {
			writer.status = http.StatusOK
		}
		principal := "unknown"
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			principal = r.TLS.PeerCertificates[0].Subject.String()
		}
		log.Printf("ha-health access principal=%q remote=%q method=%q path=%q status=%d", principal, r.RemoteAddr, r.Method, r.URL.Path, writer.status)
	})
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	expression := s.haMetricsExpression()
	confirmedWaits := `sum by (environment, region, cluster, instance) (increase(` +
		s.memberSelectorWith("halro_replication_required_confirmation_wait_total", `outcome="confirmed"`) + `[5m]))`
	confirmedWaitResets := `sum by (environment, region, cluster, instance) (resets(` +
		s.memberSelectorWith("halro_replication_required_confirmation_wait_total", `outcome="confirmed"`) + `[5m])) == 0`
	confirmationExpression := confirmedWaits +
		` and on (environment, region, cluster, instance) (` + confirmedWaitResets + `)` +
		` and on (environment, region, cluster, instance) (` +
		s.selector("halro:ha_epoch_stable:bool") + ` == 1)`
	confirmationCoverageExpression := s.memberSelectorWith("halro_replication_required_confirmation_wait_total", `outcome="confirmed"`) + "[45s]"
	epochCoverageExpression := s.selector("halro:ha_epoch_stable:bool") + "[45s]"
	type queryResult struct {
		data prometheusResult
		err  error
	}
	type alertResult struct {
		alerts []currentAlert
		err    error
	}
	metricsCh := make(chan queryResult, 1)
	confirmationCh := make(chan queryResult, 1)
	confirmationCoverageCh := make(chan queryResult, 1)
	epochCoverageCh := make(chan queryResult, 1)
	alertsCh := make(chan alertResult, 1)
	probeCh := make(chan hahealth.Signal, 1)
	statusCh := make(chan []memberStatus, 1)
	ctx := r.Context()
	go func() { data, err := s.currentMetrics(ctx, expression); metricsCh <- queryResult{data, err} }()
	go func() { data, err := s.query(ctx, confirmationExpression, 0); confirmationCh <- queryResult{data, err} }()
	go func() {
		data, err := s.currentMetrics(ctx, confirmationCoverageExpression)
		confirmationCoverageCh <- queryResult{data, err}
	}()
	go func() {
		data, err := s.currentMetrics(ctx, epochCoverageExpression)
		epochCoverageCh <- queryResult{data, err}
	}()
	go func() { alerts, err := s.fetchAlerts(ctx); alertsCh <- alertResult{alerts, err} }()
	go func() { probeCh <- s.probeClient(ctx) }()
	go func() { statusCh <- s.collectStatuses(ctx) }()
	metrics := <-metricsCh
	if metrics.err != nil {
		http.Error(w, "monitoring data unavailable", http.StatusServiceUnavailable)
		return
	}
	observations := parseMembers(metrics.data, s.members)
	confirmation := <-confirmationCh
	confirmationCoverage := <-confirmationCoverageCh
	epochCoverage := <-epochCoverageCh
	var observed *bool
	evidence := confirmationEvidence{Status: "no_fresh_sample", Source: `Prometheus increase(halro_replication_required_confirmation_wait_total{outcome="confirmed"}[5m]) gated by no counter reset, fresh stable-epoch rule, and fresh ledger/metadata counter scrapes`}
	if confirmation.err != nil || confirmationCoverage.err != nil || epochCoverage.err != nil {
		evidence.Status = "query_failed"
	} else {
		var candidates []confirmationEvidence
		for _, item := range confirmation.data.Data.Result {
			for _, member := range observations {
				if member.Instance == item.Metric["instance"] && member.Role == "primary" {
					if value, at, ok := sample(item.Value); ok && value >= 0 && time.Since(at) <= 30*time.Second && time.Since(at) >= 0 {
						candidates = append(candidates, confirmationEvidence{Status: "nonpositive", Source: evidence.Source,
							Instance: member.Instance, Increase5m: &value, EvaluatedAt: at})
					}
				}
			}
		}
		if len(candidates) == 1 {
			checkedAt := time.Now().UTC()
			rawAt, countersFresh := freshConfirmationCounters(confirmationCoverage.data, candidates[0].Instance, checkedAt)
			epochAt, epochFresh := freshStableEpoch(epochCoverage.data, candidates[0].Instance, checkedAt)
			if countersFresh && epochFresh {
				evidence = candidates[0]
				evidence.SampledAt = rawAt
				evidence.EpochSampledAt = epochAt
				if *evidence.Increase5m > 0 {
					evidence.Status = "observed"
					observed = boolPtr(true)
				}
			}
		} else if len(candidates) > 1 {
			evidence.Status = "ambiguous"
		}
	}
	evaluatedAt := time.Now().UTC()
	unexpected := unexpectedMembers(metrics.data, s.members, evaluatedAt, 30*time.Second)
	assessment := hahealth.Input{
		Now: evaluatedAt, MaxSampleAge: 30 * time.Second,
		Expected: s.members, ClusterID: s.cluster, Members: observations, Unexpected: unexpected, ClientProbe: <-probeCh,
		ObservedConfirmation: observed,
	}
	result := hahealth.Evaluate(assessment)
	statuses := <-statusCh
	if markPrefixConflicts(statuses) {
		prefixConflict := hahealth.Signal{Level: hahealth.Critical, Reason: "authenticated ordering prefixes differ at the same index"}
		result.Safety = hahealth.Combine(result.Safety, prefixConflict)
		result.Overall = hahealth.Combine(result.Overall, prefixConflict)
	}
	for index := range statuses {
		status := &statuses[index]
		if status.Error == "" {
			for _, member := range observations {
				age := evaluatedAt.Sub(member.SampledAt)
				if member.Instance == status.NodeID && member.Up != nil && *member.Up && age >= 0 && age <= 30*time.Second &&
					machineStatusDisagrees(*status, member) {
					status.Error = "metrics_disagreement"
					result.Safety = hahealth.Combine(result.Safety, hahealth.Signal{Level: hahealth.Unknown, Reason: "member machine status disagrees with fresh Metrics: " + status.NodeID})
					break
				}
			}
		}
		if status.HealthProbeError != "" {
			result.Overall = hahealth.Combine(result.Overall, hahealth.Signal{Level: hahealth.Unknown, Reason: "member health probe incomplete: " + status.NodeID})
		}
		if status.HealthLive != nil && !*status.HealthLive {
			result.Overall = hahealth.Combine(result.Overall, hahealth.Signal{Level: hahealth.Degraded, Reason: "member liveness probe failed: " + status.NodeID})
		}
		primary := status.Role == "primary"
		for _, member := range observations {
			primary = primary || member.Instance == status.NodeID && member.Role == "primary"
		}
		if primary && status.HealthReady != nil && !*status.HealthReady {
			result.Overall = hahealth.Combine(result.Overall, hahealth.Signal{Level: hahealth.Degraded, Reason: "Primary readiness probe failed: " + status.NodeID})
		}
		if status.Error != "" {
			result.Overall = hahealth.Combine(result.Overall, hahealth.Signal{Level: hahealth.Unknown, Reason: "member machine status incomplete: " + status.NodeID})
		}
	}
	archive := s.archiveView()
	if archive.Status != "not_configured" {
		if archive.Status != "ok" {
			result.Overall = hahealth.Combine(result.Overall, hahealth.Signal{Level: hahealth.Unknown, Reason: "event archive unavailable: " + archive.Status})
		} else if len(archive.CollectionErrors) > 0 {
			result.Overall = hahealth.Combine(result.Overall, hahealth.Signal{Level: hahealth.Unknown, Reason: "event archive member collection incomplete"})
		}
	}
	durableArchive := s.durableArchiveView()
	if durableArchive.Status != "not_configured" && durableArchive.Status != "caught_up" {
		result.Overall = hahealth.Combine(result.Overall, hahealth.Signal{Level: hahealth.Unknown, Reason: "durable transition coverage incomplete"})
	}
	alertQuery := <-alertsCh
	if alertQuery.err != nil {
		result.Overall = hahealth.Combine(result.Overall, hahealth.Signal{Level: hahealth.Unknown, Reason: "current HA alerts unavailable"})
	} else {
		for _, alert := range alertQuery.alerts {
			if alert.State != "firing" {
				continue
			}
			level := hahealth.Degraded
			if alert.Severity == "critical" {
				level = hahealth.Critical
			}
			result.Overall = hahealth.Combine(result.Overall, hahealth.Signal{Level: level, Reason: "firing alert: " + alert.Name})
		}
	}
	respondedAt := time.Now().UTC()
	if observed != nil && !freshConfirmationEvidence(evidence, respondedAt) {
		assessment.ObservedConfirmation = nil
		evidence.Status = "no_fresh_sample"
	}
	assessment.Now = respondedAt
	current := hahealth.Evaluate(assessment)
	result.Safety = hahealth.Combine(result.Safety, current.Safety)
	result.Confirmation = hahealth.Combine(result.Confirmation, current.Confirmation)
	result.Catchup = hahealth.Combine(result.Catchup, current.Catchup)
	result.Overall = hahealth.Combine(result.Overall, result.Client, result.Safety, result.Confirmation, result.Catchup)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		hahealth.Result
		Environment          string               `json:"environment"`
		Cluster              string               `json:"cluster"`
		ExpectedMembers      int                  `json:"expected_members"`
		ObservedAt           time.Time            `json:"observed_at"`
		MemberStatuses       []memberStatus       `json:"member_statuses,omitempty"`
		ConfirmationEvidence confirmationEvidence `json:"confirmation_evidence"`
	}{result, s.environment, s.cluster, len(s.members), respondedAt, statuses, evidence})
}

func machineStatusDisagrees(status memberStatus, member hahealth.Member) bool {
	return member.Incarnation != "" && member.Incarnation != status.Incarnation ||
		member.Role != "" && member.Role != status.Role ||
		member.Term != nil && *member.Term != status.Term ||
		member.PromisedTerm != nil && *member.PromisedTerm != status.PromisedTerm ||
		member.StartupReady != nil && *member.StartupReady != status.StartupReady ||
		member.Unavailable != nil && status.ReplicationUnavailable != nil && *member.Unavailable != *status.ReplicationUnavailable
}

func freshConfirmationCounters(data prometheusResult, primary string, now time.Time) (time.Time, bool) {
	seen := map[string]bool{}
	var oldest time.Time
	for _, item := range data.Data.Result {
		if item.Metric["instance"] != primary {
			continue
		}
		store := item.Metric["store"]
		value, at, ok := sample(item.Value)
		if item.Metric["__name__"] != "halro_replication_required_confirmation_wait_total" ||
			item.Metric["outcome"] != "confirmed" ||
			(store != "ledger" && store != "metadata") || seen[store] || !ok || value < 0 ||
			at.After(now) || now.Sub(at) > 30*time.Second {
			return time.Time{}, false
		}
		seen[store] = true
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	return oldest, seen["ledger"] && seen["metadata"]
}

func freshStableEpoch(data prometheusResult, primary string, now time.Time) (time.Time, bool) {
	seen := false
	var sampledAt time.Time
	for _, item := range data.Data.Result {
		if item.Metric["instance"] != primary {
			continue
		}
		value, at, ok := sample(item.Value)
		if seen || item.Metric["__name__"] != "halro:ha_epoch_stable:bool" || !ok || value != 1 ||
			at.After(now) || now.Sub(at) > 30*time.Second {
			return time.Time{}, false
		}
		seen = true
		sampledAt = at
	}
	return sampledAt, seen
}

func freshConfirmationEvidence(evidence confirmationEvidence, now time.Time) bool {
	for _, sampledAt := range []time.Time{evidence.SampledAt, evidence.EpochSampledAt} {
		age := now.Sub(sampledAt)
		if sampledAt.IsZero() || age < 0 || age > 30*time.Second {
			return false
		}
	}
	return true
}

func (s *server) history(w http.ResponseWriter, r *http.Request) {
	minutes, ok := historyMinutes(r)
	if !ok {
		http.Error(w, "minutes must be 15, 60, 360 or 1440", http.StatusBadRequest)
		return
	}
	response, err := s.query(r.Context(), s.historyExpression(), minutes)
	if err != nil {
		http.Error(w, "monitoring history unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Series        any            `json:"series"`
		SampledEvents []sampledEvent `json:"sampled_events"`
	}{response.Data.Result, sampledEvents(response, s.members)})
}

func historyMinutes(r *http.Request) (int, bool) {
	minutes, err := strconv.Atoi(r.URL.Query().Get("minutes"))
	return minutes, err == nil && slices.Contains([]int{15, 60, 360, 1440}, minutes)
}

func (s *server) haMetricsExpression() string {
	const names = `up|halro_cluster_(member_info|role|term|promised_term|incarnation_info|maintenance)|halro_replication_(index|startup_ready|unavailable|peer_connected|member_incompatible)`
	return `{__name__=~` + strconv.Quote(names) + `,environment=` + strconv.Quote(s.environment) + `,cluster=` + strconv.Quote(s.cluster) + `,job="halro",expected_target="true"}[45s]`
}

// Prometheus instant vectors carry the query evaluation time, which can make
// a lookback value appear freshly scraped. A raw range vector preserves each
// scrape time. Keep only its last sample for the current evaluator.
func (s *server) currentMetrics(ctx context.Context, expression string, at ...time.Time) (prometheusResult, error) {
	result, err := s.query(ctx, expression, 0, at...)
	if err != nil {
		return prometheusResult{}, err
	}
	if result.Data.ResultType != "matrix" {
		return prometheusResult{}, errors.New("current HA metrics must be raw range samples")
	}
	for i := range result.Data.Result {
		series := &result.Data.Result[i]
		series.Value = nil
		var previous float64
		for _, pair := range series.Values {
			var at float64
			if len(pair) != 2 || json.Unmarshal(pair[0], &at) != nil || math.IsNaN(at) || math.IsInf(at, 0) || at <= previous {
				return prometheusResult{}, errors.New("current HA metric samples are not ordered raw samples")
			}
			previous = at
		}
		if len(series.Values) > 0 {
			series.Value = series.Values[len(series.Values)-1]
		}
		series.Values = nil
	}
	result.Data.ResultType = "vector"
	return result, nil
}

func (s *server) historyExpression() string {
	return s.memberSelector("halro_replication_index") + " or " +
		s.memberSelector("halro_cluster_term") + " or " +
		s.memberSelector("halro_cluster_role") + " or " +
		s.memberSelector("halro_cluster_maintenance") + " or " +
		s.memberSelector("halro_replication_peer_connected") + " or " +
		s.memberSelector("halro_replication_state") + " or " +
		s.memberSelector("halro_cluster_incarnation_info") + " or " +
		`ALERTS{environment=` + strconv.Quote(s.environment) + `,cluster=` + strconv.Quote(s.cluster) + `,category="ha",alertstate="firing"}` + " or " +
		`ALERTS{environment=` + strconv.Quote(s.environment) + `,cluster=` + strconv.Quote(s.cluster) + `,alertname="HalroTargetDown",alertstate="firing"}`
}

func (s *server) evidence(w http.ResponseWriter, r *http.Request) {
	minutes, ok := historyMinutes(r)
	if !ok {
		http.Error(w, "minutes must be 15, 60, 360 or 1440", http.StatusBadRequest)
		return
	}
	type queryResult struct {
		data prometheusResult
		err  error
	}
	type alertResult struct {
		alerts []currentAlert
		err    error
	}
	metricsCh := make(chan queryResult, 1)
	historyCh := make(chan queryResult, 1)
	alertsCh := make(chan alertResult, 1)
	clientFinalCh := make(chan clientFinalResult, 1)
	statusesCh := make(chan []memberStatus, 1)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		data, err := s.currentMetrics(ctx, s.haMetricsExpression())
		metricsCh <- queryResult{data, err}
	}()
	go func() { data, err := s.query(ctx, s.historyExpression(), minutes); historyCh <- queryResult{data, err} }()
	go func() { alerts, err := s.fetchAlerts(ctx); alertsCh <- alertResult{alerts, err} }()
	go func() { clientFinalCh <- s.clientFinalResults(ctx) }()
	go func() { statusesCh <- s.collectStatuses(ctx) }()
	metrics, history, alerts := <-metricsCh, <-historyCh, <-alertsCh
	if metrics.err != nil || history.err != nil || alerts.err != nil {
		http.Error(w, "monitoring evidence incomplete", http.StatusServiceUnavailable)
		return
	}
	clientFinal := <-clientFinalCh
	statuses := <-statusesCh
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="halro-ha-evidence.json"`)
	_ = json.NewEncoder(w).Encode(struct {
		GeneratedAt    time.Time          `json:"generated_at"`
		Environment    string             `json:"environment"`
		Cluster        string             `json:"cluster"`
		Expected       []string           `json:"expected_members"`
		WindowMinutes  int                `json:"window_minutes"`
		Source         string             `json:"source"`
		Caveat         string             `json:"caveat"`
		MetricSnapshot any                `json:"metric_snapshot"`
		History        any                `json:"history"`
		Alerts         any                `json:"current_alerts"`
		MemberStatuses []memberStatus     `json:"member_statuses,omitempty"`
		SampledEvents  []sampledEvent     `json:"sampled_events"`
		EventArchive   eventArchiveView   `json:"event_archive"`
		DurableArchive durableArchiveView `json:"durable_transitions"`
		ClientFinal    clientFinalResult  `json:"client_final"`
	}{
		GeneratedAt: time.Now().UTC(), Environment: s.environment, Cluster: s.cluster,
		Expected: append([]string(nil), s.members...), WindowMinutes: minutes,
		Source:         "Prometheus fixed read-only queries",
		Caveat:         "Raw observations; missing samples and matching indexes do not authorize promotion",
		MetricSnapshot: metrics.data.Data.Result, History: history.data.Data.Result, Alerts: alerts.alerts,
		MemberStatuses: statuses, SampledEvents: sampledEvents(history.data, s.members), EventArchive: s.archiveView(), DurableArchive: s.durableArchiveView(), ClientFinal: clientFinal,
	})
}

func (s *server) alerts(w http.ResponseWriter, r *http.Request) {
	alerts, err := s.fetchAlerts(r.Context())
	if err != nil {
		http.Error(w, "monitoring alerts unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(alerts)
}

func (s *server) alertRule(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if len(name) < len("HalroX") || len(name) > 80 || !strings.HasPrefix(name, "Halro") {
		http.Error(w, "invalid HA alert name", http.StatusBadRequest)
		return
	}
	for _, c := range name {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			http.Error(w, "invalid HA alert name", http.StatusBadRequest)
			return
		}
	}
	evidence, err := s.fetchAlertRule(r.Context(), name)
	if err != nil {
		http.Error(w, "HA alert rule unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(evidence)
}

func (s *server) fetchAlertRule(ctx context.Context, name string) (alertRuleEvidence, error) {
	u := *s.base
	u.Path = "/api/v1/rules"
	query := url.Values{}
	query.Set("type", "alert")
	query.Set("rule_group[]", "halro-alerts")
	query.Set("rule_name[]", name)
	query.Set("exclude_alerts", "true")
	u.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return alertRuleEvidence{}, err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return alertRuleEvidence{}, err
	}
	defer response.Body.Close()
	var source struct {
		Status string `json:"status"`
		Data   struct {
			Groups []struct {
				Name  string `json:"name"`
				Rules []struct {
					Name           string  `json:"name"`
					Type           string  `json:"type"`
					Query          string  `json:"query"`
					Duration       float64 `json:"duration"`
					Health         string  `json:"health"`
					LastEvaluation string  `json:"lastEvaluation"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&source) != nil || source.Status != "success" {
		return alertRuleEvidence{}, errors.New("Prometheus HA rule unavailable")
	}
	evidence := alertRuleEvidence{Status: "missing"}
	for _, group := range source.Data.Groups {
		if group.Name != "halro-alerts" {
			continue
		}
		for _, rule := range group.Rules {
			if rule.Name != name || rule.Type != "alerting" {
				continue
			}
			if evidence.Status == "available" {
				return alertRuleEvidence{Status: "ambiguous"}, nil
			}
			if rule.Query == "" || len(rule.Query) > 8192 || rule.Duration < 0 || math.IsNaN(rule.Duration) || math.IsInf(rule.Duration, 0) {
				return alertRuleEvidence{Status: "invalid"}, nil
			}
			evidence = alertRuleEvidence{Status: "available", Query: rule.Query, ForSeconds: rule.Duration,
				Health: rule.Health, LastEvaluation: rule.LastEvaluation}
		}
	}
	return evidence, nil
}

func (s *server) fetchAlerts(ctx context.Context) ([]currentAlert, error) {
	u := *s.base
	u.Path = "/api/v1/alerts"
	u.RawQuery = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var source struct {
		Status string `json:"status"`
		Data   struct {
			Alerts []struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
				State       string            `json:"state"`
				ActiveAt    string            `json:"activeAt"`
				Value       string            `json:"value"`
			} `json:"alerts"`
		} `json:"data"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&source) != nil || source.Status != "success" {
		return nil, errors.New("Prometheus alerts unavailable")
	}
	alerts := make([]currentAlert, 0)
	for _, alert := range source.Data.Alerts {
		if (alert.Labels["category"] != "ha" && alert.Labels["alertname"] != "HalroTargetDown") ||
			alert.Labels["environment"] != s.environment || alert.Labels["cluster"] != s.cluster {
			continue
		}
		value := ""
		if parsed, err := strconv.ParseFloat(alert.Value, 64); err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) && len(alert.Value) <= 64 {
			value = alert.Value
		}
		alerts = append(alerts, currentAlert{
			Name: alert.Labels["alertname"], State: alert.State,
			Severity: alert.Labels["severity"], Instance: alert.Labels["instance"],
			Started: alert.ActiveAt, Summary: alert.Annotations["summary"], Runbook: alert.Annotations["runbook_url"], Value: value,
		})
	}
	return alerts, nil
}

func parseMembers(result prometheusResult, expected []string) []hahealth.Member {
	allowed := make(map[string]bool, len(expected))
	for _, name := range expected {
		allowed[name] = true
	}
	byName := map[string]*hahealth.Member{}
	seenSeries := map[string]map[memberSeriesKey]bool{}
	for _, item := range result.Data.Result {
		name := item.Metric["instance"]
		if !allowed[name] {
			continue
		}
		value, at, ok := sample(item.Value)
		if !ok {
			member := byName[name]
			if member == nil {
				member = &hahealth.Member{Instance: name}
				byName[name] = member
			}
			member.Conflicted = true
			continue
		}
		member := byName[name]
		if member == nil {
			member = &hahealth.Member{Instance: name, SampledAt: at, NewestSampledAt: at, Peers: map[string]*bool{}}
			byName[name] = member
		} else if member.SampledAt.IsZero() || at.Before(member.SampledAt) {
			member.SampledAt = at
		}
		if at.After(member.NewestSampledAt) {
			member.NewestSampledAt = at
		}
		if key, known := logicalMemberSeriesKey(item.Metric); known {
			if seenSeries[name] == nil {
				seenSeries[name] = map[memberSeriesKey]bool{}
			}
			if seenSeries[name][key] {
				member.Conflicted = true
			}
			seenSeries[name][key] = true
		}
		switch item.Metric["__name__"] {
		case "up":
			assignObservedBinary(&member.Up, value, member)
			member.UpSampledAt = at
		case "halro_cluster_member_info":
			if value != 0 && value != 1 {
				member.Conflicted = true
			}
			if value == 1 {
				clusterID, nodeID := item.Metric["cluster_id"], item.Metric["node_id"]
				if member.ClusterID != "" && member.ClusterID != clusterID || member.NodeID != "" && member.NodeID != nodeID {
					member.Conflicted = true
				}
				member.ClusterID, member.NodeID = clusterID, nodeID
			}
		case "halro_cluster_role":
			if value != 0 && value != 1 {
				member.Conflicted = true
			}
			if value == 1 {
				role := item.Metric["role"]
				if member.Role != "" && member.Role != role {
					member.Conflicted = true
				}
				member.Role = role
			}
		case "halro_cluster_incarnation_info":
			if value != 0 && value != 1 {
				member.Conflicted = true
			}
			if value == 1 {
				incarnation := item.Metric["incarnation"]
				if member.Incarnation != "" && member.Incarnation != incarnation {
					member.Conflicted = true
				}
				member.Incarnation = incarnation
			}
		case "halro_cluster_term":
			assignObservedUint(&member.Term, value, member)
		case "halro_cluster_promised_term":
			assignObservedUint(&member.PromisedTerm, value, member)
		case "halro_replication_index":
			switch item.Metric["kind"] {
			case "durable":
				assignObservedUint(&member.Durable, value, member)
			case "confirmed":
				assignObservedUint(&member.Confirmed, value, member)
			case "applied":
				assignObservedUint(&member.Applied, value, member)
			}
		case "halro_replication_startup_ready":
			assignObservedBinary(&member.StartupReady, value, member)
		case "halro_replication_unavailable":
			assignObservedBinary(&member.Unavailable, value, member)
		case "halro_cluster_maintenance":
			assignObservedBinary(&member.Maintenance, value, member)
		case "halro_replication_member_incompatible":
			reason := item.Metric["reason"]
			if value != 0 && value != 1 || reason != "schema" && reason != "key_challenge" && reason != "spki" {
				member.Conflicted = true
				break
			}
			if member.IncompatibleReasons == nil {
				member.IncompatibleReasons = map[string]*bool{}
			}
			observed := member.IncompatibleReasons[reason]
			if observed != nil && *observed != (value == 1) {
				member.Conflicted = true
			}
			member.IncompatibleReasons[reason] = boolPtr(value == 1)
			if member.Incompatible == nil {
				member.Incompatible = boolPtr(false)
			}
			if value == 1 {
				member.Incompatible = boolPtr(true)
			}
		case "halro_replication_peer_connected":
			peer := item.Metric["peer"]
			if value != 0 && value != 1 || !allowed[peer] || peer == name {
				member.Conflicted = true
				break
			}
			if member.Peers == nil {
				member.Peers = map[string]*bool{}
			}
			observed := member.Peers[peer]
			if observed != nil && *observed != (value == 1) {
				member.Conflicted = true
			}
			member.Peers[peer] = boolPtr(value == 1)
		}
	}
	members := make([]hahealth.Member, 0, len(byName))
	for _, member := range byName {
		members = append(members, *member)
	}
	return members
}

type memberSeriesKey struct {
	name, dimension string
}

func logicalMemberSeriesKey(metric map[string]string) (memberSeriesKey, bool) {
	name := metric["__name__"]
	switch name {
	case "up", "halro_cluster_member_info", "halro_cluster_role",
		"halro_cluster_incarnation_info", "halro_cluster_term", "halro_cluster_promised_term",
		"halro_replication_startup_ready", "halro_replication_unavailable", "halro_cluster_maintenance":
		return memberSeriesKey{name: name}, true
	case "halro_replication_index":
		return memberSeriesKey{name: name, dimension: metric["kind"]}, true
	case "halro_replication_member_incompatible":
		return memberSeriesKey{name: name, dimension: metric["reason"]}, true
	case "halro_replication_peer_connected":
		return memberSeriesKey{name: name, dimension: metric["peer"]}, true
	default:
		return memberSeriesKey{}, false
	}
}

func assignObserved[T comparable](destination **T, value *T, member *hahealth.Member) {
	if *destination != nil && value != nil && **destination != *value {
		member.Conflicted = true
	}
	*destination = value
}

func assignObservedBinary(destination **bool, value float64, member *hahealth.Member) {
	if value != 0 && value != 1 {
		member.Conflicted = true
		return
	}
	assignObserved(destination, boolPtr(value == 1), member)
}

func assignObservedUint(destination **uint64, value float64, member *hahealth.Member) {
	observed := uintPtr(value)
	if observed == nil {
		member.Conflicted = true
		return
	}
	assignObserved(destination, observed, member)
}

func unexpectedMembers(result prometheusResult, expected []string, now time.Time, maxAge time.Duration) []hahealth.UnexpectedMember {
	known := make(map[string]bool, len(expected))
	for _, name := range expected {
		known[name] = true
	}
	unexpected := map[string]*hahealth.UnexpectedMember{}
	for _, item := range result.Data.Result {
		name := item.Metric["instance"]
		missingInstance := name == ""
		if !missingInstance && known[name] {
			continue
		}
		member := unexpected[name]
		if member == nil {
			member = &hahealth.UnexpectedMember{Instance: name, IdentityMissing: missingInstance}
			unexpected[name] = member
		}
		value, at, ok := sample(item.Value)
		if !ok {
			continue
		}
		if member.SampledAt == nil || at.After(*member.SampledAt) {
			member.SampledAt = &at
		}
		if missingInstance {
			// The sample is real, but without its target identity it cannot
			// prove another member exists. Preserve it as unknown evidence.
			continue
		}
		age := now.Sub(at)
		if age >= 0 && age <= maxAge && (item.Metric["__name__"] != "up" || value == 1) {
			member.Observed = true
		}
	}
	members := make([]hahealth.UnexpectedMember, 0, len(unexpected))
	for _, member := range unexpected {
		members = append(members, *member)
	}
	slices.SortFunc(members, func(a, b hahealth.UnexpectedMember) int { return strings.Compare(a.Instance, b.Instance) })
	return members
}

func sample(raw []json.RawMessage) (float64, time.Time, bool) {
	if len(raw) != 2 {
		return 0, time.Time{}, false
	}
	var at float64
	var value string
	if json.Unmarshal(raw[0], &at) != nil || json.Unmarshal(raw[1], &value) != nil {
		return 0, time.Time{}, false
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, time.Time{}, false
	}
	return number, time.UnixMilli(int64(at * 1000)).UTC(), true
}

func uintPtr(value float64) *uint64 {
	index, ok := hahealth.NonnegativeIndex(value)
	if !ok {
		return nil
	}
	return &index
}

func boolPtr(value bool) *bool { return &value }

func (s *server) probeClient(ctx context.Context) hahealth.Signal {
	if s.clientURL == "" {
		return hahealth.Signal{Level: hahealth.Unknown, Reason: "client Service probe not configured"}
	}
	probeContext, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	client := s.probeHTTP
	if client == nil {
		client = s.client
	}
	return hahealth.ProbeClientService(probeContext, client, s.clientURL, s.cluster, "", max(6, len(s.members)*4)).Signal
}
