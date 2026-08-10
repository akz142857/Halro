package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const SchemaVersion = 1

type Config struct {
	Version        int            `yaml:"version"`
	Server         Server         `yaml:"server"`
	TLS            TLS            `yaml:"tls"`
	Storage        Storage        `yaml:"storage"`
	Admin          Admin          `yaml:"admin"`
	Usage          Usage          `yaml:"usage"`
	Gateway        Gateway        `yaml:"gateway"`
	Retry          Retry          `yaml:"retry"`
	CircuitBreaker CircuitBreaker `yaml:"circuit_breaker"`
	Alerts         Alerts         `yaml:"alerts"`
	Security       Security       `yaml:"security"`
	Metrics        Metrics        `yaml:"metrics"`
	Audit          Audit          `yaml:"audit"`
	ModelCatalog   ModelCatalog   `yaml:"model_catalog"`
}

type Server struct {
	GatewayListen     string   `yaml:"gateway_listen"`
	AdminListen       string   `yaml:"admin_listen"`
	MetricsListen     string   `yaml:"metrics_listen"`
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	ReadBodyTimeout   Duration `yaml:"read_body_timeout"`
	MaxHeaderBytes    int      `yaml:"max_header_bytes"`
	MaxRequestBytes   int64    `yaml:"max_request_bytes"`
}

type TLS struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type Storage struct {
	DataDir      string    `yaml:"data_dir"`
	MetadataFile string    `yaml:"metadata_file"`
	MasterKey    MasterKey `yaml:"master_key"`
}

const (
	MasterKeyModeFile     = "file"
	MasterKeyModeKeySlots = "key_slots"
)

type MasterKey struct {
	Mode            string          `yaml:"mode"`
	File            string          `yaml:"file,omitempty"`
	PrimarySlot     string          `yaml:"primary_slot,omitempty"`
	RecoverySlot    string          `yaml:"recovery_slot,omitempty"`
	StartupDeadline Duration        `yaml:"startup_deadline,omitempty"`
	CallTimeout     Duration        `yaml:"call_timeout,omitempty"`
	AllowedKMSKeys  []AllowedKMSKey `yaml:"allowed_kms_keys,omitempty"`
}

type AllowedKMSKey struct {
	Purpose   string `yaml:"purpose"`
	Provider  string `yaml:"provider"`
	Region    string `yaml:"region,omitempty"`
	Account   string `yaml:"account,omitempty"`
	KeyID     string `yaml:"key_id"`
	Endpoint  string `yaml:"endpoint,omitempty"`
	Algorithm string `yaml:"algorithm,omitempty"`
}

type Usage struct {
	Durability             string   `yaml:"durability"`
	Timezone               string   `yaml:"timezone"`
	WALQueueCapacity       int      `yaml:"wal_queue_capacity"`
	WALMaxBatch            int      `yaml:"wal_max_batch"`
	WALFlushInterval       Duration `yaml:"wal_flush_interval"`
	AnalyticsQueueCapacity int      `yaml:"analytics_queue_capacity"`
	CheckpointInterval     Duration `yaml:"checkpoint_interval"`
	ParquetInterval        Duration `yaml:"parquet_interval"`
	RetentionDays          int      `yaml:"retention_days"`
	// ExportFormat selects the container new Usage partitions are written in
	// (ADR 0017): "parquet" (default) or "ndjson". Existing partitions are
	// never rewritten — this only changes what gets written from here on.
	ExportFormat string `yaml:"export_format"`
}

const (
	UsageExportFormatParquet = "parquet"
	UsageExportFormatNDJSON  = "ndjson"
)

type Admin struct {
	SessionTTL               Duration                 `yaml:"session_ttl"`
	IdleTimeout              Duration                 `yaml:"idle_timeout"`
	LoginRPM                 int                      `yaml:"login_rpm"`
	ExternalOrigin           string                   `yaml:"external_origin"`
	MFAPolicy                string                   `yaml:"mfa_policy"`
	DeveloperWorkbench       string                   `yaml:"developer_workbench"`
	ModelCapabilityDetection ModelCapabilityDetection `yaml:"model_capability_detection"`
}

type ModelCapabilityDetection struct {
	FreshTTL            Duration `yaml:"fresh_ttl"`
	Retention           Duration `yaml:"retention"`
	RefreshCooldown     Duration `yaml:"refresh_cooldown"`
	TotalTimeout        Duration `yaml:"total_timeout"`
	GlobalConcurrency   int      `yaml:"global_concurrency"`
	ProviderConcurrency int      `yaml:"provider_concurrency"`
	MaxProviderCalls    int      `yaml:"max_provider_calls"`
	CreateRPM           int      `yaml:"create_rpm"`
}

// ModelCatalog governs optional signed background catalog updates. The remote
// endpoint, host allowlist and signature trust roots are compiled into Halro.
type ModelCatalog struct {
	Enabled             bool     `yaml:"enabled"`
	RefreshInterval     Duration `yaml:"refresh_interval"`
	PinnedRevision      string   `yaml:"pinned_revision,omitempty"`
	MaxDownloadBytes    int64    `yaml:"max_download_bytes"`
	MaxDecodedBytes     int64    `yaml:"max_decoded_bytes"`
	MaxCompressionRatio int64    `yaml:"max_compression_ratio"`
	MaxEntries          int      `yaml:"max_entries"`
}

