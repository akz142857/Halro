package authenticatedstate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func auditLimits() AuditOptions {
	return AuditOptions{MaxDuration: 20 * time.Second, MaxNoProgress: 10 * time.Second}
}

func TestAuditCompletePhysicalContentBeyondQueryPageLimit(t *testing.T) {
	for _, count := range []int{0, 1, 5000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, o := testStore(t, count)
			before := o.Budget.Snapshot().Used
			expected, err := s.RootIdentity(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			extent := s.current.content.extent
			var previous AuditProgress
			limits := auditLimits()
			limits.Progress = func(p AuditProgress) error {
				if p.Rows < previous.Rows || p.Nodes < previous.Nodes || p.ReadBytes < previous.ReadBytes {
					return errors.New("progress went backwards")
				}
				previous = p
				return nil
			}
			receipt, err := s.Audit(context.Background(), limits)
			if err != nil || receipt.Root != expected || receipt.Verified != previous {
				t.Fatal(receipt, err)
			}
			if receipt.Verified.Rows != uint64(count) || receipt.Verified.ValueBytes != uint64(count*12) || receipt.Verified.ReadBytes != extent-headerBytes {
				t.Fatal(receipt.Verified, extent)
			}
			if count > 0 && receipt.Verified.Nodes < uint64(count) {
				t.Fatal("missing leaf reads")
			}
			if after := o.Budget.Snapshot().Used; after != before {
				t.Fatal("leaked audit fees", before, after)
			}
		})
	}
}

func TestAuditPinsOldRootWhilePublishingSuccessor(t *testing.T) {
	s, _ := testStore(t, 65)
	expected, err := s.RootIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	updated := false
	limits := auditLimits()
	limits.Progress = func(p AuditProgress) error {
		if updated {
			return nil
		}
		updated = true
		_, err := s.Update(context.Background(), nextCut(1), func(tx *Transaction) error { return tx.Put([]byte("key_000000"), []byte("replacement")) })
		return err
	}
	receipt, err := s.Audit(context.Background(), limits)
	if err != nil || !updated || receipt.Root != expected || receipt.Verified.Rows != 65 || receipt.Verified.ValueBytes != 65*12 {
		t.Fatal(receipt, err)
	}
	current, err := s.RootIdentity(context.Background())
	if err != nil || current == expected || current.Revision != expected.Revision+1 {
		t.Fatal(current, err)
	}
}

