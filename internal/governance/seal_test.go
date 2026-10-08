package governance

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func governanceSealBudget(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 512 << 20, Disk: 128 << 20}, MaxClaims: 32, MaxOwners: 16, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: int64(limits.MaxClaims) * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAuthenticatedGovernanceRootBindsNativeAndCompleteProjectionAtOneCut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "governance.wal")
	key := bytes.Repeat([]byte{0x52}, governanceHMACKeySize)
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	for _, event := range []Event{projectionEvent(21), projectionEvent(22)} {
		if _, err := log.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	budget := governanceSealBudget(t)
	projectionCredit, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	root, err := BuildAuthenticatedGovernanceProjection(ctx, AuthenticatedGovernanceOptions{
		Native: log, Identity: [32]byte{1}, RootKey: bytes.Repeat([]byte{0x83}, 32),
		ProjectionPath: filepath.Join(dir, "projection.db"), RootDirectory: filepath.Join(dir, "authenticated"),
		ProjectionMaxDiskBytes: 16 << 20, RootMaxDiskBytes: 32 << 20, RootMaxAppendBytes: 4 << 20,
		ProjectionDiskReservation: projectionCredit, Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if root.Journal.Position.Component != authenticatedstate.SealComponentGovernance ||
		root.Projection.Schema != (authenticatedstate.SealRootSchema{Kind: authenticatedstate.SealRootState, Component: authenticatedstate.SealComponentGovernance, IndexID: AuthenticatedGovernanceProjectionSealIndexID, Codec: AuthenticatedGovernanceProjectionSealCodec}) ||
		root.Projection.Root.Rows == 0 || root.Projection.Position != root.Journal.Position {
		t.Fatalf("Governance roots are not complete and bound to one native cut: %+v", root)
	}
	if _, err := os.Stat(filepath.Join(dir, "projection.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary reducer survived successful root publication: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	identity := [32]byte{1}
	rootKey := bytes.Repeat([]byte{0x83}, 32)
	reservation, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 20})
	if err != nil {
		t.Fatal(err)
	}
	selected := root.Projection
	wrong := selected
	wrong.Root.Digest[0] ^= 1
	if reopened, err := OpenAuthenticatedGovernanceProjection(ctx, filepath.Join(dir, "authenticated"), identity, rootKey, root.Journal, wrong, 32<<20, 4<<20, reservation, budget); err == nil || reopened != nil {
		t.Fatalf("reopen accepted a non-selected Governance root: reopened=%v err=%v", reopened, err)
	}
	reopened, err := OpenAuthenticatedGovernanceProjection(ctx, filepath.Join(dir, "authenticated"), identity, rootKey, root.Journal, selected, 32<<20, 4<<20, reservation, budget)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Projection != selected || reopened.Journal != root.Journal {
		t.Fatal("reopen changed the selected Governance receipts")
	}
	imagePath := filepath.Join(dir, "restored-governance.db")
	p, err := reopened.RestoreProjection(ctx, imagePath, log, 16<<20, projectionCredit, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	head, err := p.ReadHead(ctx)
	if err != nil || head != log.Summary() {
		t.Fatalf("restored Governance cut mismatch: %+v %v", head, err)
	}
	for _, index := range []int{21, 22} {
		want := projectionEvent(index)
		got, found, err := p.ReadOutcome(ctx, want.OutcomeID)
		if err != nil || !found || got.ID != want.OutcomeID || got.Revision != want.Revision || got.Value != want.Value {
			t.Fatalf("restored Outcome differs: %+v %t %v", got, found, err)
		}
	}
	if _, err := reopened.RestoreProjection(ctx, imagePath, log, 16<<20, projectionCredit, budget); err == nil {
		t.Fatal("existing live file replaced")
	}
	record, err := log.Append(ctx, projectionEvent(23))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, record); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("out-of-order duplicate Apply accepted: %v", err)
	}
	head, err = p.ReadHead(ctx)
	if err != nil || head != log.Summary() {
		t.Fatalf("restored owner did not continue: %+v %v", head, err)
	}
	if err := p.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Release(); err != nil {
		t.Fatal(err)
	}
	if err := projectionCredit.Release(); err != nil {
		t.Fatal(err)
	}
	if got := budget.Snapshot().Used; got != (resourcebudget.Cost{}) {
		t.Fatal("authenticated Governance build leaked resource owners", got)
	}
}

func TestAppendBatchWithoutDurabilityCallbacksPersistsNativeFrames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "governance.wal")
	key := bytes.Repeat([]byte{0x19}, governanceHMACKeySize)
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	records, err := log.AppendBatch(context.Background(), []Event{projectionEvent(11), projectionEvent(12)})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	verified, err := Verify(path, key)
	if err != nil || len(records) != 2 || verified.Records != 2 || verified.Bytes <= 0 || verified.LastHash != records[1].Hash {
		t.Fatalf("native batch was not persisted: records=%d verified=%+v err=%v", len(records), verified, err)
	}
}

