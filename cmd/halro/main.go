package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	// Embeds the IANA database so zone resolution is a property of this binary
	// rather than of whatever the host happens to ship. Daily budget periods are
	// derived from these rules; nodes disagreeing on them would place the same
	// instant in different periods without anything surfacing the divergence.
	_ "time/tzdata"

	"github.com/akz142857/Halro/internal/app"
	backuppkg "github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/bearercred"
	"github.com/akz142857/Halro/internal/buildinfo"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/hostsecurity"
	"github.com/akz142857/Halro/internal/masterkey"
	"github.com/akz142857/Halro/internal/safelog"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	storelock "github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/timezone"
)

func main() {
	logger := safelog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(os.Args[1:], logger); err != nil && !errors.Is(err, flag.ErrHelp) {
		reportCommandFailure(os.Stderr, err)
		os.Exit(1)
	}
}

// reportCommandFailure writes the exit reason for someone at a terminal. A
// running instance keeps its structured logs, but this is the command's verdict
// — and config validation reports every problem it found in one error, so
// routing it through the JSON handler flattened twenty findings into a single
// line of escaped newlines. That threw away the one thing the validator does
// unusually well.
//
// Redaction still applies: it came free with the logger, and an error can quote
// a value from the configuration it was reading.
func reportCommandFailure(out io.Writer, err error) {
	problems := strings.Split(strings.TrimSpace(safelog.Redact(err.Error())), "\n")
	if len(problems) == 1 {
		fmt.Fprintf(out, "halro: %s\n", problems[0])
		return
	}
	fmt.Fprintf(out, "halro: %d problems found:\n", len(problems))
	for _, problem := range problems {
		fmt.Fprintf(out, "  - %s\n", strings.TrimSpace(problem))
	}
}

// usageRetentionCutoff is the oldest partition date a prune may keep.
//
// Partitions are dated in UTC while a retention promise is read in the
// operator's own day. An instance east of UTC reaches its local "N days ago"
// while the UTC partition for that day is still current, so pruning at exactly
// N would delete a day the operator was told they still had. retention_days is
// therefore a floor — at least N days — bought with one extra partition of
// storage.
func usageRetentionCutoff(now time.Time, retentionDays int) time.Time {
	return now.UTC().AddDate(0, 0, -(retentionDays + 1))
}

// versionReport extends the build stamp with the time zone rules this process
// resolves against. Both belong to the same question — "which artefact is this
// node running" — and tzdata drift between nodes is otherwise invisible.
func versionReport() map[string]any {
	report := map[string]any{"build": buildinfo.Current()}
	database, err := timezone.Describe()
	if err != nil {
		report["tzdata"] = map[string]string{"error": err.Error()}
		return report
	}
	report["tzdata"] = database
	return report
}

// printMasterKeyCustodyNotice is the one moment this can usefully be said. The
// key was just created, it is not in the encrypted backups, and losing it makes
// every stored provider credential permanently unrecoverable — but until now
// the first run said nothing about it at all, and the place that explains it
// properly is a document the operator has no reason to open yet. The risk
// window opens in the first minute; the warning belongs there.
func printMasterKeyCustodyNotice(out io.Writer, cfg config.Config) {
	if cfg.Storage.MasterKey.Mode != config.MasterKeyModeFile {
		return
	}
	location := cfg.Storage.MasterKey.File
	if absolute, err := filepath.Abs(location); err == nil {
		location = absolute
	}
	fmt.Fprintf(out, `
  Master key created at %s (permissions 0600).

  Every provider credential is encrypted with it. Encrypted backups do NOT
  contain it, by design, so a backup alone cannot restore this instance.
  Copy it somewhere durable and separate from the data directory now; if it
  is lost, the credentials it protects cannot be recovered by any means.

  See docs/guides/backup-restore.md.

`, location)
}

var (
	initializeCommand      = app.Initialize
	verifyRecoveryCommand  = app.VerifyRecoverySlot
	rotateKMSCommand       = app.RotateKMSMasterKey
	rewrapKMSCommand       = app.RewrapKMSKey
	revokeKMSCommand       = app.RevokeKMSKeySlot
	inspectKMSSlotsCommand = app.InspectKMSKeySlots
	doctorCommand          = app.DoctorWithOptions
	restoreBackupCommand   = app.RestoreBackupWithOptions
	hardenRuntimeCommand   = hostsecurity.Harden
)