type Gateway struct {
	RouteTotalTimeout             Duration        `yaml:"route_total_timeout"`
	AttemptConnectTimeout         Duration        `yaml:"attempt_connect_timeout"`
	AttemptResponseHeaderTimeout  Duration        `yaml:"attempt_response_header_timeout"`
	StreamIdleTimeout             Duration        `yaml:"stream_idle_timeout"`
	DownstreamWriteTimeout        Duration        `yaml:"downstream_write_timeout"`
	StreamMaxDuration             Duration        `yaml:"stream_max_duration"`
	MaxTotalAttempts              int             `yaml:"max_total_attempts"`
	HealthProbeInterval           Duration        `yaml:"health_probe_interval"`
	PricingClockRollbackTolerance Duration        `yaml:"pricing_clock_rollback_tolerance"`
	PricingClockForwardTolerance  Duration        `yaml:"pricing_clock_forward_tolerance"`
	PricingUnknownPolicy          string          `yaml:"pricing_unknown_policy"`
	SourceRateLimit               SourceRateLimit `yaml:"source_rate_limit"`
}

// defaultSourceRequestsPerMinute is the budget an absent
// gateway.source_rate_limit.requests_per_minute takes.
const defaultSourceRequestsPerMinute = 600

// Pricing clock tolerances an omitted gateway.pricing_clock_* key takes. Zero is
// not a usable value for either — a zero forward tolerance makes every priced
// attempt fail closed — so absence is filled rather than validated, the same way
// gateway.source_rate_limit is. Kept in step with Default() and default.yaml by
// TestDefaultTemplateMatchesDefault.
const (
	DefaultPricingClockRollbackTolerance = 2 * time.Second
	DefaultPricingClockForwardTolerance  = 30 * time.Second
)

// MinPricingClockRollbackTolerance is a floor, not a default: price selections
// on one deployment run concurrently, so they can reach their durable pin in the
// reverse of the order they captured pricing_selected_at, and the later one then
// reads a selection time behind the high-water mark. That backwards step is
// caused by this process, not by the clock. The tolerance must therefore stay
// above the span a selection can spend between capturing its time and committing
// its pin — batch delay plus fsync plus scheduling — or ordinary concurrency
// would quarantine a deployment for a wall-clock rollback that never happened.
// See ADR 0012, "Amendment 2026-08-07".
const MinPricingClockRollbackTolerance = time.Second

// SourceRateLimit bounds anonymous data-plane work per source address, ahead of
// the per-project limiter — which cannot apply until a request has been
// authenticated, and so cannot bound the cost of authenticating it.
type SourceRateLimit struct {
	// RequestsPerMinute is the per-source budget. A pointer so that "the key is
	// absent" and "the operator wrote 0" are different answers: absent takes the
	// default, because a security control that switches itself off for every
	// config file written before it existed protects nobody, and an explicit 0
	// disables the limiter for an operator who means it. Normalize resolves the
	// absent case, so everything downstream reads a value.
	RequestsPerMinute *int `yaml:"requests_per_minute"`
	// MaxTrackedSources caps distinct addresses remembered within one minute so
	// the limiter cannot itself be grown without bound. Addresses past the cap
	// share one budget.
	MaxTrackedSources int `yaml:"max_tracked_sources"`
}

type Retry struct {
	MaxAttemptsPerTarget int      `yaml:"max_attempts_per_target"`
	BaseDelay            Duration `yaml:"base_delay"`
	MaxDelay             Duration `yaml:"max_delay"`
	Jitter               bool     `yaml:"jitter"`
}

type CircuitBreaker struct {
	ConsecutiveFailures int      `yaml:"consecutive_failures"`
	OpenDuration        Duration `yaml:"open_duration"`
	HalfOpenMaxRequests int      `yaml:"half_open_max_requests"`
}

type Alerts struct {
	QueueCapacity int      `yaml:"queue_capacity"`
	Workers       int      `yaml:"workers"`
	Timeout       Duration `yaml:"timeout"`
	MaxAttempts   int      `yaml:"max_attempts"`
	BaseDelay     Duration `yaml:"base_delay"`
	MaxDelay      Duration `yaml:"max_delay"`
	DedupCooldown Duration `yaml:"dedup_cooldown"`
}

type Duration time.Duration

func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return errors.New("duration must be a scalar")
	}
	parsed, err := time.ParseDuration(value.Value)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Value() time.Duration {
	return time.Duration(d)
}

type Security struct {
	AllowPrivateProviderEndpoints bool     `yaml:"allow_private_provider_endpoints"`
	AllowPrivateWebhooks          bool     `yaml:"allow_private_webhooks"`
	TrustProxyHeaders             bool     `yaml:"trust_proxy_headers"`
	TrustedProxyCIDRs             []string `yaml:"trusted_proxy_cidrs"`
}

type Metrics struct {
	Enabled              bool       `yaml:"enabled"`
	RequireAuth          bool       `yaml:"require_auth"`
	CredentialFile       string     `yaml:"credential_file"`
	MaxConcurrentScrapes int        `yaml:"max_concurrent_scrapes"`
	WriteTimeout         Duration   `yaml:"write_timeout"`
	TLS                  MetricsTLS `yaml:"tls"`
}

type MetricsTLS struct {
	Enabled      bool   `yaml:"enabled"`
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	ClientCAFile string `yaml:"client_ca_file"`
}

