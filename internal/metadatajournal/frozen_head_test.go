package metadatajournal

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func metadataFrozenBudget(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 64 << 20}, MaxClaims: 16, MaxOwners: 8, MaxPressureAge: time.Hour}
	limits.Classes[resourcebudget.IO] = resourcebudget.Cost{RAM: 48 << 20}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMetadataFrozenHeadBothFormatsExcludeWritesTrimAndHooks(t *testing.T) {
	for _, format := range []string{"legacy", "bound"} {
		t.Run(format, func(t *testing.T) {
			var l *Log
			if format == "legacy" {
				_, l = newJournal(t)
			} else {
				_, l, _ = newBoundJournal(t)
			}
			if _, err := l.Append([]Op{put("projects", "before", "v")}); err != nil {
				t.Fatal(err)
			}
			budget := metadataFrozenBudget(t)
			before := l.Head()
			rejection := errors.New("candidate rejected")
			err := l.WithFrozenHead(context.Background(), budget, func(ctx context.Context, head Head) error {
				if head != before {
					t.Fatal("wrong complete cut", head, before)
				}
				if _, err := l.Append([]Op{put("projects", "refused", "v")}); !errors.Is(err, ErrNativeFrozen) {
					t.Fatal("semantic write admitted", err)
				}
				if err := l.AppendReplicated(head.Epoch, 2, 2, []byte("not decoded")); !errors.Is(err, ErrNativeFrozen) {
					t.Fatal("physical write admitted", err)
				}
				prepared := false
				if _, err := l.AppendPreparedContext(ctx, []Op{put("projects", "prepared", "v")}, func(Record, func() error) error { prepared = true; return nil }); !errors.Is(err, ErrNativeFrozen) || prepared {
					t.Fatal("prepared write admitted", prepared, err)
				}
				if _, err := l.TrimBoundContext(ctx, head, TrimStateBinding{}); !errors.Is(err, ErrNativeFrozen) {
					t.Fatal("trim admitted", err)
				}
				if err := l.SetAfterDurable(func(DurableBatch) (uint64, error) { return 0, nil }, func(context.Context, uint64) error { return nil }); !errors.Is(err, ErrNativeFrozen) {
					t.Fatal("hook changed", err)
				}
				if err := l.ReplayRangeValidatedContext(ctx, 0, head.Sequence, func(Record) error { return nil }); err != nil {
					t.Fatal("frozen source unreadable", err)
				}
				return rejection
			})
			if !errors.Is(err, rejection) || l.Head() != before {
				t.Fatal("candidate rejection changed native", err, l.Head())
			}
			if budget.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal("job leaked fees", budget.Snapshot())
			}
			if _, err := l.Append([]Op{put("projects", "after", "v")}); err != nil {
				t.Fatal("rejection poisoned native", err)
			}
		})
	}
}

func TestMetadataFrozenHeadCloseCancelsAndJoinsActualCallback(t *testing.T) {
	_, l, _ := newBoundJournal(t)
	budget := metadataFrozenBudget(t)
	entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	result := make(chan error, 1)
	go func() {
		result <- l.WithFrozenHead(context.Background(), budget, func(ctx context.Context, _ Head) error {
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
		t.Fatal("callback not entered")
	}
	before := budget.Snapshot().Used
	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel")
	}
	select {
	case err := <-closed:
		t.Fatal("Close skipped actual join", err)
	case <-time.After(25 * time.Millisecond):
	}
	if got := budget.Snapshot().Used; got != before {
		t.Fatal("callback refunded early", got, before)
	}
	if _, err := l.Append([]Op{put("projects", "closed", "v")}); err == nil {
		t.Fatal("closing native writable")
	}
	close(release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal("closed job returned success", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if l.file != nil || len(l.key) != 32 || budget.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("known close failed lifetime", l.file, budget.Snapshot())
	}
	for _, b := range l.key {
		if b != 0 {
			t.Fatal("known close retained key")
		}
	}
}

func TestMetadataFrozenHeadActualOldByteTamperWithdrawsWriter(t *testing.T) {
	for _, format := range []string{"legacy", "bound"} {
		t.Run(format, func(t *testing.T) {
			var path string
			var l *Log
			if format == "legacy" {
				path, l = newJournal(t)
			} else {
				path, l, _ = newBoundJournal(t)
			}
			budget := metadataFrozenBudget(t)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			err = l.WithFrozenHead(context.Background(), budget, func(context.Context, Head) error {
				raw, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				raw[frameHeaderSize+10] ^= 1
				if err := os.WriteFile(path, raw, 0600); err != nil {
					return err
				}
				return os.Chtimes(path, info.ModTime(), info.ModTime())
			})
			if !errors.Is(err, ErrCorrupt) {
				t.Fatal("old byte change accepted", err)
			}
			if _, err := l.Append([]Op{put("projects", "refused", "v")}); err == nil {
				t.Fatal("corrupt writer reopened")
			}
			if budget.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal("known rejection leaked fees", budget.Snapshot())
			}
		})
	}
}

func TestMetadataFrozenHeadAdmissionRejectsBeforeSourceOrCallback(t *testing.T) {
	_, l, _ := newBoundJournal(t)
	budget := metadataFrozenBudget(t)
	held, err := budget.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: 24 << 20})
	if err != nil {
		t.Fatal(err)
	}
	before := budget.Snapshot().Used
	called := false
	err = l.WithFrozenHead(context.Background(), budget, func(context.Context, Head) error { called = true; return nil })
	if !errors.Is(err, resourcebudget.ErrExhausted) || called {
		t.Fatal("unfunded job entered", called, err)
	}
	if got := budget.Snapshot().Used; got != before {
		t.Fatal("unfunded job leaked", got, before)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append([]Op{put("projects", "accepted", "v")}); err != nil {
		t.Fatal("pre-IO rejection poisoned native", err)
	}
}
