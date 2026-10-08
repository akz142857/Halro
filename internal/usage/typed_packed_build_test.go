package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestPackedTypedUsageMatchesOrdinaryReducerAndAuthenticatesCompleteCut(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		late   bool
		prefix int
	}{{"ordinary", false, 0}, {"late", true, 0}, {"excluded-tail", true, 3}, {"empty", false, -1}} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newTypedUsageFixtureAt(t, scenario.late, scenario.prefix)
			o := f.options
			o.ScratchMaxDiskBytes = 32 << 20
			o.ScratchPath = filepath.Join(t.TempDir(), "private.reducer.db")
			before := o.Budget.Snapshot().Used
			packed, ref, err := BuildPackedTypedUsageProjection(context.Background(), o, f.lease)
			if err != nil {
				t.Fatal(err)
			}
			defer packed.Discard()
			if _, err := os.Lstat(o.ScratchPath); !os.IsNotExist(err) {
				t.Fatal("private scratch remained", err)
			}
			if packed.db != nil || packed.file != nil || ref.Projection.Schema.Codec != TypedUsageSealCodec {
				t.Fatal("retained ordinary reducer or changed codec")
			}
			identity, err := packed.engine.cow.RootIdentity(context.Background())
			if err != nil || identity.Revision != 1 {
				t.Fatal("initial pack contains COW rewrite history", identity, err)
			}
			ordinary, err := CreateDiskProjection(filepath.Join(t.TempDir(), "ordinary.db"), f.owner)
			if err != nil {
				t.Fatal(err)
			}
			defer ordinary.Discard()
			for _, r := range f.records {
				if r.Sequence > ref.Ledger.Anchor.Head.Sequence {
					continue
				}
				if err := ordinary.ApplyContext(context.Background(), r); err != nil {
					t.Fatal(err)
				}
			}
			if err := alignPackedUsageBoundary(context.Background(), ordinary, ref.Ledger.Anchor); err != nil {
				t.Fatal(err)
			}
			compareTypedUsageRows(t, packed, ordinary)
			if err := packed.Discard(); err != nil {
				t.Fatal(err)
			}
			if after := o.Budget.Snapshot().Used; after != before {
				t.Fatal("scratch/root workspace did not retire", before, after)
			}
		})
	}
}

