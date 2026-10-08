package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/akz142857/Halro/internal/app"
	"github.com/akz142857/Halro/internal/backup"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type haMetadataCommandArguments struct {
	command                       string
	keys                          [5]string
	archive, archiveKey, instance string
	sourceDirectory               string
	restartInstallation           bool
	restartSource                 bool
	restartActivation             bool
	identity                      [32]byte
	generation                    uint64
}

// Explicit HA commands alone advertise wire2 and dual Metadata-format
// maintenance capability. Ordinary migrate and Runtime startup remain guarded.
func runHAMetadataCommand(command string, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("metadata "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "config.yaml", "same-incarnation offline HA member configuration")
	a := haMetadataCommandArguments{command: command}
	for i, name := range []string{"master-key-file", "ledger-key-file", "audit-key-file", "governance-key-file", "source-journal-key-file"} {
		flags.StringVar(&a.keys[i], name, "", "independently recovered private 32-byte key file")
	}
	identity := ""
	if command == "ha-prepare" {
		flags.BoolVar(&a.restartSource, "restart-source", false, "preserve interrupted source in a fixed opaque slot and independently authenticate all native sources again")
		flags.StringVar(&a.archive, "from-backup", "", "authenticated stopped Replica backup at the exact old Metadata cut")
		flags.StringVar(&a.archiveKey, "backup-key-file", "", "private 32-byte backup key file")
		flags.StringVar(&identity, "identity", "", "independently established 32-byte identity in hex")
		flags.StringVar(&a.instance, "instance-id", "", "independently established instance ID")
		flags.Uint64Var(&a.generation, "key-generation", 0, "independently established key generation")
	}
	if command == "ha-install" {
		flags.StringVar(&a.sourceDirectory, "from-directory", "", "stopped Primary prepared Metadata baseline directory")
		flags.BoolVar(&a.restartInstallation, "restart-installation", false, "preserve interrupted opaque slots and freshly qualify the same independent source")
	}
	if command == "ha-activate" {
		flags.BoolVar(&a.restartActivation, "restart-activation", false, "recover an independently qualified published selection or preserve unpublished files in a fixed opaque slot")
	}
	if command != "ha-prepare" && command != "ha-maintain" && command != "ha-install" && command != "ha-activate" && command != "ha-checkpoint" {
		return errors.New("unknown HA Metadata maintenance command")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("HA Metadata command has unexpected positional arguments")
	}
	for i, path := range a.keys {
		if command == "ha-checkpoint" && i > 0 {
			continue
		}
		if path == "" {
			if command == "ha-checkpoint" {
				return errors.New("HA checkpoint requires the private master key file and independent activated HA authority")
			}
			return errors.New("HA Metadata maintenance requires all five independent key files")
		}
	}
	if command == "ha-install" && a.sourceDirectory == "" {
		return errors.New("HA Metadata install requires the independently authenticated prepared source directory")
	}
	if command == "ha-prepare" {
		raw, err := hex.DecodeString(identity)
		if err != nil || len(raw) != 32 || a.archive == "" || a.archiveKey == "" || a.instance == "" || a.generation == 0 {
			return errors.New("HA Metadata prepare requires backup and independent identity, instance and key generation")
		}
		copy(a.identity[:], raw)
		if a.identity == ([32]byte{}) {
			return metadatastate.ErrSource
		}
	}
	cfg, err := config.Load(*configPath, config.LoadOptions{SkipListenerValidation: true})
	if err != nil {
		return err
	}
	if cfg.Replication == nil || cfg.ResourceBudget == nil || cfg.ResourcePressure == nil {
		return errors.New("HA Metadata maintenance requires replication, calibrated budget and real pressure monitoring")
	}
	if err := cfg.ResourceBudget.ValidateReplicationCandidates(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if command == "ha-prepare" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
	}
	return withHAMetadataPressure(ctx, cfg, func(manager *resourcebudget.Manager) error {
		return executeHAMetadataCommand(ctx, cfg, a, manager, out)
	})
}

// The real OS sampler remains attached throughout both preparation and TLS
// maintenance. No command branch manufactures a Healthy lease.
func withHAMetadataPressure(ctx context.Context, cfg config.Config, work func(*resourcebudget.Manager) error) error {
	manager, err := resourcebudget.New(cfg.ResourceBudget.Limits(cfg.ResourcePressure.MaxAge.Value()))
	if err != nil {
		return err
	}
	sampler, err := resourcebudget.NewSampler(cfg.Storage.DataDir)
	if err != nil {
		return err
	}
	monitor, err := resourcebudget.NewMonitor(sampler, cfg.ResourcePressure.Policy(), cfg.ResourcePressure.SampleInterval.Value())
	if err != nil {
		return err
	}
	if err := monitor.AttachManager(manager); err != nil {
		return err
	}
	monitorCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- monitor.Run(monitorCtx, nil) }()
	defer func() { stop(); <-done }()
	ticker := time.NewTicker(cfg.ResourcePressure.SampleInterval.Value())
	defer ticker.Stop()
	for manager.Snapshot().Pressure != resourcebudget.Healthy {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if sample := monitor.Snapshot(); sample.Samples > 0 && sample.Issues & ^resourcebudget.Cooling != 0 {
				return resourcebudget.ErrPressure
			}
		}
	}
	return work(manager)
}