func TestGovernanceSealReferenceAuthenticatesExactPrefixAndLineage(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "governance.wal")
	key := bytes.Repeat([]byte{0x7a}, governanceHMACKeySize)
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, err := log.Append(ctx, projectionEvent(1)); err != nil {
		t.Fatalf("append first Governance event: %+v", err)
	}
	cut := log.Summary()
	if _, err := log.Append(ctx, projectionEvent(2)); err != nil {
		t.Fatal(err)
	}
	budget := governanceSealBudget(t)
	ref, err := log.SealReference(ctx, budget, cut)
	if err != nil {
		t.Fatal(err)
	}
	wantSchema := authenticatedstate.SealRootSchema{Component: authenticatedstate.SealComponentGovernance, IndexID: AuthenticatedGovernanceSealIndexID, Codec: AuthenticatedGovernanceSealCodec}
	if ref.Schema != wantSchema || ref.Root.Component != wantSchema.Component || ref.Root.Rows != cut.Records || ref.Position.Sequence != cut.Records || ref.Position.Offset != uint64(cut.Bytes) || ref.Position.Hash != cut.LastHash || ref.Root.Offset+uint64(ref.Root.Length) != uint64(cut.Bytes) {
		t.Fatalf("seal reference does not describe the requested prefix: %+v", ref)
	}
	if ref.Root.FileID == ([16]byte{}) || ref.Root.Digest != cut.LastHash {
		t.Fatal("nonempty root is missing chain lineage or final-frame commitment", ref.Root)
	}
	if got := budget.Snapshot().Used; got != (resourcebudget.Cost{}) {
		t.Fatal("seal scan leaked budget owner", got)
	}
}

func TestGovernanceSealReferenceUsesExplicitEmptyRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "governance.wal")
	log, err := Open(path, bytes.Repeat([]byte{0x28}, governanceHMACKeySize))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	budget := governanceSealBudget(t)
	ref, err := log.SealReference(context.Background(), budget, Summary{})
	if err != nil {
		t.Fatal(err)
	}
	if !ref.Root.Empty || ref.Root.Rows != 0 || ref.Root.Digest == ([32]byte{}) || ref.Position != (authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentGovernance}) {
		t.Fatal("empty Governance root is not explicit", ref)
	}
	if got := budget.Snapshot().Used; got != (resourcebudget.Cost{}) {
		t.Fatal("empty seal scan leaked budget owner", got)
	}
}

func TestGovernanceSealReferenceRejectsTamperAndForeignPath(t *testing.T) {
	for _, mode := range []string{"tamper", "replace"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "governance.wal")
			log, err := Open(path, bytes.Repeat([]byte{0x4c}, governanceHMACKeySize))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			if _, err := log.Append(context.Background(), projectionEvent(1)); err != nil {
				t.Fatal(err)
			}
			expected := log.Summary()
			if mode == "tamper" {
				f, err := os.OpenFile(path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				b := []byte{0}
				if _, err := f.ReadAt(b, frameHeaderSize); err != nil {
					t.Fatal(err)
				}
				b[0] ^= 0xff
				if _, err := f.WriteAt(b, frameHeaderSize); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := log.SealReference(context.Background(), governanceSealBudget(t), expected); err == nil {
				t.Fatalf("SealReference(%s) accepted invalid source: %v", mode, err)
			}
		})
	}
}
