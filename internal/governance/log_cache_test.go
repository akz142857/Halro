package governance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/governancelimits"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func governanceCacheBudget(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 8 << 20}, RecoveryReserve: resourcebudget.Cost{RAM: 3 << 20}, MaxClaims: 4, RecoveryClaims: 2, MaxOwners: 2, MaxPressureAge: time.Minute}
	l.Classes[resourcebudget.Control].RAM = 4 * resourcebudget.ClaimRAMBytes
	l.Classes[resourcebudget.Recovery].RAM = (3 << 20) - 2*resourcebudget.ClaimRAMBytes
	b, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGovernanceNativeCacheCanonicalTimeIdentitySurvivesReopen(t *testing.T) {
	_, log, _, _, _ := testManager(t)
	defer log.Close()
	event := projectionEvent(1)
	event.ObservedAt = event.ObservedAt.In(time.FixedZone("first-local-name", 3600))
	first, err := log.Append(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	key := append([]byte(nil), log.key...)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(log.path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	// Location names do not exist in durable JSON. Identical durable content
	// must not change identity because decoding reconstructed the Location.
	event.ObservedAt = event.ObservedAt.In(time.FixedZone("other-local-name", 3600))
	head := next.Summary()
	got, err := next.Append(context.Background(), event)
	if err != nil || got.Sequence != first.Sequence || got.Offset != first.Offset || got.Hash != first.Hash || next.Summary() != head {
		t.Fatal("durable time representation changed retry identity", got.Sequence, err)
	}
	event.ObservedAt = event.ObservedAt.Add(time.Nanosecond)
	if _, err := next.Append(context.Background(), event); err == nil || next.Summary() != head {
		t.Fatal("changed durable timestamp escaped conflict detection", err)
	}
}

func TestGovernanceNativeRetryCachePreservesWindowReopenAndWideIdentity(t *testing.T) {
	_, log, _, _, _ := testManager(t)
	defer log.Close()
	ctx := context.Background()
	var first, last Event
	var lastRecord Record
	for start := 0; start <= maxTrackedEventIDs; start += 64 {
		batch := make([]Event, 0, 64)
		for i := start; i < start+64 && i <= maxTrackedEventIDs; i++ {
			e := projectionEvent(i)
			// IDs have no public byte ceiling beyond the frame. Retaining even
			// just the original ID would pin history-sized string allocations.
			e.EventID = fmt.Sprintf("wide_%08d_", i) + strings.Repeat("x", 4096)
			e.ProjectID = strings.Repeat("p", 1024)
			if i == 0 {
				first = e
			}
			last = e
			batch = append(batch, e)
		}
		records, err := log.AppendBatch(ctx, batch)
		if err != nil {
			t.Fatal(err)
		}
		lastRecord = records[len(records)-1]
	}
	if len(log.recordsByEventID) != maxTrackedEventIDs || log.trackedCount != maxTrackedEventIDs || len(log.trackedOrder) != maxTrackedEventIDs {
		t.Fatal("retry window grew with wide history")
	}
	head := log.Summary()
	if got, err := log.Append(ctx, last); err != nil || !reflect.DeepEqual(got, lastRecord) || log.Summary() != head {
		t.Fatal("tail duplicate changed durable facts", got, err)
	}
	conflict := last
	conflict.ProjectID = "different"
	if _, err := log.Append(ctx, conflict); err == nil || log.Summary() != head {
		t.Fatal("tail fingerprint credited conflicting payload", err)
	}
	// Preserve the established finite native retry window. The Manager's
	// durable idempotency is independent and is not shortened by this cache.
	if got, err := log.Append(ctx, first); err != nil || got.Sequence != head.Records+1 {
		t.Fatal("native window eviction changed", got, err)
	}
	key := append([]byte(nil), log.key...)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if log.recordsByEventID != nil || log.trackedOrder != nil || log.key != nil {
		t.Fatal("Close retained cache backing storage")
	}
	next, err := Open(log.path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	head = next.Summary()
	if got, err := next.Append(ctx, last); err != nil || !reflect.DeepEqual(got, lastRecord) || next.Summary() != head || len(next.recordsByEventID) != maxTrackedEventIDs {
		t.Fatal("authenticated reopen lost cached native position", got, err)
	}
}

func TestGovernanceNativeCacheBudgetRefusesBeforeIOAndRefundsFailedOpen(t *testing.T) {
	b := governanceCacheBudget(t)
	path := filepath.Join(t.TempDir(), "must-not-exist", "governance.wal")
	key := make([]byte, 32)
	options := Options{CacheBudget: b, CacheRAMBytes: governancelimits.MinCacheRAMBytes}
	held, err := b.TryAcquireRecovery(resourcebudget.Cost{RAM: governancelimits.MinCacheRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	if log, err := OpenWithOptions(path, key, options); log != nil || !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatal("cache acquired unowned memory", log, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused cache admission entered native IO", err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Options{{CacheRAMBytes: governancelimits.MinCacheRAMBytes}, {CacheBudget: b, CacheRAMBytes: governancelimits.MinCacheRAMBytes - 1}} {
		if log, err := OpenWithOptions(path, key, bad); log != nil || err == nil {
			t.Fatal("invalid cache ownership accepted", log, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("bad", frameHeaderSize+frameMACSize)), 0o600); err != nil {
		t.Fatal(err)
	}
	if log, err := OpenWithOptions(path, key, options); log != nil || err == nil {
		t.Fatal("corrupt native journal accepted", log, err)
	}
	if stats := b.Snapshot(); stats.Used != (resourcebudget.Cost{}) || stats.RecoveryClaims != 0 {
		t.Fatal("failed construction retained cache claim", stats)
	}
}

func TestGovernanceNativeCacheLifetimeJoinsReplayBeforeRefund(t *testing.T) {
	b := governanceCacheBudget(t)
	log, err := OpenWithOptions(filepath.Join(t.TempDir(), "governance.wal"), make([]byte, 32), Options{CacheBudget: b, CacheRAMBytes: governancelimits.MinCacheRAMBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, err := log.Append(context.Background(), projectionEvent(1)); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	replayDone := make(chan error, 1)
	go func() {
		_, err := log.Replay(func(Record) error { close(entered); <-release; return nil })
		replayDone <- err
	}()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- log.Close() }()
	select {
	case err := <-closeDone:
		close(release)
		t.Fatal("native Close passed a live reader", err)
	case <-time.After(20 * time.Millisecond):
	}
	if stats := b.Snapshot(); stats.RecoveryClaims != 1 || stats.Classes[resourcebudget.Recovery].RAM != governancelimits.MinCacheRAMBytes {
		close(release)
		t.Fatal("blocked native owner refunded cache", stats)
	}
	close(release)
	if err := <-replayDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if stats := b.Snapshot(); stats.Used != (resourcebudget.Cost{}) || stats.RecoveryClaims != 0 {
		t.Fatal("joined native owner did not refund cache", stats)
	}
}