const recoveryNextStepMessage = "Recovery Slot verified and audited; rewrap and verify a replacement Primary first, then revoke temporary AWS recovery authorization before cold start."

func run(arguments []string, logger *slog.Logger) error {
	if len(arguments) == 0 {
		return errors.New("usage: halro <start|init|bootstrap|admin|key|backup|restore|pricing|usage|audit|metrics|stats|doctor|serve|healthcheck|config|version>")
	}
	switch arguments[0] {
	case "pricing":
		if len(arguments) < 2 || arguments[1] != "migrate" {
			return errors.New("usage: halro pricing migrate --dry-run --report <path> | --resolution-file <path> --apply")
		}
		flags := flag.NewFlagSet("pricing migrate", flag.ContinueOnError)
		configPath := flags.String("config", "config.yaml", "configuration file")
		dryRun := flags.Bool("dry-run", false, "inspect migration readiness without writing")
		reportPath := flags.String("report", "", "write the migration report as JSON")
		resolutionPath := flags.String("resolution-file", "", "versioned JSON resolution file")
		apply := flags.Bool("apply", false, "apply the migration from a staging copy")
		if err := flags.Parse(arguments[2:]); err != nil {
			return err
		}
		cfg, err := config.Load(*configPath, config.LoadOptions{})
		if err != nil {
			return err
		}
		metadataPath := filepath.Join(cfg.Storage.DataDir, cfg.Storage.MetadataFile)
		offlineLock, err := storelock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			return fmt.Errorf("pricing migration requires an offline data directory: %w", err)
		}
		defer offlineLock.Close()
		if *dryRun {
			if *apply || *resolutionPath != "" || *reportPath == "" {
				return errors.New("dry-run requires --report and cannot be combined with --apply")
			}
			report, err := boltstore.DryRunPricingMigration(context.Background(), metadataPath)
			if err != nil {
				return err
			}
			payload, err := json.MarshalIndent(report, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(*reportPath, append(payload, '\n'), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "Pricing migration report written to %s; unresolved enabled deployments: %d\n", *reportPath, report.UnresolvedEnabled)
			return nil
		}
		if !*apply || *resolutionPath == "" || *reportPath != "" {
			return errors.New("apply requires --resolution-file and --apply")
		}
		payload, err := os.ReadFile(*resolutionPath)
		if err != nil {
			return err
		}
		var resolutions boltstore.PricingMigrationResolutionFile
		if err := json.Unmarshal(payload, &resolutions); err != nil {
			return fmt.Errorf("decode pricing migration resolution file: %w", err)
		}
		backupPath, err := boltstore.ApplyPricingMigrationDataDir(context.Background(), cfg.Storage.DataDir, cfg.Storage.MetadataFile, resolutions)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "Pricing migration applied; rollback metadata retained at %s\n", backupPath)
		return nil
	case "start":
		flags := flag.NewFlagSet("start", flag.ContinueOnError)
		configPath := flags.String("config", "config.yaml", "configuration file")
		allowInsecure := flags.Bool("allow-insecure-public-listen", false, "allow only the Gateway listener to bind public plaintext")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if _, err := os.Stat(*configPath); errors.Is(err, os.ErrNotExist) {
			if err := config.WriteDefault(*configPath); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			fmt.Fprintf(os.Stdout, "Created safe local configuration at %s\n", *configPath)
		} else if err != nil {
			return fmt.Errorf("inspect config: %w", err)
		}
		cfg, err := config.Load(*configPath, config.LoadOptions{AllowInsecurePublicGateway: *allowInsecure})
		if err != nil {
			return err
		}
		if *allowInsecure {
			logger.Warn("insecure public Gateway override enabled")
		}
		initialized, err := app.InitializeIfNeeded(cfg)
		if err != nil {
			return err
		}
		if initialized {
			fmt.Fprintln(os.Stdout, "Initialized Halro system storage")
			printMasterKeyCustodyNotice(os.Stdout, cfg)
		}
		return runRuntime(cfg, logger, true)
	case "stats":
		flags := flag.NewFlagSet("stats", flag.ContinueOnError)
		configPath := flags.String("config", "config.yaml", "configuration file")
		statsURL := flags.String("url", "", "metrics URL (defaults to the configured metrics listener over loopback)")
		interval := flags.Duration("interval", 0, "sample twice this far apart and report the window instead of the lifetime average")
		timeout := flags.Duration("timeout", 5*time.Second, "metrics request timeout")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		cfg, err := config.Load(*configPath, config.LoadOptions{})
		if err != nil {
			return err
		}
		if !cfg.Metrics.Enabled {
			return errors.New("metrics are disabled; halro stats reads the metrics endpoint")
		}
		endpoint := *statsURL
		if endpoint == "" {
			endpoint, err = statsMetricsURL(cfg.Server.MetricsListen, cfg.TLS.Enabled)
			if err != nil {
				return err
			}
		}
		token, err := app.MetricsToken(cfg)
		if err != nil {
			return err
		}
		defer clear(token)
		fetch, err := metricsSampleFetcher(endpoint, token, *timeout)
		if err != nil {
			return err
		}
		return runStats(fetch, *interval, os.Stdout)
	case "healthcheck":
		flags := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
		defaultURL := os.Getenv("HALRO_HEALTH_URL")
		if defaultURL == "" {
			defaultURL = "http://127.0.0.1:8080/health/ready"
		}
		healthURL := flags.String("url", defaultURL, "loopback readiness URL")
		timeout := flags.Duration("timeout", 2*time.Second, "health request timeout")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		return runHealthcheck(*healthURL, *timeout)
	case "version":
		return json.NewEncoder(os.Stdout).Encode(versionReport())
	case "init":
		flags := flag.NewFlagSet("init", flag.ContinueOnError)
		configPath := flags.String("config", "config.yaml", "configuration file")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		cfg, err := config.Load(*configPath, config.LoadOptions{})
		if err != nil {
			return err
		}
		if err := initializeCommand(cfg); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "Halro initialized")
		return nil
	case "serve":
		flags := flag.NewFlagSet("serve", flag.ContinueOnError)
		configPath := flags.String("config", "config.yaml", "configuration file")
		allowInsecure := flags.Bool("allow-insecure-public-listen", false, "allow only the Gateway listener to bind public plaintext")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		cfg, err := config.Load(*configPath, config.LoadOptions{AllowInsecurePublicGateway: *allowInsecure})
		if err != nil {
			return err
		}
		if *allowInsecure {
			logger.Warn("insecure public Gateway override enabled")
		}
		return runRuntime(cfg, logger, false)
	case "bootstrap":
		flags := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
		configPath := flags.String("config", "config.yaml", "configuration file")
		providerName := flags.String("provider-name", "OpenAI", "provider display name")
		providerType := flags.String("provider-type", string(domain.ProviderOpenAI), "openai, azure_openai, deepseek, openai_compatible, gemini, or bedrock")
		providerBaseURL := flags.String("provider-base-url", "https://api.openai.com", "provider base URL")
		providerAPIVersion := flags.String("provider-api-version", "", "provider API version (required for azure_openai)")
		providerModel := flags.String("provider-model", "", "provider model name")
		publicModel := flags.String("public-model", "chat", "public model alias")
		projectName := flags.String("project-name", "Default", "project display name")
		dailyBudget := flags.Int64("daily-budget-micros-usd", 0, "daily budget in micro-USD; zero is unlimited")
		inputPrice := flags.Int64("input-micros-per-million", 0, "input price in micro-USD per million tokens")
		outputPrice := flags.Int64("output-micros-per-million", 0, "output price in micro-USD per million tokens")
		billingMode := flags.String("billing-mode", "", "billing mode: metered or free; required when both prices are zero")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		cfg, err := config.Load(*configPath, config.LoadOptions{})
		if err != nil {
			return err
		}
		secret, err := io.ReadAll(io.LimitReader(os.Stdin, (16<<10)+1))
		if err != nil {
			return fmt.Errorf("read provider key from stdin: %w", err)
		}
		secret = bytes.TrimSpace(secret)
		defer clear(secret)
		result, err := app.Bootstrap(context.Background(), cfg, app.BootstrapOptions{
			ProviderName: *providerName, ProviderType: domain.ProviderType(*providerType),
			ProviderBaseURL: *providerBaseURL, ProviderAPIVersion: *providerAPIVersion,
			ProviderModel: *providerModel, PublicModel: *publicModel,
			ProjectName: *projectName, DailyBudgetMicrosUSD: *dailyBudget,
			BillingMode:           domain.BillingMode(*billingMode),
			InputMicrosPerMillion: *inputPrice, OutputMicrosPerMillion: *outputPrice,
		}, secret)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Gateway key is shown once; store it securely.")
		return json.NewEncoder(os.Stdout).Encode(result)
	case "admin":
		if len(arguments) < 2 || (arguments[1] != "bootstrap" && arguments[1] != "reset-password" && arguments[1] != "reset-mfa") {
			return errors.New("usage: halro admin <bootstrap|reset-password|reset-mfa> --config <path> --username <name>")
		}
		command := arguments[1]
		flags := flag.NewFlagSet("admin "+command, flag.ContinueOnError)
		configPath := flags.String("config", "config.yaml", "configuration file")
		username := flags.String("username", "admin", "local admin username")
		if err := flags.Parse(arguments[2:]); err != nil {
			return err
		}
		cfg, err := config.Load(*configPath, config.LoadOptions{})
		if err != nil {
			return err
		}
		if command == "reset-mfa" {
			if err := app.ResetAdminMFA(context.Background(), cfg, *username); err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, "Admin MFA reset; all existing sessions invalidated")
			return nil
		}
		password, err := io.ReadAll(io.LimitReader(os.Stdin, 1025))
		if err != nil {
			return fmt.Errorf("read admin password from stdin: %w", err)
		}
		password = bytes.TrimRight(password, "\r\n")
		defer clear(password)
		if command == "bootstrap" {
			if err := app.BootstrapAdmin(context.Background(), cfg, *username, password); err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, "Admin user created")
			return nil
		}
		if err := app.ResetAdminPassword(context.Background(), cfg, *username, password); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "Admin password reset; all existing sessions invalidated")
		return nil
	case "config":
		if len(arguments) < 2 || arguments[1] != "check" {
			return errors.New("usage: halro config check --config <path>")
		}
		flags := flag.NewFlagSet("config check", flag.ContinueOnError)
		configPath := flags.String("config", "config.yaml", "configuration file")
		if err := flags.Parse(arguments[2:]); err != nil {
			return err
		}
		if _, err := config.Load(*configPath, config.LoadOptions{}); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "configuration valid")
		return nil
	case "key":
		if len(arguments) < 2 {
			return errors.New("usage: halro key <create|disable|rotate|rewrap|recover|slot>")
		}
		switch arguments[1] {
		case "slot":
			if len(arguments) < 3 {
				return errors.New("usage: halro key slot <status|revoke>")
			}
			if arguments[2] == "status" {
				flags := flag.NewFlagSet("key slot status", flag.ContinueOnError)
				configPath := flags.String("config", "config.yaml", "configuration file")
				if err := flags.Parse(arguments[3:]); err != nil {
					return err
				}
				cfg, err := config.Load(*configPath, config.LoadOptions{})
				if err != nil {
					return err
				}
				result, err := inspectKMSSlotsCommand(context.Background(), cfg)
				if err != nil {
					return err
				}
				return json.NewEncoder(os.Stdout).Encode(result)
			}
			if arguments[2] != "revoke" {
				return errors.New("usage: halro key slot <status|revoke>")
			}
			flags := flag.NewFlagSet("key slot revoke", flag.ContinueOnError)
			configPath := flags.String("config", "config.yaml", "configuration file")
			slotID := flags.String("slot-id", "", "retiring Slot ID")
			expectedDescriptorRevision := flags.Uint64("expected-descriptor-revision", 0, "current descriptor revision")
			expectedSlotRevision := flags.Uint64("expected-slot-revision", 0, "current Slot revision")
			confirmSlotID := flags.String("confirm-slot-id", "", "exact retiring Slot ID confirmation")
			reason := flags.String("reason", "retirement_window_completed", "retirement_window_completed or incident_retirement")
			if err := flags.Parse(arguments[3:]); err != nil {
				return err
			}
			cfg, err := config.Load(*configPath, config.LoadOptions{})
			if err != nil {
				return err
			}
			result, err := revokeKMSCommand(context.Background(), cfg, app.KMSRevokeOptions{
				SlotID: *slotID, ConfirmSlotID: *confirmSlotID,
				ExpectedDescriptorRevision: *expectedDescriptorRevision,
				ExpectedSlotRevision:       *expectedSlotRevision, ReasonCode: *reason,
			})
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(result)
		case "recover":
			flags := flag.NewFlagSet("key recover", flag.ContinueOnError)
			configPath := flags.String("config", "config.yaml", "configuration file")
			confirmedSlot := flags.String("confirm-recovery-slot", "", "exact configured Recovery Slot ID")
			if err := flags.Parse(arguments[2:]); err != nil {
				return err
			}
			cfg, err := config.Load(*configPath, config.LoadOptions{})
			if err != nil {
				return err
			}
			result, err := verifyRecoveryCommand(context.Background(), cfg, *confirmedSlot)
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, recoveryNextStepMessage)
			return json.NewEncoder(os.Stdout).Encode(result)
		case "rotate":
			flags := flag.NewFlagSet("key rotate", flag.ContinueOnError)
			configPath := flags.String("config", "config.yaml", "configuration file")
			newKeyFile := flags.String("new-key-file", "", "existing 32-byte replacement master key file")
			operationID := flags.String("operation-id", "", "stable non-secret operation ID required for key_slots retries")
			if err := flags.Parse(arguments[2:]); err != nil {
				return err
			}
			cfg, err := config.Load(*configPath, config.LoadOptions{})
			if err != nil {
				return err
			}
			var result app.KeyRotationResult
			if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
				if *newKeyFile != "" || *operationID == "" {
					return errors.New("key_slots rotate requires --operation-id and does not accept --new-key-file")
				}
				result, err = rotateKMSCommand(context.Background(), cfg, *operationID)
			} else {
				if *newKeyFile == "" || *operationID != "" {
					return errors.New("file rotate requires --new-key-file and does not accept --operation-id")
				}
				result, err = app.RotateMasterKey(context.Background(), cfg, *newKeyFile)
			}
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(result)
		case "rewrap":
			flags := flag.NewFlagSet("key rewrap", flag.ContinueOnError)
			configPath := flags.String("config", "config.yaml", "configuration file")
			purpose := flags.String("purpose", "", "Slot purpose: primary or recovery")
			slotID := flags.String("slot-id", "", "new configured Slot ID")
			keyReference := flags.String("key-reference", "", "new allowlisted KMS Key reference")
			compromised := flags.Bool("compromised", false, "declare suspected KEK or Decrypt identity compromise (rewrap will fail closed and require DEK rotate)")
			if err := flags.Parse(arguments[2:]); err != nil {
				return err
			}
			cfg, err := config.Load(*configPath, config.LoadOptions{})
			if err != nil {
				return err
			}
			result, err := rewrapKMSCommand(context.Background(), cfg, app.KMSRewrapOptions{
				Purpose: masterkey.KeySlotPurpose(*purpose), SlotID: *slotID, KeyReference: *keyReference, Compromised: *compromised,
			})
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(result)
		case "create":
			flags := flag.NewFlagSet("key create", flag.ContinueOnError)
			configPath := flags.String("config", "config.yaml", "configuration file")
			projectID := flags.String("project-id", "", "project ID")
			name := flags.String("name", "", "key display name")
			if err := flags.Parse(arguments[2:]); err != nil {
				return err
			}
			cfg, err := config.Load(*configPath, config.LoadOptions{})
			if err != nil {
				return err
			}
			result, err := app.CreateProjectKey(context.Background(), cfg, *projectID, *name)
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "Gateway key is shown once; store it securely.")
			return json.NewEncoder(os.Stdout).Encode(result)
		case "disable":
			flags := flag.NewFlagSet("key disable", flag.ContinueOnError)
			configPath := flags.String("config", "config.yaml", "configuration file")
			keyID := flags.String("key-id", "", "Gateway key ID")
			if err := flags.Parse(arguments[2:]); err != nil {
				return err
			}
			cfg, err := config.Load(*configPath, config.LoadOptions{})
			if err != nil {
				return err
			}
			if err := app.DisableProjectKey(context.Background(), cfg, *keyID); err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, "Gateway key disabled")
			return nil
		default:
			return fmt.Errorf("unknown key command %q", arguments[1])
		}
	case "restore":
		return run(append([]string{"backup", "restore"}, arguments[1:]...), logger)
	case "backup":
		if len(arguments) < 2 {
			return errors.New("usage: halro backup <create|verify|restore>")
		}
		switch arguments[1] {
		case "create":
			flags := flag.NewFlagSet("backup create", flag.ContinueOnError)
			configPath := flags.String("config", "config.yaml", "configuration file")
			outputPath := flags.String("output", "", "encrypted backup output file")
			keyPath := flags.String("key-file", "", "32-byte backup key file with mode 0600")
			if err := flags.Parse(arguments[2:]); err != nil {
				return err
			}
			if *outputPath == "" || *keyPath == "" {
				return errors.New("backup create requires --output and --key-file")
			}
			cfg, err := config.Load(*configPath, config.LoadOptions{})
			if err != nil {
				return err
			}
			key, err := backuppkg.LoadKeyFile(*keyPath)
			if err != nil {
				return err
			}
			defer clear(key)
			absoluteOutput, err := filepath.Abs(*outputPath)
			if err != nil {
				return err
			}
			manifest, err := app.CreateBackup(
				context.Background(), cfg, *configPath, absoluteOutput, key,
			)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(manifest)
		case "verify":
			flags := flag.NewFlagSet("backup verify", flag.ContinueOnError)
			backupPath := flags.String("file", "", "encrypted backup file")
			keyPath := flags.String("key-file", "", "32-byte backup key file with mode 0600")
			if err := flags.Parse(arguments[2:]); err != nil {
				return err
			}
			if *backupPath == "" || *keyPath == "" {
				return errors.New("backup verify requires --file and --key-file")
			}
			key, err := backuppkg.LoadKeyFile(*keyPath)
			if err != nil {
				return err
			}
			defer clear(key)
			manifest, err := app.VerifyBackup(*backupPath, key)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(manifest)
		case "restore":
			flags := flag.NewFlagSet("backup restore", flag.ContinueOnError)
			configPath := flags.String("config", "config.yaml", "configuration file")
			backupPath := flags.String("file", "", "encrypted backup file")
			keyPath := flags.String("key-file", "", "32-byte backup key file with mode 0600")
			confirmation := flags.String("confirm-backup-id", "", "exact verified backup ID")
			useRecovery := flags.Bool("use-recovery-slot", false, "explicitly unlock the staged backup with the configured Recovery Slot")
			confirmRecovery := flags.String("confirm-recovery-slot", "", "exact configured Recovery Slot ID")
			if err := flags.Parse(arguments[2:]); err != nil {
				return err
			}
			if *backupPath == "" || *keyPath == "" || *confirmation == "" {
				return errors.New("backup restore requires --file, --key-file, and --confirm-backup-id")
			}
			cfg, err := config.Load(*configPath, config.LoadOptions{})
			if err != nil {
				return err
			}
			key, err := backuppkg.LoadKeyFile(*keyPath)
			if err != nil {
				return err
			}
			defer clear(key)
			result, err := restoreBackupCommand(
				context.Background(), cfg, *backupPath, key, *confirmation,
				app.RestoreOptions{UseRecoverySlot: *useRecovery, ConfirmRecoverySlot: *confirmRecovery},
			)
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "Restore complete; previous data directory was preserved for rollback.")
			return json.NewEncoder(os.Stdout).Encode(result)
		default:
			return fmt.Errorf("unknown backup command %q", arguments[1])
		}
	case "usage":
		if len(arguments) < 2 {
			return errors.New("usage: halro usage <compact|verify|prune>")
		}
		flags := flag.NewFlagSet("usage "+arguments[1], flag.ContinueOnError)
		configPath := flags.String("config", "config.yaml", "configuration file")
		before := flags.String("before", "", "prune partitions before YYYY-MM-DD")
		if err := flags.Parse(arguments[2:]); err != nil {
			return err
		}
		cfg, err := config.Load(*configPath, config.LoadOptions{})
		if err != nil {
			return err
		}
		switch arguments[1] {
		case "compact":
			manifest, err := app.CompactUsage(context.Background(), cfg)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(manifest)
		case "verify":
			report, err := app.VerifyUsage(context.Background(), cfg)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(report)
		case "prune":
			cutoff := usageRetentionCutoff(time.Now(), cfg.Usage.RetentionDays)
			if *before != "" {
				parsed, err := time.Parse("2006-01-02", *before)
				if err != nil {
					return fmt.Errorf("parse --before: %w", err)
				}
				cutoff = parsed
			}
			report, err := app.PruneUsage(context.Background(), cfg, cutoff)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(report)
		default:
			return fmt.Errorf("unknown usage command %q", arguments[1])
		}
	case "audit":
		if len(arguments) < 2 {
			return errors.New("usage: halro audit verify|verify-anchor --config <path> [--anchors <path>]")
		}
		switch arguments[1] {
		case "verify":
			flags := flag.NewFlagSet("audit verify", flag.ContinueOnError)
			configPath := flags.String("config", "config.yaml", "configuration file")
			if err := flags.Parse(arguments[2:]); err != nil {
				return err
			}
			cfg, err := config.Load(*configPath, config.LoadOptions{})
			if err != nil {
				return err
			}
			summary, err := app.VerifyAudit(context.Background(), cfg)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(summary)
		case "verify-anchor":
			flags := flag.NewFlagSet("audit verify-anchor", flag.ContinueOnError)
			configPath := flags.String("config", "config.yaml", "configuration file")
			anchorsPath := flags.String("anchors", "", "path to a JSON-lines file of anchors pulled off-host (e.g. by the dead-man probe)")
			if err := flags.Parse(arguments[2:]); err != nil {
				return err
			}
			if *anchorsPath == "" {
				return errors.New("usage: halro audit verify-anchor --config <path> --anchors <path>")
			}
			cfg, err := config.Load(*configPath, config.LoadOptions{})
			if err != nil {
				return err
			}
			anchors, err := app.LoadAuditAnchorsFile(*anchorsPath)
			if err != nil {
				return err
			}
			verdicts, err := app.VerifyAuditAnchors(context.Background(), cfg, anchors)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(verdicts)
		default:
			return errors.New("usage: halro audit verify|verify-anchor --config <path> [--anchors <path>]")
		}
	case "ledger":
		if len(arguments) < 2 || arguments[1] != "verify" {
			return errors.New("usage: halro ledger verify --config <path>")
		}
		flags := flag.NewFlagSet("ledger verify", flag.ContinueOnError)
		configPath := flags.String("config", "config.yaml", "configuration file")
		if err := flags.Parse(arguments[2:]); err != nil {
			return err
		}
		cfg, err := config.Load(*configPath, config.LoadOptions{})
		if err != nil {
			return err
		}
		report, err := app.VerifyLedger(context.Background(), cfg)
		if err != nil {
			return err
		}
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			return err
		}
		// The report is the answer; the exit status is what a monitor reads.
		// A chain that could not be authenticated is not a pass, even when it
		// is the honest state of a ledger that predates epoch 4.
		if !report.ChainVerified {
			return errors.New("ledger chain could not be authenticated")
		}
		return nil
	case "doctor":
		flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
		configPath := flags.String("config", "config.yaml", "configuration file")
		noKMS := flags.Bool("no-kms", false, "perform static Key Slot checks without KMS unwrap")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		cfg, err := config.Load(*configPath, config.LoadOptions{})
		if err != nil {
			return err
		}
		report, doctorErr := doctorCommand(context.Background(), cfg, app.DoctorOptions{NoKMS: *noKMS})
		encodeErr := json.NewEncoder(os.Stdout).Encode(report)
		return errors.Join(doctorErr, encodeErr)
	case "metrics":
		if len(arguments) < 2 {
			return errors.New("usage: halro metrics <token|rotate|revoke|list|verify-audit> --config <path>")
		}
		flags := flag.NewFlagSet("metrics "+arguments[1], flag.ContinueOnError)
		configPath := flags.String("config", "config.yaml", "configuration file")
		overlap := flags.Duration("overlap", 10*time.Minute, "old/new token overlap for rotate")
		version := flags.Uint64("version", 0, "credential version for revoke")
		if err := flags.Parse(arguments[2:]); err != nil {
			return err
		}
		cfg, err := config.Load(*configPath, config.LoadOptions{})
		if err != nil {
			return err
		}
		switch arguments[1] {
		case "token":
			token, err := app.MetricsToken(cfg)
			if err != nil {
				return err
			}
			defer clear(token)
			fmt.Fprintln(os.Stderr, "Metrics bearer token grants access to aggregate operational data.")
			_, err = fmt.Fprintln(os.Stdout, string(token))
			return err
		case "rotate":
			if cfg.Metrics.CredentialFile == "" {
				return errors.New("metrics.credential_file is required for versioned rotation")
			}
			rotation, err := bearercred.Rotate(cfg.Metrics.CredentialFile, *overlap, time.Now())
			if err != nil {
				return err
			}
			defer clear(rotation.Token)
			fmt.Fprintln(os.Stderr, "Store this one-time Metrics bearer token directly in the Prometheus secret file.")
			if _, err := fmt.Fprintln(os.Stdout, string(rotation.Token)); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "credential_version="+strconv.FormatUint(rotation.Version, 10))
			return nil
		case "revoke":
			if cfg.Metrics.CredentialFile == "" || *version == 0 {
				return errors.New("metrics revoke requires metrics.credential_file and --version")
			}
			return bearercred.Revoke(cfg.Metrics.CredentialFile, *version, time.Now())
		case "list":
			if cfg.Metrics.CredentialFile == "" {
				return errors.New("metrics.credential_file is required for versioned credentials")
			}
			if err := bearercred.VerifyAudit(cfg.Metrics.CredentialFile); err != nil {
				return fmt.Errorf("verify metrics credential audit: %w", err)
			}
			file, err := bearercred.Load(cfg.Metrics.CredentialFile)
			if err != nil {
				return err
			}
			type summary struct {
				Version uint64           `json:"version"`
				State   bearercred.State `json:"state"`
				Created time.Time        `json:"created_at"`
				Retire  *time.Time       `json:"retire_at,omitempty"`
				Revoked *time.Time       `json:"revoked_at,omitempty"`
			}
			result := make([]summary, 0, len(file.Credentials))
			for _, credential := range file.Credentials {
				result = append(result, summary{credential.Version, credential.State, credential.CreatedAt, credential.RetireAt, credential.RevokedAt})
			}
			return json.NewEncoder(os.Stdout).Encode(result)
		case "verify-audit":
			if cfg.Metrics.CredentialFile == "" {
				return errors.New("metrics.credential_file is required for audit verification")
			}
			head, err := bearercred.AuditStatus(cfg.Metrics.CredentialFile)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(head)
		default:
			return fmt.Errorf("unknown metrics command %q", arguments[1])
		}
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func runRuntime(cfg config.Config, logger *slog.Logger, printGuide bool) error {
	hardening, err := hardenRuntimeCommand()
	if err != nil {
		return fmt.Errorf("apply runtime host hardening before Master Key unlock: %w", err)
	}
	logger.Info("runtime host hardening applied",
		"core_dumps_disabled", hardening.CoreDumpsDisabled,
		"process_dump_disabled", hardening.ProcessDumpDisabled,
		"managed_heap_dontdump", hardening.ManagedHeapDontDump,
	)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runtime, err := app.Open(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer runtime.Close()
	ready := func() error { return nil }
	if printGuide {
		ready = func() error {
			status, err := runtime.SetupStatus(ctx)
			if err != nil {
				return err
			}
			scheme := "http"
			if cfg.TLS.Enabled {
				scheme = "https"
			}
			adminPath := "/admin"
			if status.SetupRequired {
				adminPath = "/admin/setup"
			}
			fmt.Fprintln(os.Stdout, "Halro is running")
			fmt.Fprintf(os.Stdout, "Admin:   %s://%s%s\n", scheme, cfg.Server.AdminListen, adminPath)
			fmt.Fprintf(os.Stdout, "Gateway: %s://%s\n", scheme, cfg.Server.GatewayListen)
			if cfg.Metrics.Enabled {
				fmt.Fprintf(os.Stdout, "Metrics: %s://%s\n", scheme, cfg.Server.MetricsListen)
			}
			if token, required, err := runtime.SetupToken(ctx); err != nil {
				return err
			} else if required {
				// This is deliberately written directly to the controlling process,
				// not through structured application logging.
				fmt.Fprintf(os.Stderr, "One-time setup token: %s\n", token)
			}
			return nil
		}
	}
	return runtime.RunWithReady(ctx, ready)
}

func runHealthcheck(rawURL string, timeout time.Duration) error {
	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("healthcheck redirects are disabled")
		},
	}
	return runHealthcheckWithClient(rawURL, timeout, client)
}

func runHealthcheckWithClient(rawURL string, timeout time.Duration, client *http.Client) error {
	if timeout <= 0 || timeout > 30*time.Second {
		return errors.New("healthcheck timeout must be between zero and 30 seconds")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("healthcheck URL must be a plain HTTP(S) loopback URL")
	}
	hostname := parsed.Hostname()
	loopback := hostname == "localhost"
	if address, parseErr := netip.ParseAddr(hostname); parseErr == nil {
		loopback = address.IsLoopback()
	}
	if !loopback {
		return errors.New("healthcheck URL must use a loopback host")
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, parsed.String(), nil)
	if err != nil {
		return errors.New("create healthcheck request")
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("readiness endpoint is unavailable")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("readiness endpoint returned status %d", response.StatusCode)
	}
	return nil
}
