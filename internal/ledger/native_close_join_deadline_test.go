package ledger

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestNativeCloseDeadlineWithdrawsFullQueueBeforeLifecycleJoin(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{
		ChainKey: testChainKey, QueueCapacity: 1, MaxBatch: 1,
		AfterDurable: func(DurableBatch) (uint64, error) {
			close(entered)
			<-release
			return 1, nil
		},
		WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 3)
	appendOne := func(id string) {
		_, err := l.Append(context.Background(), validReservation(id, id))
		results <- err
	}
	go appendOne("actual-durable")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("original durable writer did not enter")
	}
	go appendOne("actual-queued")
	deadline := time.Now().Add(time.Second)
	for len(l.appendQueue) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("original queue did not fill")
		}
		time.Sleep(time.Millisecond)
	}
	go appendOne("actual-blocked-enqueue")
	for {
		// The first two enqueues already released their lifecycle readers.
		// The third genuine enqueue owns the sole remaining blocked reader.
		if !l.lifecycle.TryLock() {
			break
		}
		l.lifecycle.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("full queue did not block the original enqueue")
		}
		time.Sleep(time.Millisecond)
	}
	original, key := l.file, bytes.Clone(l.chainKey)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- l.CloseContext(ctx) }()
	select {
	case err := <-closed:
		var debt *nativeCloseJoinRetirement
		if !errors.As(err, &debt) || debt.log != l || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("full-queue deadline lost original join debt", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close waited for lifecycle before withdrawing the full queue")
	}
	select {
	case err := <-results:
		if err == nil {
			t.Fatal("withdrawn enqueue reported a durable result")
		}
	case <-time.After(time.Second):
		t.Fatal("original blocked enqueue was not withdrawn")
	}
	if l.file != original || !bytes.Equal(l.chainKey, key) {
		t.Fatal("still-live original writer was retired")
	}
	once.Do(func() { close(release) })
	for range 2 {
		select {
		case <-results:
		case <-time.After(time.Second):
			t.Fatal("actual accepted request failed to finish")
		}
	}
	if err := l.Close(); !errors.Is(err, ownedfile.ErrRetirement) || l.file != nil || !bytes.Equal(l.chainKey, key) {
		t.Fatal("actual writer exit cleared prior unknown retirement", err)
	}
}

func TestNativeCloseDeadlineRetainsOriginalLifecycleReader(t *testing.T) {
	l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	original, key := l.file, bytes.Clone(l.chainKey)
	l.lifecycle.RLock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err = l.CloseContext(ctx)
	l.lifecycle.RUnlock()
	var debt *nativeCloseJoinRetirement
	if !errors.As(err, &debt) || debt.log != l || !errors.Is(err, context.DeadlineExceeded) || l.file != original || !bytes.Equal(l.chainKey, key) {
		t.Fatal("lifecycle reader was falsely joined", err)
	}
	if _, err := l.Append(context.Background(), validReservation("after-withdraw", "after-withdraw")); err == nil {
		t.Fatal("withdrawn lifecycle accepted a new mutation")
	}
	if err := l.Close(); !errors.Is(err, ownedfile.ErrRetirement) || l.file != nil || !bytes.Equal(l.chainKey, key) {
		t.Fatal("ordinary retry cleared original lifecycle debt", err)
	}
}

