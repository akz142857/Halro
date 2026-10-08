package statecut

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/usage"
)

func TestCommonSelectedUsageRestoresRealRootAndOnlyNativeDelta(t *testing.T) {
	for _, damageOld := range []bool{false, true} {
		t.Run(map[bool]string{false: "actual-delta", true: "old-damage-no-delta"}[damageOld], func(t *testing.T) {
			ctx := context.Background()
			f := newCommonFixture(t)
			o := prepareCommonReopen(t, f)
			selected, found, err := f.owners.Publisher.Select(f.authority)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			if err := f.owners.Lease.Close(); err != nil {
				t.Fatal(err)
			}
			if !damageOld {
				for i, kind := range []ledger.EventKind{ledger.EventRequestAccepted, ledger.EventReservationCreated, ledger.EventAttemptStarted, ledger.EventAttemptSettled, ledger.EventRequestFinalized} {
					e := ledger.Event{EventID: []string{"delta_accept", "delta_reserve", "delta_start", "delta_settle", "delta_final"}[i], Kind: kind, RequestID: "req_delta", ProjectID: "prj_common", PeriodID: "2026-10-06", PeriodTimezone: "UTC", PeriodTimezoneVersion: 1, OccurredAt: commonGovernanceEvent().IngestedAt}
					if i >= 1 && i <= 3 {
						e.AttemptID = "att_delta"
					}
					if i == 1 {
						v := int64(20)
						e.ReservationMicrosUSD = &v
					}
					if i == 3 {
						v := int64(12)
						e.CommittedMicrosUSD = &v
						e.Outcome = "success"
						e.ProviderInputTokens = 10
					}
					if _, err := f.owners.NativeLedger.Append(ctx, e); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := f.owners.NativeLedger.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.dir, "ledger.wal")
			native, nr, err := ledger.OpenWithSelectedSeal(ctx, path, ledger.NewStatus(), ledger.Options{ChainKey: f.key}, ledger.SelectedSealOpenOptions{Publisher: f.owners.Publisher, Guard: f.owners.Guard, Authority: f.authority, Directory: o.DirectoryPath, DirectoryOptions: o.DirectoryOptions})
			if err != nil {
				t.Fatal(err)
			}
			defer native.Close()
			lease, err := native.BeginMaintenance(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			so := ledger.SelectedLedgerProjectionOpenOptions{Publisher: f.owners.Publisher, Guard: f.owners.Guard, Authority: f.authority}
			lo := ledger.AuthenticatedLedgerOpenOptions{AuthenticatedLedgerOptions: o.LedgerOptions, SavedAnchor: nr.Saved, DiskReservation: o.LedgerDisk}
			lo.Prefix = &ledger.ProjectionPrefix{Head: nr.Head.Head, Hash: nr.Head.Hash}
			owner, lr, err := ledger.OpenSelectedAuthenticatedLedgerProjection(ctx, native, o.LedgerDirectory, lo, so)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := owner.Close(); err != nil {
					t.Error(err)
				}
				if err := f.owners.Ledger.AdoptClosedSuccessor(owner); err != nil {
					t.Error(err)
				}
			}()
			if damageOld {
				file, err := os.OpenFile(path, os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				var raw [1]byte
				if _, err := file.ReadAt(raw[:], 64); err != nil {
					t.Fatal(err)
				}
				raw[0] ^= 1
				if _, err := file.WriteAt(raw[:], 64); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var savedLedger ledger.AuthenticatedLedgerSealReference
			if err := native.WithSelectedStartupOwner(ctx, o.Sources.Budget, so, f.authority.Instance, func(ctx context.Context, source ledger.SelectedStartupSource) error {
				ref, err := owner.SealReference(ctx)
				if err != nil {
					return err
				}
				savedLedger = ref
				savedLedger.Root = selected.Roots[0]
				savedLedger.Anchor = nr.Saved
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			uo := o.UsageOptions
			uo.Native, uo.Owner = native, owner
			ur := usage.TypedUsageReference{Ledger: savedLedger, Projection: usage.AuthenticatedUsageSealReference{Schema: HardLimits().RequiredRoots[5], Root: selected.Roots[5], Scope: selected.Roots[5].State.Scope, Position: selected.Positions[4]}}
			p, err := usage.OpenSelectedTypedUsageProjection(ctx, uo, lease, ur, o.UsageDisk, so)
			if err != nil {
				t.Fatal("actual selected Usage initialization", err)
			}
			defer func() {
				if err := p.Close(); err != nil {
					t.Error(err)
				}
				if err := f.owners.Usage.AdoptClosedTypedSuccessor(p); err != nil {
					t.Error(err)
				}
			}()
			dr := ledger.AuthenticatedDirectoryReference{Schema: HardLimits().RequiredRoots[6], Root: selected.Roots[6], Position: selected.Positions[0]}
			directory, err := ledger.OpenSelectedAuthenticatedDirectory(ctx, native, dr, o.DirectoryPath, o.DirectoryOptions, so)
			if err != nil {
				t.Fatal("actual selected Directory initialization", err)
			}
			defer func() {
				if err := directory.Close(); err != nil {
					t.Error(err)
				}
				if err := f.owners.Directory.AdoptClosedSuccessor(directory); err != nil {
					t.Error(err)
				}
			}()
			if _, err := directory.SealReference(ctx); !errors.Is(err, ledger.ErrStartupAuditPending) {
				t.Fatal("pending audit permitted Directory publication", err)
			}
			if _, err := p.ReadReplayAnchor(ctx); !errors.Is(err, ledger.ErrStartupAuditPending) {
				t.Fatal("pending grant allowed business query", err)
			}
			if damageOld {
				if nr.DecodedRecords != 0 || lr.AppliedRecords != 0 {
					t.Fatal(nr, lr)
				}
				if err := native.AuditSelectedStartup(ctx, o.Sources.Budget); err == nil {
					t.Fatal("historical damage hidden")
				}
				if _, err := p.ReadReplayAnchor(ctx); err == nil {
					t.Fatal("failed historical audit retained query eligibility")
				}
				return
			}
			if nr.DecodedRecords != 5 || lr.AppliedRecords != 5 {
				t.Fatal(nr, lr)
			}
			if err := native.AuditSelectedStartup(ctx, o.Sources.Budget); err != nil {
				t.Fatal(err)
			}
			if anchor, err := p.ReadReplayAnchor(ctx); err != nil || anchor != nr.Head {
				t.Fatal(anchor, nr.Head, err)
			}
			if ref, err := directory.SealReference(ctx); err != nil || ref.Position.Sequence != nr.Head.Head.Sequence || ref.Position.Hash != nr.Head.Hash {
				t.Fatal("Directory and Ledger cut diverged", ref, err)
			}
			if row, found, err := p.ReadRequest(ctx, "req_delta"); err != nil || !found || row.RequestID != "req_delta" {
				t.Fatal("actual new delta row", row, found, err)
			}
			if row, found, err := p.ReadRequest(ctx, "req_common"); err != nil || !found || row.RequestID != "req_common" {
				t.Fatal("actual saved row", row, found, err)
			}
			wrong := so
			wrong.Authority.Coverage = authenticatedstate.SealCoverageLegacy
			if other, err := usage.OpenSelectedTypedUsageProjection(ctx, uo, lease, ur, o.UsageDisk, wrong); other != nil || err == nil {
				t.Fatal("wrong authority qualified selected source", err)
			}
		})
	}
}
