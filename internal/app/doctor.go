package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/akz142857/Heimdall/internal/budget"
	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/ledger"
	"github.com/akz142857/Heimdall/internal/masterkey"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/store/lock"
	"github.com/akz142857/Heimdall/internal/timezone"
	"github.com/akz142857/Heimdall/internal/usage"
	"github.com/akz142857/Heimdall/internal/vault"
)

type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type DoctorReport struct {
	Healthy             bool          `json:"healthy"`
	VaultStatus         string        `json:"vault_status"`
	ExternalAuditEvents bool          `json:"external_audit_events"`
	CheckedAt           time.Time     `json:"checked_at"`
	Checks              []DoctorCheck `json:"checks"`
}

type DoctorOptions struct {
	NoKMS bool
}

// Doctor performs only read operations against application data. It acquires
// the normal data lock to obtain one consistent offline view, but never opens
// bbolt or the Ledger in repair/write mode.
func Doctor(ctx context.Context, cfg config.Config) (DoctorReport, error) {
	return DoctorWithOptions(ctx, cfg, DoctorOptions{})
}

func DoctorWithOptions(ctx context.Context, cfg config.Config, options DoctorOptions) (DoctorReport, error) {
	report := DoctorReport{Healthy: true, VaultStatus: "unknown", CheckedAt: time.Now().UTC()}
	failedChecks := 0
	add := func(name, status, detail string) {
		report.Checks = append(report.Checks, DoctorCheck{Name: name, Status: status, Detail: detail})
		if status == "fail" {
			report.Healthy = false
			failedChecks++
		}
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if err := cfg.Validate(config.LoadOptions{}); err != nil {
		add("config", "fail", err.Error())
		return report, errors.New("doctor found an invalid configuration")
	}
	add("config", "pass", "configuration schema and safety policy are valid")

	dataLock, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
	if err != nil {
		add("data_lock", "fail", "data directory is in use or cannot be locked")
		return report, errors.New("doctor requires exclusive offline access")
	}
	defer dataLock.Close()
	add("data_lock", "pass", "exclusive offline snapshot acquired")

	for name, path := range map[string]string{
		"data_permissions":     cfg.Storage.DataDir,
		"metadata_permissions": cfg.MetadataPath(),
		"ledger_permissions":   cfg.LedgerPath(),
		"audit_permissions":    cfg.AuditPath(),
	} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			add(name, "fail", statErr.Error())
			continue
		}
		if info.Mode().Perm()&0o077 != 0 {
			add(name, "fail", fmt.Sprintf("permissions %04o expose owner data", info.Mode().Perm()))
			continue
		}
		add(name, "pass", fmt.Sprintf("permissions %04o", info.Mode().Perm()))
	}

	store, err := boltstore.OpenReadOnly(cfg.MetadataPath())
	if err != nil {
		add("metadata", "fail", err.Error())
	} else {
		defer store.Close()
		add("metadata", "pass", fmt.Sprintf("bbolt schema v%d", boltstore.CurrentSchemaVersion()))
	}
	staticKMS := options.NoKMS && cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots
	if staticKMS {
		if store == nil {
			add("key_slots", "fail", "metadata is unavailable for static Key Slot validation")
		} else if staticErr := validateDoctorKeySlots(ctx, cfg, store); staticErr != nil {
			add("key_slots", "fail", staticErr.Error())
		} else {
			add("key_slots", "pass", "descriptor, Keyring, configured Slots, and KMS allowlist references are structurally valid")
		}
		report.Healthy = false
		report.VaultStatus = "vault_unverified"
		add("master_key", "unverified", "KMS unwrap was explicitly disabled; Vault recovery is not verified")
	} else {
		if cfg.Storage.MasterKey.Mode == config.MasterKeyModeKeySlots {
			report.ExternalAuditEvents = true
		}
		masterKey, keyErr := unlockMasterKey(ctx, cfg, store)
		if keyErr != nil {
			add("master_key", "fail", keyErr.Error())
		} else {
			defer clear(masterKey)
			if store == nil {
				add("master_key", "warn", "key file is valid; metadata key check was skipped")
			} else if secretVault, vaultErr := vault.New(masterKey); vaultErr != nil {
				add("master_key", "fail", vaultErr.Error())
			} else {
				if verifyErr := verifyVaultKeyCheck(store, secretVault); verifyErr != nil {
					add("master_key", "fail", "master key does not decrypt the metadata key check")
				} else {
					report.VaultStatus = "verified"
					add("master_key", "pass", "mode and encrypted metadata key check are valid")
				}
				secretVault.Close()
			}
		}
	}

	ledgerState := ledger.NewState()
	recoveredReleased, recoveredSettled := 0, 0
	watermark, partial, walErr := ledger.InspectReplay(cfg.LedgerPath(), func(record ledger.Record) error {
		if record.Event.Outcome == "recovered_not_started" {
			recoveredReleased++
		} else if record.Event.Outcome == "recovered_started_unknown_result" {
			recoveredSettled++
		}
		return ledgerState.Apply(record)
	})
	if walErr != nil {
		add("ledger", "fail", walErr.Error())
	} else if partial {
		add("ledger", "fail", fmt.Sprintf("partial WAL tail after committed offset %d; doctor did not truncate it", watermark.Offset))
	} else {
		add("ledger", "pass", fmt.Sprintf("committed sequence %d at offset %d", watermark.Sequence, watermark.Offset))
		pending, oldestAge := ledgerState.PendingLeaseStats(time.Now())
		status := "pass"
		if pending > 0 {
			status = "warn"
		}
		add("accounting_leases", status, fmt.Sprintf("pending=%d oldest_age=%s recovered_released=%d recovered_settled=%d", pending, oldestAge.Round(time.Second), recoveredReleased, recoveredSettled))
	}

	exporter, exportErr := usage.NewExporter(cfg.UsagePath())
	if exportErr != nil {
		add("parquet", "fail", exportErr.Error())
	} else if _, manifestErr := exporter.LoadManifest(); errors.Is(manifestErr, os.ErrNotExist) {
		add("parquet", "warn", "no usage manifest yet")
	} else if manifestErr != nil {
		add("parquet", "fail", manifestErr.Error())
	} else if verifyErr := exporter.Verify(nil); verifyErr != nil {
		add("parquet", "fail", verifyErr.Error())
	} else {
		add("parquet", "pass", "manifest, checksums, schemas, and summaries are valid")
	}

	// The stored setting decides the boundary; config.yaml only seeded it. An
	// operator who edited the file and restarted would otherwise have no way to
	// discover that the edit did nothing.
	accountingZone := cfg.Usage.Timezone
	configInEffect := true
	if store != nil {
		switch settings, settingsErr := store.InstanceAccountingSettings(); {
		case errors.Is(settingsErr, boltstore.ErrNotFound):
			add("accounting_timezone", "warn", "no stored accounting timezone yet; the next start seeds it from usage.timezone")
		case settingsErr != nil:
			add("accounting_timezone", "fail", settingsErr.Error())
		default:
			accountingZone = settings.Timezone
			configInEffect = settings.Timezone == cfg.Usage.Timezone
			detail := fmt.Sprintf("stored=%s version=%d", settings.Timezone, settings.TimezoneVersion)
			if settings.HasPendingChange() {
				detail += fmt.Sprintf(" pending=%s effective=%s",
					settings.PendingTimezone, settings.PendingEffectiveAt.UTC().Format(time.RFC3339))
			}
			if configInEffect {
				add("accounting_timezone", "pass", detail)
			} else {
				add("accounting_timezone", "warn", detail+fmt.Sprintf(
					"; config.yaml says %s and is no longer applied", cfg.Usage.Timezone))
			}
		}
	}
	zone, zoneErr := time.LoadLocation(accountingZone)
	if zoneErr != nil {
		add("clock", "fail", zoneErr.Error())
	} else {
		now := time.Now()
		period, periodErr := budget.PeriodAt(now, zone)
		if periodErr != nil {
			add("clock", "fail", periodErr.Error())
		} else {
			add("clock", "pass", fmt.Sprintf("system UTC=%s accounting timezone=%s current period %s=[%s,%s)",
				now.UTC().Format(time.RFC3339), zone.String(), period.ID,
				period.Start.Format(time.RFC3339), period.End.Format(time.RFC3339)))
		}
	}
	// tzdata drift between nodes moves period boundaries without any other
	// symptom, so the fingerprint is reported whether or not it looks healthy —
	// it only means something when compared against another node.
	if database, tzErr := timezone.Describe(accountingZone); tzErr != nil {
		add("tzdata", "fail", tzErr.Error())
	} else {
		add("tzdata", "pass", fmt.Sprintf("source=%s version=%s fingerprint=%s", database.Source, database.Version, database.Fingerprint))
	}
	var stats syscall.Statfs_t
	if statErr := syscall.Statfs(cfg.Storage.DataDir, &stats); statErr != nil {
		add("disk", "fail", statErr.Error())
	} else {
		free := stats.Bavail * uint64(stats.Bsize)
		status := "pass"
		if free < 1<<30 {
			status = "warn"
		}
		add("disk", status, fmt.Sprintf("%d bytes available", free))
	}

	if store != nil {
		checkDoctorTopology(ctx, store, add)
		if err := store.PricingReadiness(ctx); err != nil {
			add("pricing_clock", "fail", err.Error())
		} else {
			add("pricing_clock", "pass", "pricing high-water marks are coherent and not quarantined")
		}
	}
	add("provider_connectivity", "warn", "network probes skipped by read-only offline doctor; use Admin connection tests")
	if failedChecks > 0 {
		return report, errors.New("doctor found one or more failed checks")
	}
	return report, nil
}