// AuditAnchorSink identifies where anchors (ADR 0015) are sent. Only
// AuditAnchorSinkDeadManPull is implemented; the others are reserved names so
// a deployment's config does not need to change again when they land.
const (
	AuditAnchorSinkDeadManPull  = "dead_man_pull"
	AuditAnchorSinkSyslog       = "syslog"
	AuditAnchorSinkS3ObjectLock = "s3_object_lock"
)

type Audit struct {
	Anchor AuditAnchor `yaml:"anchor"`
}

type AuditAnchor struct {
	Enabled        bool     `yaml:"enabled"`
	Sink           string   `yaml:"sink"`
	Interval       Duration `yaml:"interval"`
	RecordDelta    int      `yaml:"record_delta"`
	CredentialFile string   `yaml:"credential_file"`
}

type LoadOptions struct {
	AllowInsecurePublicGateway bool
}

func Load(path string, opts LoadOptions) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	cfg, err := Decode(file)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Normalize(); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(opts); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Decode(r io.Reader) (Config, error) {
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("decode config: multiple YAML documents are not allowed")
		}
		return Config{}, fmt.Errorf("decode trailing config: %w", err)
	}
	return cfg, nil
}

func (c *Config) Normalize() error {
	var err error
	if c.Gateway.SourceRateLimit.RequestsPerMinute == nil {
		budget := defaultSourceRequestsPerMinute
		c.Gateway.SourceRateLimit.RequestsPerMinute = &budget
	}
	if c.Gateway.PricingClockRollbackTolerance == 0 {
		c.Gateway.PricingClockRollbackTolerance = Duration(DefaultPricingClockRollbackTolerance)
	}
	if c.Gateway.PricingClockForwardTolerance == 0 {
		c.Gateway.PricingClockForwardTolerance = Duration(DefaultPricingClockForwardTolerance)
	}
	if c.Gateway.SourceRateLimit.MaxTrackedSources == 0 {
		// Omitting the ceiling means "whatever is sane", not "track nothing".
		// Kept in step with sourcelimit.DefaultMaxTrackedSources by
		// TestSourceRateLimitCeilingMatchesLimiterDefault.
		c.Gateway.SourceRateLimit.MaxTrackedSources = 16384
	}
	for index := range c.Storage.MasterKey.AllowedKMSKeys {
		key := &c.Storage.MasterKey.AllowedKMSKeys[index]
		if key.Provider == "aws-kms" && key.Algorithm == "" {
			key.Algorithm = "SYMMETRIC_DEFAULT"
		}
		if strings.HasSuffix(key.Endpoint, "/") {
			key.Endpoint = strings.TrimSuffix(key.Endpoint, "/")
		}
	}
	c.Storage.DataDir, err = cleanAbsolutePath(c.Storage.DataDir)
	if err != nil {
		return fmt.Errorf("storage.data_dir: %w", err)
	}
	if c.Storage.MasterKey.File != "" {
		c.Storage.MasterKey.File, err = cleanAbsolutePath(c.Storage.MasterKey.File)
		if err != nil {
			return fmt.Errorf("storage.master_key.file: %w", err)
		}
	}
	if c.TLS.CertFile != "" {
		c.TLS.CertFile, err = cleanAbsolutePath(c.TLS.CertFile)
		if err != nil {
			return fmt.Errorf("tls.cert_file: %w", err)
		}
	}
	if c.TLS.KeyFile != "" {
		c.TLS.KeyFile, err = cleanAbsolutePath(c.TLS.KeyFile)
		if err != nil {
			return fmt.Errorf("tls.key_file: %w", err)
		}
	}
	for name, value := range map[string]*string{
		"metrics.credential_file":    &c.Metrics.CredentialFile,
		"metrics.tls.cert_file":      &c.Metrics.TLS.CertFile,
		"metrics.tls.key_file":       &c.Metrics.TLS.KeyFile,
		"metrics.tls.client_ca_file": &c.Metrics.TLS.ClientCAFile,
	} {
		if *value == "" {
			continue
		}
		*value, err = cleanAbsolutePath(*value)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if c.Retry.MaxAttemptsPerTarget == 0 {
		c.Retry.MaxAttemptsPerTarget = 2
	}
	if c.Retry.BaseDelay == 0 {
		c.Retry.BaseDelay = Duration(100 * time.Millisecond)
	}
	if c.Retry.MaxDelay == 0 {
		c.Retry.MaxDelay = Duration(2 * time.Second)
	}
	if c.CircuitBreaker.ConsecutiveFailures == 0 {
		c.CircuitBreaker.ConsecutiveFailures = 5
	}
	if c.Metrics.MaxConcurrentScrapes == 0 {
		c.Metrics.MaxConcurrentScrapes = 2
	}
	if c.Metrics.WriteTimeout == 0 {
		c.Metrics.WriteTimeout = Duration(5 * time.Second)
	}
	if c.Gateway.HealthProbeInterval == 0 {
		c.Gateway.HealthProbeInterval = Duration(30 * time.Second)
	}
	if c.CircuitBreaker.OpenDuration == 0 {
		c.CircuitBreaker.OpenDuration = Duration(30 * time.Second)
	}
	if c.CircuitBreaker.HalfOpenMaxRequests == 0 {
		c.CircuitBreaker.HalfOpenMaxRequests = 1
	}
	if c.Alerts.QueueCapacity == 0 {
		c.Alerts.QueueCapacity = 1024
	}
	if c.Alerts.Workers == 0 {
		c.Alerts.Workers = 2
	}
	if c.Alerts.Timeout == 0 {
		c.Alerts.Timeout = Duration(5 * time.Second)
	}
	if c.Alerts.MaxAttempts == 0 {
		c.Alerts.MaxAttempts = 3
	}
	if c.Alerts.BaseDelay == 0 {
		c.Alerts.BaseDelay = Duration(250 * time.Millisecond)
	}
	if c.Alerts.MaxDelay == 0 {
		c.Alerts.MaxDelay = Duration(5 * time.Second)
	}
	if c.Alerts.DedupCooldown == 0 {
		c.Alerts.DedupCooldown = Duration(time.Minute)
	}
	if c.Usage.WALQueueCapacity == 0 {
		c.Usage.WALQueueCapacity = 4096
	}
	if c.Usage.AnalyticsQueueCapacity == 0 {
		c.Usage.AnalyticsQueueCapacity = 4096
	}
	if c.Usage.WALMaxBatch == 0 {
		c.Usage.WALMaxBatch = 128
	}
	if c.Usage.WALFlushInterval == 0 {
		c.Usage.WALFlushInterval = Duration(2 * time.Millisecond)
	}
	if c.Usage.CheckpointInterval == 0 {
		c.Usage.CheckpointInterval = Duration(time.Minute)
	}
	if c.Usage.ParquetInterval == 0 {
		c.Usage.ParquetInterval = Duration(time.Hour)
	}
	if c.Usage.RetentionDays == 0 {
		c.Usage.RetentionDays = 90
	}
	if c.Usage.ExportFormat == "" {
		c.Usage.ExportFormat = UsageExportFormatParquet
	}
	if c.Admin.SessionTTL == 0 {
		c.Admin.SessionTTL = Duration(8 * time.Hour)
	}
	if c.Admin.IdleTimeout == 0 {
		c.Admin.IdleTimeout = Duration(30 * time.Minute)
	}
	if c.Admin.LoginRPM == 0 {
		c.Admin.LoginRPM = 5
	}
	if c.Admin.MFAPolicy == "" {
		c.Admin.MFAPolicy = "optional"
	}
	if c.Admin.DeveloperWorkbench == "" {
		c.Admin.DeveloperWorkbench = "enabled"
	}
	defaultDetection := Default().Admin.ModelCapabilityDetection
	if c.Admin.ModelCapabilityDetection.FreshTTL == 0 {
		c.Admin.ModelCapabilityDetection.FreshTTL = defaultDetection.FreshTTL
	}
	if c.Admin.ModelCapabilityDetection.Retention == 0 {
		c.Admin.ModelCapabilityDetection.Retention = defaultDetection.Retention
	}
	if c.Admin.ModelCapabilityDetection.RefreshCooldown == 0 {
		c.Admin.ModelCapabilityDetection.RefreshCooldown = defaultDetection.RefreshCooldown
	}
	if c.Admin.ModelCapabilityDetection.TotalTimeout == 0 {
		c.Admin.ModelCapabilityDetection.TotalTimeout = defaultDetection.TotalTimeout
	}
	if c.Admin.ModelCapabilityDetection.GlobalConcurrency == 0 {
		c.Admin.ModelCapabilityDetection.GlobalConcurrency = defaultDetection.GlobalConcurrency
	}
	if c.Admin.ModelCapabilityDetection.ProviderConcurrency == 0 {
		c.Admin.ModelCapabilityDetection.ProviderConcurrency = defaultDetection.ProviderConcurrency
	}
	if c.Admin.ModelCapabilityDetection.MaxProviderCalls == 0 {
		c.Admin.ModelCapabilityDetection.MaxProviderCalls = defaultDetection.MaxProviderCalls
	}
	if c.Admin.ModelCapabilityDetection.CreateRPM == 0 {
		c.Admin.ModelCapabilityDetection.CreateRPM = defaultDetection.CreateRPM
	}
	defaultCatalog := Default().ModelCatalog
	if c.ModelCatalog.RefreshInterval == 0 {
		c.ModelCatalog.RefreshInterval = defaultCatalog.RefreshInterval
	}
	if c.ModelCatalog.MaxDownloadBytes == 0 {
		c.ModelCatalog.MaxDownloadBytes = defaultCatalog.MaxDownloadBytes
	}
	if c.ModelCatalog.MaxDecodedBytes == 0 {
		c.ModelCatalog.MaxDecodedBytes = defaultCatalog.MaxDecodedBytes
	}
	if c.ModelCatalog.MaxCompressionRatio == 0 {
		c.ModelCatalog.MaxCompressionRatio = defaultCatalog.MaxCompressionRatio
	}
	if c.ModelCatalog.MaxEntries == 0 {
		c.ModelCatalog.MaxEntries = defaultCatalog.MaxEntries
	}
	return nil
}

func (c Config) Validate(opts LoadOptions) error {
	var problems []error
	if c.Version != SchemaVersion {
		problems = append(problems, fmt.Errorf("version must be %d", SchemaVersion))
	}
	if c.Storage.DataDir == "" {
		problems = append(problems, errors.New("storage.data_dir is required"))
	}
	problems = append(problems, validateMasterKey(c.Storage.MasterKey)...)
	if c.Storage.MetadataFile == "" || filepath.Base(c.Storage.MetadataFile) != c.Storage.MetadataFile {
		problems = append(problems, errors.New("storage.metadata_file must be a file name without path components"))
	}
	if c.ModelCatalog.RefreshInterval < Duration(5*time.Minute) || c.ModelCatalog.RefreshInterval > Duration(7*24*time.Hour) {
		problems = append(problems, errors.New("model_catalog.refresh_interval must be between 5 minutes and 7 days"))
	}
	if c.ModelCatalog.MaxDownloadBytes < 4096 || c.ModelCatalog.MaxDownloadBytes > 16<<20 {
		problems = append(problems, errors.New("model_catalog.max_download_bytes must be between 4096 and 16777216"))
	}
	if c.ModelCatalog.MaxDecodedBytes < c.ModelCatalog.MaxDownloadBytes || c.ModelCatalog.MaxDecodedBytes > 64<<20 {
		problems = append(problems, errors.New("model_catalog.max_decoded_bytes must be at least max_download_bytes and at most 67108864"))
	}
	if c.ModelCatalog.MaxCompressionRatio < 1 || c.ModelCatalog.MaxCompressionRatio > 100 {
		problems = append(problems, errors.New("model_catalog.max_compression_ratio must be between 1 and 100"))
	}
	if c.ModelCatalog.MaxEntries < 1 || c.ModelCatalog.MaxEntries > 100000 {
		problems = append(problems, errors.New("model_catalog.max_entries must be between 1 and 100000"))
	}
	if pin := c.ModelCatalog.PinnedRevision; pin != "" {
		if len(pin) != len("sha256:")+64 || !strings.HasPrefix(pin, "sha256:") {
			problems = append(problems, errors.New("model_catalog.pinned_revision must be a sha256 digest"))
		} else if _, err := hex.DecodeString(strings.TrimPrefix(pin, "sha256:")); err != nil {
			problems = append(problems, errors.New("model_catalog.pinned_revision must be a sha256 digest"))
		}
	}
	if c.TLS.Enabled {
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			problems = append(problems, errors.New("tls.cert_file and tls.key_file are required when TLS is enabled"))
		}
	} else if c.TLS.CertFile != "" || c.TLS.KeyFile != "" {
		problems = append(problems, errors.New("tls cert/key cannot be set while TLS is disabled"))
	}

	problems = append(problems, validateListener("server.gateway_listen", c.Server.GatewayListen, c.TLS.Enabled, opts.AllowInsecurePublicGateway)...)
	problems = append(problems, validateListener("server.admin_listen", c.Server.AdminListen, c.TLS.Enabled, false)...)
	if c.Metrics.Enabled {
		metricsTLSEnabled := c.Metrics.TLS.Enabled
		problems = append(problems, validateListener("server.metrics_listen", c.Server.MetricsListen, metricsTLSEnabled, false)...)
		metricsHost, _, metricsAddressErr := net.SplitHostPort(c.Server.MetricsListen)
		if metricsAddressErr == nil && !listenerHostIsLoopback(metricsHost) {
			if c.Metrics.CredentialFile == "" {
				problems = append(problems, errors.New("non-loopback metrics listener requires metrics.credential_file"))
			}
			if !c.Metrics.TLS.Enabled {
				problems = append(problems, errors.New("non-loopback metrics listener requires dedicated metrics.tls mutual authentication"))
			}
		}
		if c.Metrics.CredentialFile != "" && !c.Metrics.RequireAuth {
			problems = append(problems, errors.New("metrics.credential_file requires metrics.require_auth"))
		}
		if c.Metrics.MaxConcurrentScrapes < 1 || c.Metrics.MaxConcurrentScrapes > 32 {
			problems = append(problems, errors.New("metrics.max_concurrent_scrapes must be between 1 and 32"))
		}
		if c.Metrics.WriteTimeout <= 0 || c.Metrics.WriteTimeout > Duration(30*time.Second) {
			problems = append(problems, errors.New("metrics.write_timeout must be between zero and 30 seconds"))
		}
		if c.Metrics.TLS.Enabled {
			if c.Metrics.TLS.CertFile == "" || c.Metrics.TLS.KeyFile == "" || c.Metrics.TLS.ClientCAFile == "" {
				problems = append(problems, errors.New("metrics.tls cert_file, key_file, and client_ca_file are required when enabled"))
			}
		} else if c.Metrics.TLS.CertFile != "" || c.Metrics.TLS.KeyFile != "" || c.Metrics.TLS.ClientCAFile != "" {
			problems = append(problems, errors.New("metrics.tls files cannot be set while metrics.tls is disabled"))
		}
	}
	if c.Audit.Anchor.Enabled {
		switch c.Audit.Anchor.Sink {
		case AuditAnchorSinkDeadManPull:
			// The anchor-pull endpoint is served on the metrics listener
			// (ADR 0015): it is already an independent port with a
			// bearer/mTLS story a probe-style caller needs, and standing up
			// a second listener for one more endpoint would duplicate that
			// story rather than reuse it.
			if !c.Metrics.Enabled {
				problems = append(problems, errors.New("audit.anchor.sink dead_man_pull requires metrics.enabled"))
			}
			if c.Audit.Anchor.CredentialFile == "" {
				problems = append(problems, errors.New("audit.anchor.credential_file is required for sink dead_man_pull"))
			}
			// The anchor exists so a witness outside this host can contradict
			// it. Sharing one credential with /metrics collapses that into one
			// domain: whoever scrapes metrics can read the witness feed, and a
			// leak on the scrape path takes the witness with it. The code
			// already keeps two authorizers; nothing until now kept two files.
			if c.Audit.Anchor.CredentialFile != "" && c.Audit.Anchor.CredentialFile == c.Metrics.CredentialFile {
				problems = append(problems, errors.New("audit.anchor.credential_file must differ from metrics.credential_file; the anchor is a separate credential domain"))
			}
			// Anchors and the token that fetches them are the evidence of
			// non-repudiation. Serving them in the clear lets anyone on the
			// path read the chain heads and take the credential.
			if !c.Metrics.TLS.Enabled {
				problems = append(problems, errors.New("audit.anchor.sink dead_man_pull requires metrics.tls.enabled; the anchor feed must not be served in the clear"))
			}
		case AuditAnchorSinkSyslog, AuditAnchorSinkS3ObjectLock:
			problems = append(problems, fmt.Errorf("audit.anchor.sink %q is a reserved name and not implemented yet", c.Audit.Anchor.Sink))
		default:
			problems = append(problems, fmt.Errorf("audit.anchor.sink %q is not a recognized sink", c.Audit.Anchor.Sink))
		}
		if c.Audit.Anchor.Interval <= 0 || c.Audit.Anchor.Interval > Duration(time.Hour) {
			problems = append(problems, errors.New("audit.anchor.interval must be between zero and one hour"))
		}
		if c.Audit.Anchor.RecordDelta < 1 {
			problems = append(problems, errors.New("audit.anchor.record_delta must be at least 1"))
		}
	}
	listeners := map[string]string{
		"gateway": c.Server.GatewayListen,
		"admin":   c.Server.AdminListen,
	}
	if c.Metrics.Enabled {
		listeners["metrics"] = c.Server.MetricsListen
	}
	for leftName, leftAddress := range listeners {
		for rightName, rightAddress := range listeners {
			if leftName < rightName && leftAddress == rightAddress {
				problems = append(problems, fmt.Errorf("server %s and %s listeners must be distinct", leftName, rightName))
			}
		}
	}

	if c.Server.ReadHeaderTimeout <= 0 {
		problems = append(problems, errors.New("server.read_header_timeout must be positive"))
	}
	if c.Server.ReadBodyTimeout <= 0 {
		problems = append(problems, errors.New("server.read_body_timeout must be positive"))
	}
	if c.Server.MaxHeaderBytes < 1024 {
		problems = append(problems, errors.New("server.max_header_bytes must be at least 1024"))
	}
	if c.Server.MaxRequestBytes <= 0 {
		problems = append(problems, errors.New("server.max_request_bytes must be positive"))
	}
	for name, value := range map[string]Duration{
		"gateway.route_total_timeout":             c.Gateway.RouteTotalTimeout,
		"gateway.attempt_connect_timeout":         c.Gateway.AttemptConnectTimeout,
		"gateway.attempt_response_header_timeout": c.Gateway.AttemptResponseHeaderTimeout,
		"gateway.stream_idle_timeout":             c.Gateway.StreamIdleTimeout,
		"gateway.downstream_write_timeout":        c.Gateway.DownstreamWriteTimeout,
		"gateway.stream_max_duration":             c.Gateway.StreamMaxDuration,
		"gateway.health_probe_interval":           c.Gateway.HealthProbeInterval,
	} {
		if value <= 0 {
			problems = append(problems, fmt.Errorf("%s must be positive", name))
		}
	}
	if c.Gateway.PricingClockRollbackTolerance.Value() < MinPricingClockRollbackTolerance {
		problems = append(problems, fmt.Errorf("gateway.pricing_clock_rollback_tolerance must be at least %s, so concurrent price selections cannot be mistaken for a wall-clock rollback", MinPricingClockRollbackTolerance))
	}
	if c.Gateway.PricingClockForwardTolerance < 0 {
		problems = append(problems, errors.New("gateway.pricing_clock_forward_tolerance cannot be negative"))
	}
	if c.Gateway.PricingUnknownPolicy != "" && c.Gateway.PricingUnknownPolicy != "reject" &&
		c.Gateway.PricingUnknownPolicy != "allow_without_cost_governance" {
		problems = append(problems, errors.New("gateway.pricing_unknown_policy must be reject or allow_without_cost_governance"))
	}
	if c.Gateway.MaxTotalAttempts < 1 {
		problems = append(problems, errors.New("gateway.max_total_attempts must be at least 1"))
	}
	if c.Gateway.SourceRateLimit.RequestsPerMinute != nil && *c.Gateway.SourceRateLimit.RequestsPerMinute < 0 {
		problems = append(problems, errors.New("gateway.source_rate_limit.requests_per_minute cannot be negative"))
	}
	if c.Gateway.SourceRateLimit.MaxTrackedSources < 0 {
		problems = append(problems, errors.New("gateway.source_rate_limit.max_tracked_sources cannot be negative"))
	}
	if c.Retry.MaxAttemptsPerTarget < 1 {
		problems = append(problems, errors.New("retry.max_attempts_per_target must be at least 1"))
	}
	if c.Retry.BaseDelay <= 0 || c.Retry.MaxDelay < c.Retry.BaseDelay {
		problems = append(problems, errors.New("retry delays must be positive and max_delay must be at least base_delay"))
	}
	if c.CircuitBreaker.ConsecutiveFailures < 1 || c.CircuitBreaker.OpenDuration <= 0 ||
		c.CircuitBreaker.HalfOpenMaxRequests < 1 {
		problems = append(problems, errors.New("circuit_breaker values must be positive"))
	}
	if c.Alerts.QueueCapacity < 1 || c.Alerts.Workers < 1 || c.Alerts.Timeout <= 0 ||
		c.Alerts.MaxAttempts < 1 || c.Alerts.BaseDelay <= 0 ||
		c.Alerts.MaxDelay < c.Alerts.BaseDelay || c.Alerts.DedupCooldown <= 0 {
		problems = append(problems, errors.New("alerts queue, workers, timeout, attempts, delays, and cooldown must be positive"))
	}
	if c.Usage.Durability != "strict" && c.Usage.Durability != "balanced" {
		problems = append(problems, errors.New("usage.durability must be strict or balanced"))
	}
	if c.Usage.WALQueueCapacity < 1 || c.Usage.AnalyticsQueueCapacity < 1 || c.Usage.WALMaxBatch < 1 ||
		c.Usage.WALMaxBatch > c.Usage.WALQueueCapacity ||
		c.Usage.WALFlushInterval <= 0 {
		problems = append(problems, errors.New(
			"usage WAL queue, max batch, and flush interval must be positive and max batch cannot exceed queue capacity",
		))
	}
	if c.Usage.Timezone == "" {
		problems = append(problems, errors.New("usage.timezone is required"))
	} else if _, err := time.LoadLocation(c.Usage.Timezone); err != nil {
		problems = append(problems, fmt.Errorf("usage.timezone: %w", err))
	}
	if c.Usage.CheckpointInterval <= 0 {
		problems = append(problems, errors.New("usage.checkpoint_interval must be positive"))
	}
	if c.Usage.ParquetInterval <= 0 {
		problems = append(problems, errors.New("usage.parquet_interval must be positive"))
	}
	if c.Usage.RetentionDays < 1 {
		problems = append(problems, errors.New("usage.retention_days must be at least 1"))
	}
	if c.Usage.ExportFormat != UsageExportFormatParquet && c.Usage.ExportFormat != UsageExportFormatNDJSON {
		problems = append(problems, errors.New("usage.export_format must be parquet or ndjson"))
	}
	if c.Admin.SessionTTL <= 0 || c.Admin.IdleTimeout <= 0 ||
		c.Admin.IdleTimeout > c.Admin.SessionTTL || c.Admin.LoginRPM < 1 {
		problems = append(problems, errors.New(
			"admin session TTL, idle timeout, and login RPM must be positive; idle timeout cannot exceed TTL",
		))
	}
	if c.Admin.DeveloperWorkbench != "enabled" && c.Admin.DeveloperWorkbench != "disabled" {
		return errors.New("admin.developer_workbench must be enabled or disabled")
	}
	if c.Admin.MFAPolicy != "optional" && c.Admin.MFAPolicy != "required" {
		return errors.New("admin.mfa_policy must be optional or required")
	}
	detection := c.Admin.ModelCapabilityDetection
	if detection.FreshTTL <= 0 || detection.Retention < detection.FreshTTL || detection.RefreshCooldown <= 0 ||
		detection.TotalTimeout <= 0 || detection.TotalTimeout > Duration(2*time.Minute) ||
		detection.GlobalConcurrency < 1 || detection.ProviderConcurrency < 1 ||
		detection.ProviderConcurrency > detection.GlobalConcurrency || detection.MaxProviderCalls < 1 || detection.MaxProviderCalls > 8 ||
		detection.CreateRPM < 1 || detection.CreateRPM > 60 {
		problems = append(problems, errors.New("admin.model_capability_detection limits are invalid"))
	}
	if c.Admin.ExternalOrigin != "" {
		origin, err := url.Parse(c.Admin.ExternalOrigin)
		if err != nil || origin.Scheme != "https" || origin.Host == "" ||
			origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
			problems = append(problems, errors.New(
				"admin.external_origin must be an HTTPS origin without path, query, or fragment",
			))
		}
	}
	for _, raw := range c.Security.TrustedProxyCIDRs {
		if _, err := netip.ParsePrefix(raw); err != nil {
			problems = append(problems, fmt.Errorf("security.trusted_proxy_cidrs %q: %w", raw, err))
		}
	}
	if c.Security.TrustProxyHeaders && len(c.Security.TrustedProxyCIDRs) == 0 {
		problems = append(problems, errors.New("security.trusted_proxy_cidrs is required when proxy headers are trusted"))
	}
	return errors.Join(problems...)
}

