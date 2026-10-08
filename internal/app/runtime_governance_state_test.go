package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/store/lock"
)

// Legacy adapter coverage remains explicit after default Standalone selects
// disk. Only a quiescent unserved fixture may choose this alternate owner.
func installTestGovernanceState(t *testing.T, r *Runtime) {
	t.Helper()
	if err := r.governance.manager.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := governance.NewState()
	if _, err := r.governance.log.Replay(state.Apply); err != nil {
		t.Fatal(err)
	}
	r.governance.manager = governance.NewManager(r.governance.log, state, r.accounting, r.store, r.store)
}

func TestStandaloneGovernanceDefaultDiskRebuildPreservesRevisionsAndIdempotency(t *testing.T) {
	r, bootstrap, _ := openBootstrappedRuntime(t)
	defer r.Close()
	ctx := context.Background()
	if r.governance.owner == nil || r.governance.manager.State() != nil {
		t.Fatal("default Governance still retains State")
	}
	project, err := r.store.GetProject(ctx, bootstrap.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	definition := domain.OutcomeDefinition{ID: "odef_defaultdisk", ProjectID: project.ID, Name: "disk", Version: 1,
		DataType: domain.OutcomeCategorical, AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: true, CreatedAt: now, CreatedBy: "admin"}
	if _, err := r.store.PutOutcomeDefinition(ctx, definition, project.Revision, 0, nil); err != nil {
		t.Fatal(err)
	}
	unit, _, err := r.accounting.CreateWorkUnitWithDefinitions(ctx, project.ID, bootstrap.KeyID, 10,
		[]domain.OutcomeDefinitionRef{{ID: definition.ID, Version: 1}}, budget.GovernanceIntent{Operation: "create_work_unit", IdempotencyKeyHash: outcomePageTestLabel("default-wu"), RequestFingerprint: outcomePageTestLabel("default-wu-fp")})
	if err != nil {
		t.Fatal(err)
	}
	input := governance.ReportInput{ProjectID: project.ID, WorkUnitID: unit.ID, DefinitionID: definition.ID, ReporterKeyID: bootstrap.KeyID, Value: "accepted", ObservedAt: now,
		Intent: governance.Intent{IdempotencyKeyHash: outcomePageTestLabel("first"), RequestFingerprint: outcomePageTestLabel("first-fp")}}
	first, _, err := r.governance.manager.Report(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	secondInput := input
	secondInput.Value, secondInput.SupersedesOutcomeID = "rejected", first.ID
	secondInput.Intent = governance.Intent{IdempotencyKeyHash: outcomePageTestLabel("second"), RequestFingerprint: outcomePageTestLabel("second-fp")}
	second, _, err := r.governance.manager.Report(ctx, secondInput)
	if err != nil {
		t.Fatal(err)
	}
	wantHead := r.governance.log.Summary()
	old := r.governance.owner.projection
	path := standaloneGovernanceProjectionPath(r.config)
	// Old derived checkpoint content must never be decoded into resident State.
	payload := []byte("invalid legacy snapshot")
	if err := r.store.SaveGovernanceCheckpoint(wantHead.Records, wantHead.Bytes, wantHead.LastHash, sha256.Sum256(payload), [32]byte{1}, payload); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := old.ReadHead(ctx); err == nil {
		t.Fatal("closed physical Governance owner still usable")
	}
	if err := os.WriteFile(path, []byte("untrusted crash derivative"), 0o600); err != nil {
		t.Fatal(err)
	}
	next, err := Open(ctx, r.config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.governance.owner == nil || next.governance.manager.State() != nil || next.governance.log.Summary() != wantHead {
		t.Fatal("restart selected wrong owner or changed native facts")
	}
	replayed, duplicate, err := next.governance.manager.Report(ctx, input)
	if err != nil || !duplicate || !reflect.DeepEqual(replayed, first) || next.governance.log.Summary() != wantHead {
		t.Fatal("restart lost durable idempotency", replayed, duplicate, err)
	}
	current, found, err := next.governance.owner.projection.ReadCurrent(ctx, project.ID, unit.ID, definition.ID, 1)
	if err != nil || !found || current.ID != second.ID || current.Revision != 2 {
		t.Fatal("restart lost current revision", current, found, err)
	}
}

func TestStandaloneGovernanceReservedPathRefusesConfiguredAuthority(t *testing.T) {
	for _, kind := range []string{"metadata", "master", "metadata-hardlink", "tls-key"} {
		t.Run(kind, func(t *testing.T) {
			cfg := testConfig(t)
			if kind == "metadata" {
				cfg.Storage.MetadataFile = "governance-projection.db"
			}
			if err := Initialize(cfg); err != nil {
				t.Fatal(err)
			}
			path := standaloneGovernanceProjectionPath(cfg)
			switch kind {
			case "master", "tls-key":
				raw, err := os.ReadFile(cfg.Storage.MasterKey.File)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if kind == "master" {
					cfg.Storage.MasterKey.File = path
				} else {
					cfg.TLS.Certificates = []config.TLSCertificate{{KeyFile: path}}
				}
			case "metadata-hardlink":
				if err := os.Link(cfg.MetadataPath(), path); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if runtime, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil || runtime != nil {
				t.Fatal("authority collision was accepted", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("configured authority was modified", err)
			}
		})
	}
}

func TestStandaloneDerivedPathRefusesNativeSymlinkIntoReservedTarget(t *testing.T) {
	for _, owner := range []string{"ledger", "governance"} {
		for _, native := range []string{"ledger", "governance", "audit", "lock", "publication-lock", "ledger-generation"} {
			t.Run(owner+"-"+native, func(t *testing.T) {
				cfg := testConfig(t)
				if err := Initialize(cfg); err != nil {
					t.Fatal(err)
				}
				reserved := standaloneProjectionPath(cfg)
				if owner == "governance" {
					reserved = standaloneGovernanceProjectionPath(cfg)
				}
				paths := map[string]string{"ledger": cfg.LedgerPath(), "governance": cfg.GovernancePath(), "audit": cfg.AuditPath(), "lock": filepath.Join(cfg.Storage.DataDir, ".halro.lock"), "publication-lock": lock.PublicationPath(cfg.Storage.DataDir), "ledger-generation": filepath.Join(filepath.Dir(cfg.LedgerPath()), "sealed-history.wal")}
				path := paths[native]
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				before := []byte("native bytes must survive rejected startup")
				if err := os.WriteFile(reserved, before, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if err := os.Symlink(reserved, path); err != nil {
					t.Fatal(err)
				}
				if r, err := Open(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); r != nil || err == nil {
					t.Fatal("native symlink into reserved target was accepted", err)
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, before) {
					t.Fatal("native target was removed or changed", err)
				}
			})
		}
	}
}

func TestStandaloneDerivedPathPreservesAuthenticatedGovernanceSymlinkTarget(t *testing.T) {
	for _, owner := range []string{"ledger", "governance"} {
		t.Run(owner, func(t *testing.T) {
			r, _, _ := openBootstrappedRuntime(t)
			defer r.Close()
			ctx := context.Background()
			record, err := r.governance.log.Append(ctx, governanceRecoveryEvent("out_first", "accepted", "", "one", 1))
			if err != nil {
				t.Fatal(err)
			}
			if err := r.governance.owner.projection.Apply(ctx, record); err != nil {
				t.Fatal(err)
			}
			head, authentication := r.governance.log.AuthenticatedHead()
			if err := r.store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, authentication); err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			path := r.config.GovernancePath()
			reserved := standaloneGovernanceProjectionPath(r.config)
			if owner == "ledger" {
				reserved = standaloneProjectionPath(r.config)
			}
			if err := os.Rename(path, reserved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(reserved, path); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Per-frame authentication and the external anchor are valid. Refusal
			// must be about native ownership, before either reserved unlink.
			if next, err := Open(ctx, r.config, slog.New(slog.NewTextHandler(io.Discard, nil))); next != nil || err == nil {
				if next != nil {
					next.Close()
				}
				t.Fatal("authenticated native target was discarded", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("native authenticated bytes did not survive", err)
			}
		})
	}
}

func TestStandaloneDerivedAuthorityScanCancellationDoesNotAuthorizeDiscard(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	reserved := standaloneGovernanceProjectionPath(cfg)
	before := []byte("saved derivative retained on canceled preparation")
	if err := os.WriteFile(reserved, before, 0o600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(cfg.LedgerPath())
	for i := range 100 {
		if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("history-%03d", i)), []byte("history"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	visited := 0
	err := checkDerivedNativeDirectory(ctx, directory, reserved, func(path string) error {
		if _, err := os.Stat(path); err != nil {
			return err
		}
		visited++
		if visited == 2 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || visited != 2 {
		t.Fatal("canceled directory scan continued or authorized partial success", visited, err)
	}
	if r, err := Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); r != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled validation entered startup mutations", r, err)
	}
	after, err := os.ReadFile(reserved)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("canceled scan discarded saved file", err)
	}
}

func TestStandaloneGovernanceAnchorRejectsTruncatedOrMissingEvidence(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "truncated", true: "unanchored"}[missing], func(t *testing.T) {
			store, log, path, key := openGovernanceRecoveryFixture(t)
			defer store.Close()
			defer log.Close()
			if _, err := log.Append(context.Background(), governanceRecoveryEvent("out_first", "accepted", "", "one", 1)); err != nil {
				t.Fatal(err)
			}
			firstEnd := log.Summary().Bytes
			if !missing {
				if _, err := log.Append(context.Background(), governanceRecoveryEvent("out_second", "rejected", "out_first", "two", 2)); err != nil {
					t.Fatal(err)
				}
				head, authentication := log.AuthenticatedHead()
				if err := store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, authentication); err != nil {
					t.Fatal(err)
				}
				if err := log.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(path, firstEnd); err != nil {
					t.Fatal(err)
				}
				var err error
				log, err = governance.Open(path, key)
				if err != nil {
					t.Fatal(err)
				}
				defer log.Close()
			}
			cfg := config.Config{}
			cfg.Storage.DataDir = filepath.Dir(path)
			if owner, err := prepareStandaloneGovernance(context.Background(), cfg, store, log, key, nil); owner != nil || !errors.Is(err, errGovernanceAnchorMismatch) {
				t.Fatal("lost external anchor protection", owner, err)
			}
		})
	}
}

func TestStandaloneGovernanceFailedOpenJoinsPhysicalReaderBeforeUnlock(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Usage.ExportFormat = "invalid-export-format"
	entered, release, cleanup := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	readerDone := make(chan error, 1)
	openDone := make(chan error, 1)
	go func() {
		_, err := OpenWithOptions(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), OpenOptions{AllowGeneratedSetupToken: true,
			governancePrepared: func(p *governance.GovernanceProjection) func() {
				ctx := &blockedProjectionReadContext{Context: context.Background(), entered: entered, release: release}
				go func() { _, err := p.ReadHead(ctx); readerDone <- err }()
				<-entered
				return func() { close(cleanup) }
			}})
		openDone <- err
	}()
	select {
	case <-cleanup:
	case <-time.After(5 * time.Second):
		t.Fatal("failed startup did not reach Governance cleanup")
	}
	good := cfg
	good.Usage.ExportFormat = "parquet"
	if runtime, err := Open(context.Background(), good, slog.New(slog.NewTextHandler(io.Discard, nil))); runtime != nil || !errors.Is(err, lock.ErrAlreadyLocked) {
		t.Fatal("directory lock released before actual Governance reader", runtime, err)
	}
	select {
	case err := <-openDone:
		t.Fatal("startup cleanup did not join actual reader", err)
	default:
	}
	once.Do(func() { close(release) })
	if err := <-readerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-openDone; err == nil {
		t.Fatal("invalid exporter accepted")
	}
	if _, err := os.Stat(standaloneGovernanceProjectionPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed startup kept Governance derivative", err)
	}
	if runtime, err := Open(context.Background(), good, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	} else {
		runtime.Close()
	}
}

func TestStandaloneGovernanceClosePreservesReplacementPath(t *testing.T) {
	r, _, _ := openBootstrappedRuntime(t)
	defer r.Close()
	path := standaloneGovernanceProjectionPath(r.config)
	if err := os.Rename(path, path+".owned"); err != nil {
		t.Fatal(err)
	}
	replacement := []byte("external replacement must survive")
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err == nil {
		t.Fatal("replacement path was silently removed")
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, replacement) {
		t.Fatal("closed owner removed a different file", err)
	}
}

func TestStandaloneGovernanceCorruptJournalDoesNotPublishEmptyGovernance(t *testing.T) {
	r, _, _ := openBootstrappedRuntime(t)
	defer r.Close()
	ctx := context.Background()
	record, err := r.governance.log.Append(ctx, governanceRecoveryEvent("out_first", "accepted", "", "one", 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.governance.owner.projection.Apply(ctx, record); err != nil {
		t.Fatal(err)
	}
	head, authentication := r.governance.log.AuthenticatedHead()
	if err := r.store.PutGovernanceJournalAnchor(head.Records, head.Bytes, head.LastHash, authentication); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	path := r.config.GovernancePath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	next, err := Open(ctx, r.config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if ready, err := next.governance.manager.Ready(); ready || err == nil || next.governance.owner != nil {
		t.Fatal("corrupt native Governance published a usable empty projection", ready, err)
	}
	// Preserve the established contract: corruption seals Governance, while
	// independent ordinary accounting still admits without a provider call.
	request, err := next.accounting.BeginRequestDetailed(ctx, "project_ordinary", "key_ordinary", "req_ordinary", "chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := next.accounting.Finalize(ctx, request, "success"); err != nil {
		t.Fatal(err)
	}
}
