package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func frozenOrderingBudget(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 1 << 20}, MaxClaims: 16, MaxOwners: 8, MaxPressureAge: time.Hour}
	limits.Classes[resourcebudget.IO] = resourcebudget.Cost{RAM: 512 << 10}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	b, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return b
}
func frozenOrderingFixture(t *testing.T) (*OrderingJournal, *resourcebudget.Manager) {
	t.Helper()
	j, err := OpenOrderingJournal(filepath.Join(t.TempDir(), "ordering.log"), bytes.Repeat([]byte{42}, 32), OrderingHeader{ClusterID: "frozen", Incarnation: "inc_01"}, 0, [sha256.Size]byte{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil && !errors.Is(err, ownedfile.ErrRetirement) {
			t.Error(err)
		}
	})
	if _, err := j.Append(testLeadershipRecord(1, 1, "leadership")); err != nil {
		t.Fatal(err)
	}
	record := testOrderingRecord(2, 1, "actual-fact")
	record.StoreSequenceFirst, record.StoreSequenceLast = 1, 1
	if _, err := j.Append(record); err != nil {
		t.Fatal(err)
	}
	return j, frozenOrderingBudget(t)
}
func TestOrderingFrozenHeadAuthenticatesFourCursorsAndExcludesAppend(t *testing.T) {
	j, b := frozenOrderingFixture(t)
	index, term, mac := j.Head()
	rejected := errors.New("candidate rejected")
	err := j.WithFrozenHead(context.Background(), b, func(ctx context.Context, h FrozenOrderingHead) error {
		if h.Index != index || h.Term != term || h.MAC != mac || h.StoreCursors[0] != (StoreCursor{Generation: 1, Sequence: 1}) {
			t.Fatal("incorrect frozen authority", h)
		}
		if _, err := j.Append(testOrderingRecord(3, 1, "refused")); !errors.Is(err, ErrOrderingFrozen) {
			t.Fatal("append admitted", err)
		}
		if _, err := j.Record(2); err != nil {
			t.Fatal("frozen record unreadable", err)
		}
		return rejected
	})
	if !errors.Is(err, rejected) || b.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("candidate rejection changed fees", err, b.Snapshot())
	}
	record := testOrderingRecord(3, 1, "after")
	record.StoreSequenceFirst, record.StoreSequenceLast = 2, 2
	if _, err := j.Append(record); err != nil {
		t.Fatal("rejection poisoned healthy journal", err)
	}
}
func TestOrderingFrozenHeadCloseCancelsAndJoinsWholeCallback(t *testing.T) {
	j, b := frozenOrderingFixture(t)
	entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	done := make(chan error, 1)
	go func() {
		done <- j.WithFrozenHead(context.Background(), b, func(ctx context.Context, _ FrozenOrderingHead) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("job did not enter")
	}
	before := b.Snapshot().Used
	closed := make(chan error, 1)
	go func() { closed <- j.Close() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel")
	}
	select {
	case err := <-closed:
		t.Fatal("Close skipped join", err)
	case <-time.After(25 * time.Millisecond):
	}
	if b.Snapshot().Used != before {
		t.Fatal("live callback refunded")
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("closed job accepted", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if b.Snapshot().Used != (resourcebudget.Cost{}) || j.key != ([32]byte{}) {
		t.Fatal("known exit retained resources", b.Snapshot())
	}
}
func TestOrderingFrozenHeadActualOldBytesAndHeaderCannotHideBehindMtime(t *testing.T) {
	for _, shape := range []string{"header", "old-record", "missing-tail", "replaced-name"} {
		t.Run(shape, func(t *testing.T) {
			j, b := frozenOrderingFixture(t)
			path := j.file.Name()
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			err = j.WithFrozenHead(context.Background(), b, func(context.Context, FrozenOrderingHead) error {
				raw, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				switch shape {
				case "header":
					raw[16] ^= 1
				case "old-record":
					raw[j.dataOffset+32] ^= 1
				case "missing-tail":
					raw = raw[:len(raw)-orderingRecordBytes]
				case "replaced-name":
					if err := os.Rename(path, path+".original"); err != nil {
						return err
					}
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					return err
				}
				return os.Chtimes(path, info.ModTime(), info.ModTime())
			})
			if !errors.Is(err, ErrOrderingSource) {
				t.Fatal("changed actual source accepted", shape, err)
			}
			if _, err := j.Append(testOrderingRecord(3, 1, "refused")); err == nil {
				t.Fatal("corrupt source writable")
			}
			if b.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal("known rejection leaked", b.Snapshot())
			}
		})
	}
}
func TestOrderingActualCloseDebtPreservesDescriptorAndKey(t *testing.T) {
	j, _ := frozenOrderingFixture(t)
	actual := j.file
	key := j.key
	if err := actual.Close(); err != nil {
		t.Fatal(err)
	}
	first := j.Close()
	if !errors.Is(first, ownedfile.ErrRetirement) || j.file != actual || j.key != key {
		t.Fatal("actual close erased original debt", first, j.file, j.key)
	}
	if err := j.Close(); err != first || j.file != actual || j.key != key {
		t.Fatal("repeat close erased debt", err)
	}
}
func TestOrderingFrozenHeadAdmissionRejectsBeforeCallback(t *testing.T) {
	j, b := frozenOrderingFixture(t)
	held, err := b.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: (512 << 10) - 1024})
	if err != nil {
		t.Fatal(err)
	}
	before := b.Snapshot().Used
	called := false
	err = j.WithFrozenHead(context.Background(), b, func(context.Context, FrozenOrderingHead) error { called = true; return nil })
	if !errors.Is(err, resourcebudget.ErrExhausted) || called || b.Snapshot().Used != before {
		t.Fatal("unfunded native job entered", called, err, b.Snapshot())
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	record := testOrderingRecord(3, 1, "after")
	record.StoreSequenceFirst, record.StoreSequenceLast = 2, 2
	if _, err := j.Append(record); err != nil {
		t.Fatal("pre-IO refusal poisoned journal", err)
	}
}
