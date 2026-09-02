package config

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// defaultTemplate is what `halro start` writes when no config exists. It is
// the annotated form of Default(): the first file an operator ever opens should
// explain what it is offering to change, not hand them a marshalled struct.
// TestDefaultTemplateMatchesDefault keeps the two from drifting.
//
//go:embed default.yaml
var defaultTemplate []byte

// Default returns the safe, loopback-only configuration used by `halro start`.
// Relative storage paths intentionally remain relative to the process working
// directory, matching the existing config loading semantics.
func Default() Config {
	return Config{
		Version: SchemaVersion,
		Server: Server{
			GatewayListen:     "127.0.0.1:8080",
			AdminListen:       "127.0.0.1:8081",
			MetricsListen:     "127.0.0.1:9090",
			ReadHeaderTimeout: Duration(5 * time.Second),
			ReadBodyTimeout:   Duration(15 * time.Second),
			ShutdownTimeout:   Duration(2 * time.Minute),
			MaxHeaderBytes:    32768,
			MaxRequestBytes:   10 << 20,
		},
		Storage: Storage{
			DataDir:      "./data",
			MetadataFile: "halro.db",
			MasterKey: MasterKey{
				Mode: MasterKeyModeFile,
				File: "./master.key",
			},
		},
		Admin: Admin{
			SessionTTL:            Duration(8 * time.Hour),
			IdleTimeout:           Duration(30 * time.Minute),
			LoginRPM:              5,
			MFAPolicy:             "optional",
			DeveloperWorkbench:    "enabled",
			ReauthElevationWindow: durationPointer(defaultReauthElevationWindow),
			ModelCapabilityDetection: ModelCapabilityDetection{
				FreshTTL: Duration(24 * time.Hour), Retention: Duration(30 * 24 * time.Hour),
				RefreshCooldown: Duration(5 * time.Minute), TotalTimeout: Duration(90 * time.Second),
				GlobalConcurrency: 4, ProviderConcurrency: 1, MaxProviderCalls: 10, CreateRPM: 6,
			},
		},
		Usage: Usage{
			Durability:             "balanced",
			Timezone:               "UTC",
			WALQueueCapacity:       4096,
			WALMaxBatch:            128,
			WALFlushInterval:       Duration(2 * time.Millisecond),
			AnalyticsQueueCapacity: 4096,
			CheckpointInterval:     Duration(time.Minute),
			ParquetInterval:        Duration(time.Hour),
			RetentionDays:          90,
			ConsoleWindowDays:      DefaultConsoleWindowDays,
			ExportFormat:           UsageExportFormatParquet,
		},
		Gateway: Gateway{
			RouteTotalTimeout:             Duration(2 * time.Minute),
			AttemptConnectTimeout:         Duration(5 * time.Second),
			AttemptResponseHeaderTimeout:  Duration(time.Minute),
			DownstreamWriteTimeout:        Duration(15 * time.Second),
			StreamMaxDuration:             Duration(10 * time.Minute),
			MaxTotalAttempts:              3,
			HealthProbeInterval:           Duration(30 * time.Second),
			PricingClockRollbackTolerance: Duration(DefaultPricingClockRollbackTolerance),
			PricingClockForwardTolerance:  Duration(DefaultPricingClockForwardTolerance),
			PricingUnknownPolicy:          "reject",
			SourceRateLimit: SourceRateLimit{
				// Generous enough that a busy application never notices, small
				// enough that one address cannot occupy the gateway. This
				// package stays free of internal imports, so the tracking
				// ceiling is repeated from sourcelimit.DefaultMaxTrackedSources
				// rather than referenced.
				RequestsPerMinute: intPointer(defaultSourceRequestsPerMinute),
				MaxTrackedSources: 16384,
			},
			// Off, with its bounds already set. Turning it on is a decision
			// about what this instance's data directory holds — it is the only
			// store that keeps material a caller wrote — and that decision
			// should be one boolean, not a block composed during the incident
			// that made someone want it.
			FailureCapture: FailureCapture{
				Enabled:          false,
				MaxBytes:         DefaultFailureCaptureMaxBytes,
				MaxRecordsPerDay: DefaultFailureCaptureMaxRecordsPerDay,
				Retain:           Duration(DefaultFailureCaptureRetain),
			},
		},
		Retry: Retry{
			MaxAttemptsPerTarget: 2,
			BaseDelay:            Duration(100 * time.Millisecond),
			MaxDelay:             Duration(2 * time.Second),
			Jitter:               true,
		},
		CircuitBreaker: CircuitBreaker{
			ConsecutiveFailures: 5,
			OpenDuration:        Duration(30 * time.Second),
			HalfOpenMaxRequests: 1,
		},
		Alerts: Alerts{
			QueueCapacity: 1024,
			Workers:       2,
			Timeout:       Duration(5 * time.Second),
			MaxAttempts:   3,
			BaseDelay:     Duration(250 * time.Millisecond),
			MaxDelay:      Duration(5 * time.Second),
			DedupCooldown: Duration(time.Minute),
		},
		Metrics: Metrics{
			Enabled: true, RequireAuth: true, MaxConcurrentScrapes: 2,
			WriteTimeout: Duration(5 * time.Second),
		},
		Audit: Audit{
			Anchor: AuditAnchor{
				// Off by default: enabling it takes a deliberate credential
				// file and a dead-man probe configured to pull it. mode:file
				// deployments get a startup warning instead (ADR 0015),
				// which is enough to put the decision in front of an
				// operator without breaking the out-of-the-box default.
				Enabled: false, Sink: AuditAnchorSinkDeadManPull,
				Interval: Duration(5 * time.Minute), RecordDelta: 500,
			},
		},
		ModelCatalog: ModelCatalog{
			Enabled: false, RefreshInterval: Duration(6 * time.Hour),
			MaxDownloadBytes: 1 << 20, MaxDecodedBytes: 4 << 20,
			MaxCompressionRatio: 20, MaxEntries: 10_000,
		},
		Providers: Providers{Bedrock: BedrockProvider{Region: DefaultBedrockRegion}},
		Logging: Logging{
			Level: "info", Format: LogFormatJSON, Output: LogOutputStderr,
			MaxSizeMB: 64, MaxFiles: 5,
			// Off by default, with its limits already set: switching it on is
			// one boolean rather than a block an operator has to compose
			// during the incident that made them want it.
			ErrorFile: ErrorFile{Enabled: false, MaxSizeMB: 32, MaxFiles: 10},
		},
	}
}

// WriteDefault publishes a default config without replacing an existing file.
// A hard link gives us create-if-absent semantics while keeping the write atomic.
func WriteDefault(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	directory := filepath.Dir(absolute)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".halro-config-*")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect temporary config: %w", err)
	}
	_, writeErr := temporary.Write(defaultTemplate)
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write default config: %w", err)
	}
	if err := os.Link(temporaryPath, absolute); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return fmt.Errorf("publish default config: %w", err)
	}
	return nil
}