func (c Config) MetadataPath() string {
	return filepath.Join(c.Storage.DataDir, c.Storage.MetadataFile)
}

func (c Config) LedgerPath() string {
	return filepath.Join(c.Storage.DataDir, "ledger", "ledger.wal")
}

func (c Config) UsagePath() string {
	return filepath.Join(c.Storage.DataDir, "usage")
}

func (c Config) AuditPath() string {
	return filepath.Join(c.Storage.DataDir, "audit", "audit.log")
}

func validateMasterKey(masterKey MasterKey) []error {
	var problems []error
	switch masterKey.Mode {
	case MasterKeyModeFile:
		if masterKey.File == "" {
			problems = append(problems, errors.New("storage.master_key.file is required in file mode"))
		}
		if masterKey.PrimarySlot != "" || masterKey.RecoverySlot != "" ||
			masterKey.StartupDeadline != 0 || masterKey.CallTimeout != 0 || len(masterKey.AllowedKMSKeys) != 0 {
			problems = append(problems, errors.New("storage.master_key key_slots fields cannot be set in file mode"))
		}
	case MasterKeyModeKeySlots:
		if masterKey.File != "" {
			problems = append(problems, errors.New("storage.master_key.file cannot be set in key_slots mode"))
		}
		if strings.TrimSpace(masterKey.PrimarySlot) == "" || strings.TrimSpace(masterKey.RecoverySlot) == "" {
			problems = append(problems, errors.New("storage.master_key primary_slot and recovery_slot are required in key_slots mode"))
		} else if masterKey.PrimarySlot == masterKey.RecoverySlot {
			problems = append(problems, errors.New("storage.master_key primary_slot and recovery_slot must be different"))
		}
		if masterKey.StartupDeadline <= 0 {
			problems = append(problems, errors.New("storage.master_key.startup_deadline must be positive in key_slots mode"))
		}
		if masterKey.CallTimeout <= 0 || masterKey.CallTimeout >= masterKey.StartupDeadline {
			problems = append(problems, errors.New("storage.master_key.call_timeout must be positive and less than startup_deadline"))
		}
		problems = append(problems, validateAllowedKMSKeys(masterKey.AllowedKMSKeys)...)
	default:
		problems = append(problems, errors.New("storage.master_key.mode must be file or key_slots"))
	}
	return problems
}