func TestNativeCloseDeadlineRetainsOriginalDurableWriterUntilActualExit(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{
		ChainKey: testChainKey,
		AfterDurable: func(DurableBatch) (uint64, error) {
			close(entered)
			<-release // A genuinely durable callback ignores cancellation.
			return 1, nil
		},
		WaitConfirmed: func(context.Context, uint64) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	original, key := l.file, bytes.Clone(l.chainKey)
	admission := &retainedNativeAdmission{}
	l.uncertainAdmissions = []AppendAdmission{admission}
	appended := make(chan error, 1)
	go func() {
		_, e := l.Append(context.Background(), validReservation("durable-close", "durable-close"))
		appended <- e
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("actual post-fsync writer not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- l.CloseContext(ctx) }()
	var closeErr error
	select {
	case closeErr = <-closed:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("shutdown deadline blocked behind the actual durable writer")
	}
	var debt *nativeCloseJoinRetirement
	if !errors.As(closeErr, &debt) || debt.log != l || !errors.Is(closeErr, context.DeadlineExceeded) || !errors.Is(closeErr, ownedfile.ErrRetirement) {
		close(release)
		t.Fatal("unknown join lost its original owner", closeErr)
	}
	if l.file != original || !bytes.Equal(l.chainKey, key) || admission.releases != 0 || len(l.uncertainAdmissions) != 1 {
		close(release)
		t.Fatal("deadline refunded a still-live writer/descriptor")
	}
	if _, err := original.Stat(); err != nil {
		close(release)
		t.Fatal("actual descriptor exited early", err)
	}
	if _, err := l.Append(context.Background(), validReservation("later", "later")); err == nil {
		close(release)
		t.Fatal("unknown shutdown reopened new native admission")
	}
	close(release)
	<-appended
	if err := l.Close(); !errors.Is(err, ownedfile.ErrRetirement) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("ordinary retry cleared the original unknown join", err)
	}
	if l.file != nil || !bytes.Equal(l.chainKey, key) || admission.releases != 0 || len(l.uncertainAdmissions) != 1 {
		t.Fatal("actual descriptor exit was not joined, or unresolved obligations were refunded")
	}
}

func TestNativeCloseDeadlineRetainsOriginalAcceptedMutation(t *testing.T) {
	l, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	finish, err := l.beginMutation(nil)
	if err != nil {
		t.Fatal(err)
	}
	original, key := l.file, bytes.Clone(l.chainKey)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err = l.CloseContext(ctx)
	var debt *nativeCloseJoinRetirement
	if !errors.As(err, &debt) || debt.log != l || !errors.Is(err, context.DeadlineExceeded) || l.file != original || !bytes.Equal(l.chainKey, key) {
		finish()
		t.Fatal("accepted mutation was falsely joined", err)
	}
	finish()
	if err := l.Close(); !errors.Is(err, ownedfile.ErrRetirement) || l.file != nil || !bytes.Equal(l.chainKey, key) {
		t.Fatal("ordinary retry cleared mutation join debt", err)
	}
}

func TestNativeCloseDeadlineRetainsActualStartupReaderAndItsPaidWork(t *testing.T) {
	f := newSelectedStartupFixture(t, true)
	f.reopen(t)
	entered, release := make(chan *os.File, 1), make(chan struct{})
	a, err := f.native.startSelectedStartupAudit(context.Background(), f.o.DirectoryOptions.Budget,
		StartupAuditOptions{Deadline: time.Second, ProgressTimeout: time.Second}, func(r Record, file *os.File) {
			if r.Sequence == 1 {
				entered <- file
				<-release
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	reader := <-entered
	paid := f.o.DirectoryOptions.Budget.Snapshot().Used
	key := bytes.Clone(f.native.chainKey)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err = f.native.CloseContext(ctx)
	var debt *nativeCloseJoinRetirement
	if !errors.As(err, &debt) || debt.log != f.native || !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatal("actual reader exit was falsely joined", err)
	}
	// This native join stays uncertain; the distinct historical reader's IO
	// work may retire only after its own actual FD has subsequently exited.
	f.expectNativeJoinDebt = true
	if got := f.o.DirectoryOptions.Budget.Snapshot().Used; got != paid || !bytes.Equal(f.native.chainKey, key) {
		close(release)
		t.Fatal("live historical reader work or key was retired", got, paid)
	}
	if _, err := reader.Stat(); err != nil {
		close(release)
		t.Fatal("actual historical FD closed before its callback", err)
	}
	close(release)
	_ = a.Wait(context.Background())
	if err := f.native.Close(); !errors.Is(err, ownedfile.ErrRetirement) || f.native.file != nil || !bytes.Equal(f.native.chainKey, key) {
		t.Fatal("ordinary close cleared original historical join debt", err)
	}
}
