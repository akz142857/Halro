package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestHASelectedRuntimeFoundationActualBothRolesNeverOpensLegacyMetadataDB(t *testing.T) {
	fixture := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, fixture)
	completeHAMemoryActivationBoundary(t, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, cfg := range fixture.configs {
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		activated, err := ActivateHAMemoryRuntime(ctx, cfg, fixture.keys, guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := activated.Close(); err != nil {
			t.Fatal(err)
		}
		if err := guard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, cfg := range fixture.configs {
		t.Run(cfg.Replication.NodeID, func(t *testing.T) {
			// A native bbolt opener would fail ISDIR here. Preserve the actual old bytes;
			// their backup digest/native journal authority remains independently checked.
			if err := os.Rename(cfg.MetadataPath(), cfg.MetadataPath()+".preserved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(cfg.MetadataPath(), 0700); err != nil {
				t.Fatal(err)
			}
			guard, err := lock.Acquire(cfg.Storage.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := guard.Close(); err != nil {
					t.Error(err)
				}
			}()
			manager := functionalHAMetadataManager(t, cfg)
			foundation, err := openHASelectedRuntimeFoundation(ctx, cfg, fixture.keys.Master, guard, manager, logger)
			if err != nil {
				t.Fatal("actual selected Runtime foundation", err)
			}
			defer func() {
				if err := foundation.Close(); err != nil {
					t.Error(err)
				}
			}()
			c := foundation.owners.Components
			if c.NativeLedger == nil || c.NativeGovernance == nil || c.Audit == nil || c.Metadata == nil || c.NativeLedger.SelectedStartupReady() || c.NativeGovernance.SelectedStartupReady() || c.Audit.SelectedStartupReady() {
				t.Fatal("actual foundations skipped historical readiness gate")
			}
			if err := statecut.WithHASelectedCut(ctx, foundation.owners, func(ctx context.Context, cut statecut.HASelectedCut) error {
				if cut.Seal.RootCount != 7 || cut.Authority.Role == authenticatedstate.SealStandalone || cut.Authority.OrderingIndex != cut.AllowedIndex {
					t.Fatal("actual HA common permission", cut)
				}
				ref, err := c.Metadata.SealReference(ctx)
				if err != nil || ref.Root != cut.Seal.Roots[1] {
					t.Fatal("candidate replay granted another Metadata root", ref, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			la, err := c.NativeLedger.StartSelectedStartupAudit(ctx, manager, ledger.StartupAuditOptions{Deadline: time.Second * 10, ProgressTimeout: time.Second * 2})
			if err != nil {
				t.Fatal(err)
			}
			aa, err := c.Audit.StartSelectedStartupAudit(ctx, manager, audit.StartupAuditOptions{Deadline: time.Second * 10, ProgressTimeout: time.Second * 2})
			if err != nil {
				t.Fatal(err)
			}
			ga, err := c.NativeGovernance.StartSelectedStartupAudit(ctx, manager, governance.StartupAuditOptions{Deadline: time.Second * 10, ProgressTimeout: time.Second * 2})
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range []*audit.StartupAudit{la, aa, ga} {
				if err := a.Wait(ctx); err != nil {
					t.Fatal(err)
				}
			}
			// Historical authentication and the actual maintenance/admission owner
			// remain separate: completing the former does not close the latter.
			if replicated, err := c.Lease.ValidateOwner(c.NativeLedger); err != nil || !replicated {
				t.Fatal("actual replicated maintenance owner disappeared", replicated, err)
			}
			entered, release, parentDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				parentDone <- foundation.withSelectedCut(ctx, func(scope context.Context, _ statecut.HASelectedCut) error {
					go func() {
						_ = foundation.withSelectedCut(scope, func(context.Context, statecut.HASelectedCut) error { close(entered); <-release; return nil })
					}()
					<-entered
					return nil
				})
			}()
			<-entered
			select {
			case err := <-parentDone:
				t.Fatal("actual Member freeze released before borrowed callback joined", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			if err := <-parentDone; err != nil {
				t.Fatal("joined actual permission borrower", err)
			}
			var escaped context.Context
			if err := c.NativeLedger.WithHASelectedSealAuthority(ctx, func(scope context.Context, _ authenticatedstate.SealEligibility, _ ledger.HASelectedLedgerPosition) error {
				escaped = scope
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := c.NativeLedger.WithHASelectedSealAuthority(escaped, func(context.Context, authenticatedstate.SealEligibility, ledger.HASelectedLedgerPosition) error {
				t.Fatal("expired Member/source scope reactivated")
				return nil
			}); !errors.Is(err, ledger.ErrProjectionSource) && !errors.Is(err, context.Canceled) {
				t.Fatal("escaped authority", err)
			}
			reopened, err := foundation.reopenSelectedGeneration(ctx)
			if err != nil {
				t.Fatal("actual seven selected derived owners", err)
			}
			roots := reopened.Owners.Components
			if roots.Ledger == nil || roots.Usage == nil || roots.Governance == nil || roots.Directory == nil || roots.Metadata == nil {
				t.Fatal("missing actual selected derived owner")
			}
			if err := reopened.ResumeSelectedMetadata(ctx); err != nil {
				t.Fatal("actual Metadata source handoff", err)
			}
			store, err := reopened.OpenHAMetadataStore(ctx)
			if err != nil {
				t.Fatal("actual selected Metadata DAO", err)
			}
			if _, err := store.ListProjects(ctx); err != nil {
				t.Fatal("actual DAO read under independent permission", err)
			}
			if err := roots.Usage.ActivateTypedLive(ctx, c.NativeLedger, manager); err == nil {
				t.Fatal("Standalone live source admitted HA owner")
			}
			if err := roots.Usage.ActivateHASelectedTypedLive(ctx, c.NativeLedger, manager); err != nil {
				t.Fatal("actual HA Usage startup source", err)
			}
			if err := c.Lease.Close(); err != nil {
				t.Fatal("actual startup lease join", err)
			}
			if _, err := roots.Usage.ReadStats(ctx); err != nil {
				t.Fatal("actual HA live Usage read permission", err)
			}
			if _, err := reopened.Derivative.CatchUpTypedLive(ctx, c.NativeLedger, manager); err != nil {
				t.Fatal("actual HA live Usage complete scoped job", err)
			}
			if _, err := os.Stat(cfg.MetadataPath()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