func validateAllowedKMSKeys(keys []AllowedKMSKey) []error {
	var problems []error
	purposes := map[string]bool{"primary": false, "recovery": false}
	identities := make(map[string]string, len(keys))
	for index, key := range keys {
		prefix := fmt.Sprintf("storage.master_key.allowed_kms_keys[%d]", index)
		if _, ok := purposes[key.Purpose]; !ok {
			problems = append(problems, fmt.Errorf("%s.purpose must be primary or recovery", prefix))
		} else {
			purposes[key.Purpose] = true
		}
		if strings.TrimSpace(key.Provider) == "" {
			problems = append(problems, fmt.Errorf("%s.provider is required", prefix))
		} else if key.Provider != "aws-kms" {
			problems = append(problems, fmt.Errorf("%s.provider is not available in this release", prefix))
		}
		if strings.TrimSpace(key.KeyID) == "" {
			problems = append(problems, fmt.Errorf("%s.key_id is required", prefix))
		}
		if key.Provider == "aws-kms" {
			partition, region, account, resource, ok := parseAWSKMSKeyARN(key.KeyID)
			if !ok || !strings.HasPrefix(partition, "aws") || region != key.Region || account != key.Account ||
				!strings.HasPrefix(resource, "key/") || strings.TrimPrefix(resource, "key/") == "" {
				problems = append(problems, fmt.Errorf("%s.key_id must be a full KMS Key ARN matching region and account", prefix))
			}
			if !validAWSRegion(key.Region) {
				problems = append(problems, fmt.Errorf("%s.region is invalid", prefix))
			}
			if !validAWSAccount(key.Account) {
				problems = append(problems, fmt.Errorf("%s.account must contain exactly 12 digits", prefix))
			}
			if key.Algorithm != "" && key.Algorithm != "SYMMETRIC_DEFAULT" {
				problems = append(problems, fmt.Errorf("%s.algorithm must be SYMMETRIC_DEFAULT", prefix))
			}
		}
		if key.Endpoint != "" {
			endpoint, err := url.Parse(key.Endpoint)
			if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
				(endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawQuery != "" || endpoint.Fragment != "" {
				problems = append(problems, fmt.Errorf("%s.endpoint must be an HTTPS origin without userinfo, path, query, or fragment", prefix))
			}
		}
		identity := strings.Join([]string{key.Provider, key.Region, key.Account, key.KeyID}, "\x00")
		if previousPurpose, exists := identities[identity]; exists && previousPurpose != key.Purpose {
			problems = append(problems, errors.New("storage.master_key primary and recovery allowlists must not use the same KMS key"))
		} else {
			identities[identity] = key.Purpose
		}
	}
	for purpose, present := range purposes {
		if !present {
			problems = append(problems, fmt.Errorf("storage.master_key.allowed_kms_keys requires a %s entry", purpose))
		}
	}
	return problems
}

func parseAWSKMSKeyARN(value string) (partition, region, account, resource string, ok bool) {
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "kms" {
		return "", "", "", "", false
	}
	return parts[1], parts[3], parts[4], parts[5], true
}

func validAWSRegion(value string) bool {
	if len(value) < 3 || len(value) > 63 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return value[0] != '-' && value[len(value)-1] != '-'
}

func validAWSAccount(value string) bool {
	if len(value) != 12 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func cleanAbsolutePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func validateListener(name, address string, tlsEnabled, allowInsecurePublic bool) []error {
	if address == "" {
		return []error{fmt.Errorf("%s is required", name)}
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return []error{fmt.Errorf("%s: %w", name, err)}
	}
	if tlsEnabled || listenerHostIsLoopback(host) {
		return nil
	}
	if allowInsecurePublic && name == "server.gateway_listen" {
		return nil
	}
	return []error{fmt.Errorf("%s must bind loopback unless TLS is enabled", name)}
}

// ListenerHostIsLoopback reports whether a listener host is reachable only from
// this machine. Exported because deployment warnings elsewhere need the same
// answer this package's validation uses, and two implementations would drift.
func ListenerHostIsLoopback(host string) bool { return listenerHostIsLoopback(host) }

func listenerHostIsLoopback(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

func intPointer(value int) *int { return &value }

// SourceRequestsPerMinute is the resolved per-source budget. Normalize fills the
// absent case, so callers never have to decide what a missing key meant.
func (s SourceRateLimit) SourceRequestsPerMinute() int {
	if s.RequestsPerMinute == nil {
		return defaultSourceRequestsPerMinute
	}
	return *s.RequestsPerMinute
}
