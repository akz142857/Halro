package metadatastate

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func epochCandidate(t *testing.T) (*Baseline, BaselineOptions, []byte, string) {
	t.Helper()
	h, _, _ := baselineArchive(t, true, nil, nil)
	o := baselineOptions(t)
	key := bytes.Repeat([]byte{51}, 32)
	b, err := BuildFromBackup(context.Background(), h, key, privateSource(t), filepath.Join(t.TempDir(), "root"), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Discard(); err != nil {
			t.Error(err)
		}
	})
	return b, o, key, filepath.Join(filepath.Dir(privateSource(t)), "metadata.journal")
}

func TestEpochFactoryBindsRealNormalizedRootAndRetainsPersistentOwners(t *testing.T) {
	b, o, key, path := epochCandidate(t)
	original, err := b.store.RootIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	head, err := b.StartNewEpoch(context.Background(), key, path, 8, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.store.RootIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if head.Epoch != 8 || head.Sequence != 0 || head.FormatVersion != 2 || head.FileRevision != 1 || head.Baseline.Epoch != 8 || head.Baseline.RootDigest != root.Digest || head.Baseline.RootID != root.ID || head.Baseline.Rows != root.Rows || head.Baseline.Cut != root.Cut || root.Cut == original.Cut || root.Revision != original.Revision+1 {
		t.Fatal(head, root, original)
	}
	if b.Evidence().Metadata.MetadataJournalEpoch != 7 || b.Evidence().Metadata.MetadataJournalSequence != 13 {
		t.Fatal("source evidence was rewritten", b.Evidence())
	}
	pin, err := b.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		name  string
		value uint64
	}{{"metadata_journal_epoch", 8}, {"applied_journal_sequence", 0}} {
		k, err := RowKey([][]byte{[]byte("meta")}, []byte(entry.name), false)
		if err != nil {
			t.Fatal(err)
		}
		v, found, err := pin.Get(context.Background(), k)
		if err != nil || !found || len(v) != 8 || binary.BigEndian.Uint64(v) != entry.value {
			t.Fatal(entry, found, v, err)
		}
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := b.EpochHead(); err != nil || got != head {
		t.Fatal(got, err)
	}
	before := o.Budget.Snapshot()
	nativeBefore, _ := os.ReadFile(path)
	if _, err := b.StartNewEpoch(context.Background(), key, path+".again", 9, 1<<20); err == nil {
		t.Fatal("fresh transition repeated")
	}
	nativeAfter, _ := os.ReadFile(path)
	if !bytes.Equal(nativeBefore, nativeAfter) || o.Budget.Snapshot().Used != before.Used {
		t.Fatal("repeated transition changed native/fees")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if o.Budget.Snapshot().Used.Disk != o.MaxDiskBytes+1<<20 {
		t.Fatal("Close refunded persistent bytes", o.Budget.Snapshot())
	}
	journalKey, err := hkdf.Key(sha256.New, key, root.Scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(journalKey)
	if legacy, err := metadatajournal.Open(path, journalKey); err == nil || legacy != nil {
		t.Fatal("ordinary native Open bypassed binding", legacy, err)
	}
	reader, err := metadatajournal.OpenBoundContext(context.Background(), path, journalKey, head.Baseline, 1, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if reader.Head() != head {
		t.Fatal(reader.Head(), head)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Discard(); err != nil {
		t.Fatal(err)
	}
	if o.Budget.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal(o.Budget.Snapshot())
	}
}

func TestEpochFactoryPreflightRefusesWithoutChangingRootOrForeignFiles(t *testing.T) {
	for _, shape := range []string{"key", "epoch", "relative", "existing", "public parent", "disk", "pressure", "canceled"} {
		t.Run(shape, func(t *testing.T) {
			b, o, key, path := epochCandidate(t)
			epoch, limit := uint64(8), int64(1<<20)
			ctx := context.Background()
			beforeRoot, err := b.store.RootIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch shape {
			case "key":
				key = bytes.Repeat([]byte{1}, 32)
			case "epoch":
				epoch = 7
			case "relative":
				path = "metadata.journal"
			case "existing":
				if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			case "public parent":
				if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(filepath.Dir(path), 0700)
			case "disk":
				fee, err := o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 192 << 20})
				if err != nil {
					t.Fatal(err)
				}
				defer fee.Release()
			case "pressure":
				if err := o.Budget.SetPressure(resourcebudget.Pressured); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			before := o.Budget.Snapshot()
			if got, err := b.StartNewEpoch(ctx, key, path, epoch, limit); err == nil || got != (metadatajournal.Head{}) {
				t.Fatal("invalid transition accepted", got, err)
			}
			if shape == "pressure" {
				if err := o.Budget.SetPressure(resourcebudget.Healthy); err != nil {
					t.Fatal(err)
				}
			}
			afterRoot, err := b.store.RootIdentity(context.Background())
			if err != nil || afterRoot != beforeRoot {
				t.Fatal("preflight changed root", afterRoot, beforeRoot, err)
			}
			if !b.ready.Load() || o.Budget.Snapshot().Used != before.Used {
				t.Fatal("preflight retired input or altered fee", o.Budget.Snapshot())
			}
			if shape == "existing" {
				raw, _ := os.ReadFile(path)
				if string(raw) != "foreign" {
					t.Fatal("foreign journal replaced")
				}
			} else if filepath.IsAbs(path) {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("refused transition created journal", err)
				}
			}
		})
	}
}

func TestEpochFactoryCanceledPrivateStagesRetireCandidateUntilPhysicalDiscard(t *testing.T) {
	for _, phase := range []string{"root-normalized", "journal-durable"} {
		t.Run(phase, func(t *testing.T) {
			b, o, key, path := epochCandidate(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			head, err := b.startNewEpoch(ctx, key, path, 8, 1<<20, func(current string) {
				if current == phase {
					cancel()
				}
			})
			if !errors.Is(err, context.Canceled) || head != (metadatajournal.Head{}) || b.ready.Load() {
				t.Fatal(head, err, b.ready.Load())
			}
			if _, err := b.Snapshot(context.Background()); err == nil {
				t.Fatal("failed candidate remained readable")
			}
			if o.Budget.Snapshot().Classes[resourcebudget.IO] != (resourcebudget.Cost{}) || o.Budget.Snapshot().Used.Disk != o.MaxDiskBytes+1<<20 {
				t.Fatal("persistent stage refunded or RAM retained", o.Budget.Snapshot())
			}
			if err := b.Discard(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("journal stage retained", err)
			}
			if o.Budget.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal(o.Budget.Snapshot())
			}
		})
	}
}

func TestEpochFactoryCloseCancelsAndJoinsActualJobBeforeRefund(t *testing.T) {
	b, o, key, path := epochCandidate(t)
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := b.startNewEpoch(context.Background(), key, path, 8, 1<<20, func(phase string) {
			if phase == "root-normalized" {
				close(entered)
				<-release
			}
		})
		finished <- err
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case <-b.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel factory")
	}
	select {
	case err := <-closed:
		t.Fatal("Close did not join job", err)
	default:
	}
	if o.Budget.Snapshot().Classes[resourcebudget.IO].RAM != EpochBuildRAMBytes || o.Budget.Snapshot().Used.Disk != o.MaxDiskBytes+1<<20 {
		t.Fatal("in-flight work refunded", o.Budget.Snapshot())
	}
	close(release)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := b.Discard(); err != nil {
		t.Fatal(err)
	}
	if o.Budget.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal(o.Budget.Snapshot())
	}
}

func TestEpochForeignJournalPreservesReferencedRootAndBothDiskOwners(t *testing.T) {
	b, o, key, path := epochCandidate(t)
	if _, err := b.StartNewEpoch(context.Background(), key, path, 8, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.Discard(); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal("foreign native file removed", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "foreign" {
		t.Fatal("foreign bytes changed")
	}
	if _, err := os.Stat(filepath.Join(b.output, "pages")); err != nil {
		t.Fatal("referenced root deleted", err)
	}
	if o.Budget.Snapshot().Used.Disk != o.MaxDiskBytes+1<<20 {
		t.Fatal("persistent ownership prematurely refunded", o.Budget.Snapshot())
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".original", path); err != nil {
		t.Fatal(err)
	}
	if err := b.Discard(); err != nil {
		t.Fatal(err)
	}
	if o.Budget.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal(o.Budget.Snapshot())
	}
}