func TestPackedTypedUsageRejectsForeignScratchWithoutChargingSurvivingFiles(t *testing.T) {
	f := newTypedUsageFixture(t, false)
	o := f.options
	o.ScratchMaxDiskBytes = 32 << 20
	foreign := []byte("saved bytes are not a construction authority")
	if err := os.WriteFile(o.ScratchPath, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	before := o.Budget.Snapshot().Used
	p, _, err := BuildPackedTypedUsageProjection(context.Background(), o, f.lease)
	if err == nil || p != nil {
		t.Fatal("accepted preexisting scratch", p, err)
	}
	actual, err := os.ReadFile(o.ScratchPath)
	if err != nil || !bytes.Equal(actual, foreign) || o.Budget.Snapshot().Used != before {
		t.Fatal("foreign scratch or fees changed", actual, err, o.Budget.Snapshot())
	}
}

func TestPackedTypedUsageAuthenticatesExcludedNativeTailBeforeScratch(t *testing.T) {
	f := newTypedUsageFixtureAt(t, true, 3)
	o := f.options
	o.ScratchMaxDiskBytes = 32 << 20
	before := o.Budget.Snapshot().Used
	source, err := os.OpenFile(filepath.Join(filepath.Dir(o.ScratchPath), "native.wal"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var value [1]byte
	offset := f.records[len(f.records)-1].Offset - 1
	if _, err := source.ReadAt(value[:], offset); err != nil {
		t.Fatal(err)
	}
	value[0] ^= 0x40
	if _, err := source.WriteAt(value[:], offset); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	p, ref, err := BuildPackedTypedUsageProjection(context.Background(), o, f.lease)
	if err == nil || p != nil || ref != (TypedUsageReference{}) {
		t.Fatal("accepted corrupt excluded tail", p, ref, err)
	}
	if _, err := os.Lstat(o.ScratchPath); !os.IsNotExist(err) {
		t.Fatal("scratch created before complete authentication", err)
	}
	if o.Budget.Snapshot().Used != before {
		t.Fatal("known failed source job leaked fees", o.Budget.Snapshot())
	}
}

func TestPackedTypedUsageThousandRequestsFitFixed64MiBInitialPack(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	key := bytes.Repeat([]byte{0x67}, 32)
	path := filepath.Join(directory, "ledger.wal")
	native, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: key, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	for request := 0; request < 1000; request++ {
		for _, r := range finalizationFixture(request%2 == 0, false) {
			e := r.Event
			e.EventID = fmt.Sprintf("%s_%d", e.EventID, request)
			e.RequestID = fmt.Sprintf("req_%d", request)
			if e.AttemptID != "" {
				e.AttemptID = fmt.Sprintf("att_%d", request)
			}
			if _, err := native.Append(ctx, e); err != nil {
				t.Fatal(request, err)
			}
		}
	}
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 512 << 20, Disk: 256 << 20}, MaxClaims: 48, MaxOwners: 8, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 48 * resourcebudget.ClaimRAMBytes}
	manager, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	report, partial, err := ledger.InspectReplayAuthenticated(path, key, nil)
	if err != nil || partial {
		t.Fatal(report, partial, err)
	}
	owner, err := ledger.BuildPackedAuthenticatedLedgerProjection(ctx, path, key, filepath.Join(directory, "ledger-root"), report, ledger.AuthenticatedLedgerOptions{Identity: sha256.Sum256([]byte("packed-large-ledger")), MaxDiskBytes: 64 << 20, MaxAppendBytes: 16 << 20, Budget: manager})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Discard()
	lease, err := native.BeginMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	packed, ref, err := BuildPackedTypedUsageProjection(ctx, AuthenticatedUsageOptions{Native: native, Owner: owner, Identity: sha256.Sum256([]byte("packed-large-usage")), UsageKey: key, ScratchPath: filepath.Join(directory, "usage.reducer.db"), ScratchMaxDiskBytes: 64 << 20, RootDirectory: filepath.Join(directory, "usage-root"), RootMaxDiskBytes: 64 << 20, RootMaxAppendBytes: 16 << 20, Budget: manager}, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer packed.Discard()
	stats, err := packed.ReadStats(ctx)
	identity, rootErr := packed.engine.cow.RootIdentity(ctx)
	if err != nil || rootErr != nil || identity.Revision != 1 || ref.Projection.Root.State.Extent >= 64<<20 || stats.Totals.Attempts != 1000 || stats.Metrics.ActiveRequests != 0 || ref.Ledger.Anchor.Head.Sequence != 5000 {
		t.Fatal(stats, identity, ref, err, rootErr)
	}
	var ordinary *DiskProjection
	err = owner.WithOfflineMaintenanceUsageSource(ctx, native, lease, manager, ref.Ledger.Scope, func(ctx context.Context, source ledger.AuthenticatedUsageSource) error {
		var err error
		ordinary, err = CreateDiskProjectionWithLimit(filepath.Join(directory, "ordinary.db"), packedUsageSource{source}, 64<<20)
		if err != nil {
			return err
		}
		return replayPackedUsageScratch(ctx, source, ordinary, ref.Ledger.Anchor)
	})
	if ordinary != nil {
		defer ordinary.Discard()
	}
	if err != nil {
		t.Fatal(err)
	}
	compareTypedUsageRows(t, packed, ordinary)
	t.Logf("1000 requests, 5000 native facts; initial pack extent=%d rows=%d revision=%d", ref.Projection.Root.State.Extent, identity.Rows, identity.Revision)
}

func TestPackedTypedUsageRestoredActualInodeRetiresConstructionDebt(t *testing.T) {
	for _, namespace := range []string{"scratch", "candidate"} {
		t.Run(namespace, func(t *testing.T) {
			f := newTypedUsageFixture(t, false)
			o := f.options
			o.ScratchMaxDiskBytes = 32 << 20
			before := o.Budget.Snapshot().Used
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := o.ScratchPath
			if namespace == "candidate" {
				path = o.RootDirectory
			}
			moved := path + ".actual-owner"
			foreign := []byte("preserve foreign bytes")
			p, ref, err := buildPackedTypedUsageProjection(ctx, o, f.lease, func(phase string) {
				if phase != "before-publication" {
					return
				}
				if err := os.Rename(path, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, foreign, 0600); err != nil {
					t.Fatal(err)
				}
				cancel()
			})
			var debt *PackedUsageBuildRetirement
			if !errors.Is(err, ownedfile.ErrRetirement) || !errors.Is(err, ownedfile.ErrReplaced) || !errors.As(err, &debt) || p == nil || ref != (TypedUsageReference{}) {
				t.Fatal("unresolved actual owner was discarded", p, ref, err)
			}
			charged := o.Budget.Snapshot().Used
			if charged.Disk <= before.Disk || charged.RAM <= before.RAM {
				t.Fatal("unresolved derivative refunded construction", before, charged)
			}
			if err := debt.Retire(); err == nil || o.Budget.Snapshot().Used != charged {
				t.Fatal("repeat failed retire lost fees", err, o.Budget.Snapshot())
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(got, foreign) {
				t.Fatal("foreign bytes deleted", got, readErr)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(moved, path); err != nil {
				t.Fatal(err)
			}
			if err := debt.Retire(); err != nil || o.Budget.Snapshot().Used != before {
				t.Fatal("restored actual inode did not retire/refund", err, o.Budget.Snapshot())
			}
			if err := debt.Retire(); err != nil || o.Budget.Snapshot().Used != before {
				t.Fatal("successful retirement not idempotent", err, o.Budget.Snapshot())
			}
		})
	}
}
