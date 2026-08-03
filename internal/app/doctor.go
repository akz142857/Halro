package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/akz142857/Heimdall/internal/config"
	"github.com/akz142857/Heimdall/internal/ledger"
	boltstore "github.com/akz142857/Heimdall/internal/store/bolt"
	"github.com/akz142857/Heimdall/internal/store/lock"
	"github.com/akz142857/Heimdall/internal/usage"
	"github.com/akz142857/Heimdall/internal/vault"
)

type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type DoctorReport struct {
	Healthy   bool          `json:"healthy"`
	CheckedAt time.Time     `json:"checked_at"`
	Checks    []DoctorCheck `json:"checks"`
}

// Doctor performs only read operations against application data. It acquires
// the normal data lock to obtain one consistent offline view, but never opens
// bbolt or the Ledger in repair/write mode.
func Doctor(ctx context.Context, cfg config.Config) (DoctorReport, error) {
	report := DoctorReport{Healthy: true, CheckedAt: time.Now().UTC()}
	add := func(name, status, detail string) {
		report.Checks = append(report.Checks, DoctorCheck{Name: name, Status: status, Detail: detail})
		if status == "fail" {
			report.Healthy = false
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
				add("master_key", "pass", "mode and encrypted metadata key check are valid")
			}
			secretVault.Close()
		}
	}

	watermark, partial, walErr := ledger.Inspect(cfg.LedgerPath())
	if walErr != nil {
		add("ledger", "fail", walErr.Error())
	} else if partial {
		add("ledger", "fail", fmt.Sprintf("partial WAL tail after committed offset %d; doctor did not truncate it", watermark.Offset))
	} else {
		add("ledger", "pass", fmt.Sprintf("committed sequence %d at offset %d", watermark.Sequence, watermark.Offset))
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

	zone, zoneErr := time.LoadLocation(cfg.Usage.Timezone)
	if zoneErr != nil {
		add("clock", "fail", zoneErr.Error())
	} else {
		add("clock", "pass", fmt.Sprintf("system UTC=%s usage timezone=%s", time.Now().UTC().Format(time.RFC3339), zone.String()))
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
	}
	add("provider_connectivity", "warn", "network probes skipped by read-only offline doctor; use Admin connection tests")
	if !report.Healthy {
		return report, errors.New("doctor found one or more failed checks")
	}
	return report, nil
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
	warnings := 0
	for _, item := range deployments {
		deploymentEnabled[item.ID] = item.Enabled && item.DeletedAt == nil && providerEnabled[item.ProviderID]
		if item.Enabled && item.DeletedAt == nil && item.InputMicrosPerMillion == 0 && item.OutputMicrosPerMillion == 0 {
			warnings++
		}
	}
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
	status := "pass"
	detail := fmt.Sprintf("%d active routes; all references are available", active)
	if warnings > 0 {
		status = "warn"
		detail = fmt.Sprintf("%s; %d active or configured deployments have zero pricing", detail, warnings)
	}
	add("topology", status, detail)
}