func TestAuditFindsUntouchedCorruptSubtreeAndPoisonsEveryPin(t *testing.T) {
	s, _ := testStore(t, 65)
	old, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	q := query{ctx: context.Background(), snapshot: old, maxReads: maxHeight}
	d := descriptor{count: old.current.count, height: old.current.height, ref: old.current.root}
	var target ref
	for {
		n, err := q.load(d)
		if err != nil {
			t.Fatal(err)
		}
		if n.height == 1 {
			target = n.value
			break
		}
		d = n.children[len(n.children)-1]
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "pages"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err = f.ReadAt(b[:], int64(target.offset+1)); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 1
	_, err = f.WriteAt(b[:], int64(target.offset+1))
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	// A COW update to another branch does not read the corrupted value.
	if _, err = s.Update(context.Background(), nextCut(1), func(tx *Transaction) error { return tx.Put([]byte("key_000000"), []byte("new")) }); err != nil {
		t.Fatal(err)
	}
	current, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	receipt, err := s.Audit(context.Background(), auditLimits())
	if !errors.Is(err, ErrCorrupt) || receipt != (AuditReceipt{}) {
		t.Fatal(receipt, err)
	}
	for _, pin := range []*Snapshot{old, current} {
		if _, _, err := pin.Get(context.Background(), []byte("key_000000")); !errors.Is(err, ErrRecovery) {
			t.Fatal("pin did not fail closed", err)
		}
	}
	if _, err := s.RootIdentity(context.Background()); !errors.Is(err, ErrRecovery) {
		t.Fatal(err)
	}
	if _, err := s.Update(context.Background(), nextCut(2), func(tx *Transaction) error { return nil }); !errors.Is(err, ErrRecovery) {
		t.Fatal(err)
	}
}

func TestAuditWatchdogFailsOtherPinsBeforeBlockedJobRetires(t *testing.T) {
	for _, mode := range []string{"stalled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			s, o := testStore(t, 2)
			other, err := s.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			before := o.Budget.Snapshot().Used
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			limits := AuditOptions{MaxDuration: 3 * time.Second, MaxNoProgress: 150 * time.Millisecond}
			wanted := ErrAuditStalled
			if mode == "deadline" {
				limits.MaxDuration = 150 * time.Millisecond
				limits.MaxNoProgress = limits.MaxDuration
				wanted = ErrAuditDeadline
			}
			limits.Progress = func(AuditProgress) error { close(entered); <-release; return nil }
			result := make(chan error, 1)
			go func() {
				receipt, err := s.Audit(context.Background(), limits)
				if receipt != (AuditReceipt{}) {
					err = errors.Join(err, errors.New("failed audit returned receipt"))
				}
				result <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("audit did not enter")
			}
			if _, err := s.Audit(context.Background(), auditLimits()); !errors.Is(err, ErrBusy) {
				t.Fatal("duplicate audit admitted", err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, _, err = other.Get(context.Background(), []byte("key_000000"))
				if errors.Is(err, wanted) {
					break
				}
				if err != nil || time.Now().After(deadline) {
					t.Fatal("watchdog did not fail other pin", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if used := o.Budget.Snapshot().Used; used.RAM < before.RAM+ReadRAMBytes+4096 || used.Disk != before.Disk {
				t.Fatal("live job fees retired", before, used)
			}
			closed := make(chan error, 1)
			go func() { closed <- s.Close() }()
			select {
			case err := <-closed:
				t.Fatal("Close did not join actual callback", err)
			case <-time.After(30 * time.Millisecond):
			}
			close(release)
			select {
			case err := <-result:
				if !errors.Is(err, wanted) {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("audit did not retire")
			}
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Close did not retire")
			}
			if used := o.Budget.Snapshot().Used; used.RAM != resourcebudget.ClaimRAMBytes+4096 || used.Disk != before.Disk {
				t.Fatal("close fee mismatch", used)
			}
			if err := s.Discard(); err != nil {
				t.Fatal(err)
			}
			if used := o.Budget.Snapshot().Used; used != (resourcebudget.Cost{}) {
				t.Fatal("physical retirement retained fees", used)
			}
		})
	}
}

func TestAuditCloseCancelsAndJoinsWithoutIntegrityPoison(t *testing.T) {
	s, o := testStore(t, 2)
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	limits := auditLimits()
	limits.Progress = func(AuditProgress) error { close(entered); <-release; return nil }
	result := make(chan error, 1)
	go func() {
		receipt, err := s.Audit(context.Background(), limits)
		if receipt != (AuditReceipt{}) {
			err = errors.Join(err, errors.New("closed audit returned receipt"))
		}
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("audit did not enter")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		t.Fatal("Close skipped callback join", err)
	case <-time.After(30 * time.Millisecond):
	}
	if o.Budget.Snapshot().Used.RAM < ReadRAMBytes {
		t.Fatal("live workspace fee released")
	}
	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("audit stuck")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close stuck")
	}
	if s.fault() != nil {
		t.Fatal("caller close poisoned integrity", s.fault())
	}
}

func TestAuditPreflightCancellationCallbackFailureAndPanicRetire(t *testing.T) {
	s, o := testStore(t, 2)
	before := o.Budget.Snapshot().Used
	for _, limits := range []AuditOptions{{}, {MaxDuration: time.Second}, {MaxDuration: time.Second, MaxNoProgress: 2 * time.Second}} {
		if receipt, err := s.Audit(context.Background(), limits); !errors.Is(err, ErrLimit) || receipt != (AuditReceipt{}) {
			t.Fatal(receipt, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Audit(ctx, auditLimits()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	sentinel := errors.New("consumer stopped")
	limits := auditLimits()
	limits.Progress = func(AuditProgress) error { return sentinel }
	if receipt, err := s.Audit(context.Background(), limits); !errors.Is(err, sentinel) || receipt != (AuditReceipt{}) {
		t.Fatal(receipt, err)
	}
	func() {
		defer func() {
			if recover() != sentinel {
				t.Error("missing callback panic")
			}
		}()
		limits.Progress = func(AuditProgress) error { panic(sentinel) }
		s.Audit(context.Background(), limits)
	}()
	if after := o.Budget.Snapshot().Used; after != before {
		t.Fatal("failure leaked fees", before, after)
	}
	if _, err := s.Audit(context.Background(), auditLimits()); err != nil {
		t.Fatal("retry failed", err)
	}
}

func TestAuditImmutableSnapshotReadsCompleteRoot(t *testing.T) {
	o := testOptions(t)
	snapshot, _ := testBuild(t, o, 65)
	result, err := snapshot.Audit(context.Background(), auditLimits())
	if err != nil || result.Rows != 65 || result.ValueBytes != 65*12 || result.ReadBytes != snapshot.current.extent-headerBytes {
		t.Fatal(result, err)
	}
}