func validateDoctorKeySlots(ctx context.Context, cfg config.Config, store *boltstore.Store) error {
	descriptor, err := store.KeySlotDescriptor(ctx)
	if err != nil {
		return err
	}
	if !descriptor.ProductionReady() {
		return errors.New("Key Slot descriptor is not production-ready")
	}
	keyring, err := store.VaultKeyring()
	if err != nil {
		return err
	}
	if keyring.ActiveFingerprint != descriptor.MasterKeyFingerprint {
		return errors.New("Key Slot descriptor and Vault Keyring fingerprints differ")
	}
	for _, target := range []struct {
		id      string
		purpose masterkey.KeySlotPurpose
	}{
		{id: cfg.Storage.MasterKey.PrimarySlot, purpose: masterkey.KeySlotPrimary},
		{id: cfg.Storage.MasterKey.RecoverySlot, purpose: masterkey.KeySlotRecovery},
	} {
		slot, ok := keySlotByID(descriptor, target.id)
		if !ok || slot.Purpose != target.purpose || slot.State != masterkey.KeySlotActive || slot.VerifiedAt == nil {
			return fmt.Errorf("configured %s Slot is not active and verified", target.purpose)
		}
		if _, err := trustedAllowedKMSKey(cfg.Storage.MasterKey, slot); err != nil {
			return err
		}
	}
	return nil
}

