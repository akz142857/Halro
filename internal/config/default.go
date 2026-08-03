package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Default returns the safe, loopback-only configuration used by `heimdall start`.
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
			MaxHeaderBytes:    32768,
			MaxRequestBytes:   10 << 20,
		},
		Storage: Storage{
			DataDir:      "./data",
			MetadataFile: "heimdall.db",
			MasterKey: MasterKey{
				Mode: MasterKeyModeFile,
				File: "./master.key",
			},
		},
		Admin: Admin{
			SessionTTL:  Duration(8 * time.Hour),
			IdleTimeout: Duration(30 * time.Minute),
			LoginRPM:    5,
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
		},
		Gateway: Gateway{
			RouteTotalTimeout:            Duration(2 * time.Minute),
			AttemptConnectTimeout:        Duration(5 * time.Second),
			AttemptResponseHeaderTimeout: Duration(time.Minute),
			StreamIdleTimeout:            Duration(time.Minute),
			DownstreamWriteTimeout:       Duration(15 * time.Second),
			StreamMaxDuration:            Duration(10 * time.Minute),
			MaxTotalAttempts:             3,
			HealthProbeInterval:          Duration(30 * time.Second),
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
	temporary, err := os.CreateTemp(directory, ".heimdall-config-*")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect temporary config: %w", err)
	}
	encoder := yaml.NewEncoder(temporary)
	encoder.SetIndent(2)
	encodeErr := encoder.Encode(Default())
	closeEncoderErr := encoder.Close()
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if err := errors.Join(encodeErr, closeEncoderErr, syncErr, closeErr); err != nil {
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