func executeHAMetadataCommand(ctx context.Context, cfg config.Config, a haMetadataCommandArguments, manager *resourcebudget.Manager, out io.Writer) (resultErr error) {
	guard, err := lock.Acquire(cfg.Storage.DataDir)
	if err != nil {
		return fmt.Errorf("HA Metadata maintenance requires the stopped member's exclusive lock: %w", err)
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = &metadataMigrationLockDebt{guard: guard, cause: resultErr}
		} else {
			resultErr = errors.Join(resultErr, guard.Close())
		}
	}()
	fee, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 32 << 10})
	if err != nil {
		return err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, fee.Release())
		}
	}()
	var raw [5][]byte
	for i, path := range a.keys {
		if a.command == "ha-checkpoint" && i > 0 {
			continue
		}
		raw[i], err = metadataMigrationKeyFile(path)
		if err != nil {
			return err
		}
		defer clear(raw[i])
	}
	keys := app.HAMetadataKeys{Master: raw[0], Ledger: raw[1], Audit: raw[2], Governance: raw[3], Metadata: raw[4]}
	if a.command == "ha-checkpoint" {
		owner, err := app.CheckpointHASelectedRuntime(ctx, cfg, keys.Master, guard, manager, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		if owner != nil {
			err = errors.Join(err, owner.Close())
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(struct {
			Phase   string `json:"phase"`
			Receipt any    `json:"receipt"`
		}{"ha-seven-root-checkpoint", owner.Publication.Receipt})
	}
	if a.command == "ha-activate" {
		owner, err := app.ActivateHAMemoryRuntimeWithOptions(ctx, cfg, keys, guard, manager, slog.New(slog.NewTextHandler(os.Stderr, nil)), app.HAMemoryActivationOptions{Restart: a.restartActivation})
		if err != nil {
			return err
		}
		if err := owner.Close(); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(struct {
			Phase         string `json:"phase"`
			BootstrapPath string `json:"bootstrap_path"`
			Receipt       any    `json:"receipt"`
		}{Phase: "ha-seven-roots-activated", BootstrapPath: owner.BootstrapPath, Receipt: owner.Receipt})
	}
	if a.command == "ha-install" {
		installed, err := app.InstallHAMetadataBaselineWithOptions(ctx, cfg, keys, a.sourceDirectory, guard, manager, app.HAMetadataInstallOptions{Restart: a.restartInstallation})
		if err != nil {
			return err
		}
		if err := installed.Close(); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(struct {
			Phase     string `json:"phase"`
			Directory string `json:"directory"`
		}{Phase: "metadata-baseline-installed", Directory: installed.Path})
	}
	if a.command == "ha-maintain" {
		owner, err := app.OpenHAMetadataMaintenance(ctx, cfg, keys, guard, manager, slog.New(slog.NewTextHandler(os.Stderr, nil)))
		if err != nil {
			return err
		}
		runErr := owner.Run(ctx, func(state replication.MemberState) error {
			return json.NewEncoder(out).Encode(struct {
				Phase string                  `json:"phase"`
				State replication.MemberState `json:"state"`
			}{Phase: "metadata-boundary-confirmed", State: state})
		})
		return errors.Join(runErr, owner.Close())
	}
	backupKey, err := metadataMigrationKeyFile(a.archiveKey)
	if err != nil {
		return err
	}
	defer clear(backupKey)
	workspace, err := backup.NewWorkspaceBudget(ctx, cfg.ResourceBudget.BackupWorkspaceDiskBytes, func(ctx context.Context, bytes int64) (func() error, error) {
		permit, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 32 << 20, Disk: bytes})
		if err != nil {
			return nil, err
		}
		return permit.Release, nil
	})
	if err != nil {
		return err
	}
	ctx, err = workspace.Bind(ctx)
	if err != nil {
		return err
	}
	archive, err := backup.OpenVerified(ctx, a.archive, backupKey)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, archive.Close()) }()
	directory := filepath.Join(cfg.Storage.DataDir, app.HAMetadataBaselineDirectory)
	owner, err := app.PrepareHAMetadataBaselineWithOptions(ctx, cfg, keys, archive, directory, guard, metadatastate.BaselineOptions{Identity: a.identity, InstanceID: a.instance, KeyGeneration: a.generation, Budget: manager, MaxSourceBytes: cfg.ResourceBudget.MetadataStoreDiskBytes, MaxDiskBytes: cfg.ResourceBudget.MetadataStoreDiskBytes, MaxAppendBytes: min(8<<20, cfg.ResourceBudget.MetadataStoreDiskBytes/2)}, app.HAMetadataPrepareOptions{RestartSource: a.restartSource})
	if err != nil {
		return err
	}
	if err := owner.Close(); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Phase   string                         `json:"phase"`
		Carrier string                         `json:"carrier"`
		Receipt metadatastate.MigrationReceipt `json:"receipt"`
	}{Phase: "metadata-baseline-prepared", Carrier: owner.CarrierPath, Receipt: owner.Candidate.Receipt})
}