func checkDoctorTopology(ctx context.Context, store *boltstore.Store, add func(string, string, string)) {
	providers, providerErr := store.ListProviders(ctx)
	deployments, deploymentErr := store.ListDeployments(ctx)
	routes, routeErr := store.ListRoutes(ctx)
	if err := errors.Join(providerErr, deploymentErr, routeErr); err != nil {
		add("topology", "fail", err.Error())
		return
	}
	providerEnabled := make(map[string]bool, len(providers))
	for _, item := range providers {
		providerEnabled[item.ID] = item.Enabled && item.DeletedAt == nil
	}
	deploymentEnabled := make(map[string]bool, len(deployments))
	pricingSelectedAt := time.Now().UTC()
	for _, item := range deployments {
		deploymentEnabled[item.ID] = item.Enabled && item.DeletedAt == nil && providerEnabled[item.ProviderID]
		if item.Enabled && item.DeletedAt == nil {
			if _, err := store.SelectDeploymentPriceVersion(ctx, item.ID, pricingSelectedAt); err != nil {
				add("pricing_readiness", "fail", fmt.Sprintf("enabled deployment %q has no effective versioned price: %v", item.ID, err))
				return
			}
		}
	}
	add("pricing_readiness", "pass", "all enabled deployments have an effective versioned price")
	active := 0
	for _, item := range routes {
		if !item.Enabled || item.DeletedAt != nil {
			continue
		}
		active++
		available := deploymentEnabled[item.DeploymentID]
		if item.DeploymentID == "" {
			available = providerEnabled[item.ProviderID]
		}
		if !available {
			add("topology", "fail", "an enabled route references an unavailable deployment or provider")
			return
		}
	}
	detail := fmt.Sprintf("%d active routes; all references are available", active)
	add("topology", "pass", detail)
}
