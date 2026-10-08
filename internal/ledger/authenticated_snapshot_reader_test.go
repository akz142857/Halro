package ledger

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

// This fixture tests complete Ledger-domain representation in the authenticated
// reader. The test freezes a freshly rebuilt projection, NOT an arbitrary saved
// DB. It is not a production publisher, identity authority or resume-token API.
func TestAuthenticatedSnapshotReaderCoversActualLedgerNativeProjection(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	native := filepath.Join(root, "ledger.wal")
	log := openChained(t, native, nil)
	for _, e := range projectionTestAttribution() {
		if _, err := log.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 20; i++ {
		for _, e := range projectionTestLifecycle(i, true) {
			if _, err := log.Append(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		if i == 10 {
			if _, err := log.Roll(); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Keep an accepted request and actual unresolved attempt, so the complete key
	// set covers both recovery levels and their association/time indexes.
	for _, e := range projectionTestLifecycle(21, true)[:3] {
		if _, err := log.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := log.Compact(1); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	report, partial, err := InspectReplayAuthenticated(native, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(partial, err)
	}
	projection, err := BuildLedgerProjection(ctx, native, testChainKey, filepath.Join(root, "fresh.db"), report)
	if err != nil {
		t.Fatal(err)
	}
	defer projection.Discard()
	anchor, err := projection.ReadReplayAnchor(ctx)
	if err != nil || anchor.Head != report.Head || anchor.Hash != report.ChainHash {
		t.Fatal(anchor, err)
	}
	// Freeze all writes/Close; individual source transactions finish before the
	// snapshot writes. No source row collection is built even in this fixture.
	projection.mu.Lock()
	defer projection.mu.Unlock()
	buckets := append(append([]string{}, projectionBuckets...), projectionListBuckets...)
	contextBytes := []byte("test:complete-ledger-projection-scope:standalone:v1\x00")
	contextBytes = binary.BigEndian.AppendUint64(contextBytes, anchor.Head.Generation)
	contextBytes = binary.BigEndian.AppendUint64(contextBytes, uint64(anchor.Head.Offset))
	contextBytes = binary.BigEndian.AppendUint64(contextBytes, anchor.Head.Sequence)
	contextBytes = append(contextBytes, anchor.Hash[:]...)
	contextBytes = append(contextBytes, anchor.Epoch)
	for _, name := range buckets {
		contextBytes = binary.BigEndian.AppendUint16(contextBytes, uint16(len(name)))
		contextBytes = append(contextBytes, []byte(name)...)
	}
	key, err := hkdf.Key(sha256.New, testChainKey, []byte("test-only:ledger-snapshot-key"), "test-only:ledger-snapshot-hmac", 32)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 256 << 20, Disk: 256 << 20}, MaxClaims: 16, MaxOwners: 4, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	budget, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err = budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	options := authenticatedstate.Options{Context: sha256.Sum256(contextBytes), Key: key, MaxDiskBytes: 32 << 20, Budget: budget}
	bucketIndex := 0
	var after []byte
	sourceRows := 0
	next := func() (authenticatedstate.Row, bool, error) {
		for bucketIndex < len(buckets) {
			var row authenticatedstate.Row
			err := projection.db.View(func(tx *bbolt.Tx) error {
				bucket := tx.Bucket([]byte(buckets[bucketIndex]))
				if bucket == nil {
					return ErrProjectionCorrupt
				}
				cursor := bucket.Cursor()
				k, v := cursor.First()
				if after != nil {
					k, v = cursor.Seek(after)
					if bytes.Equal(k, after) {
						k, v = cursor.Next()
					}
				}
				if k == nil {
					return nil
				}
				if v == nil {
					return ErrProjectionCorrupt
				}
				row.Key = append([]byte{byte(bucketIndex + 1)}, k...)
				row.Value = bytes.Clone(v)
				after = bytes.Clone(k)
				return nil
			})
			if err != nil {
				return authenticatedstate.Row{}, false, err
			}
			if row.Key != nil {
				sourceRows++
				return row, true, nil
			}
			bucketIndex++
			after = nil
		}
		return authenticatedstate.Row{}, false, nil
	}
	file := filepath.Join(root, "snapshot.pack")
	snapshot, err := authenticatedstate.Build(ctx, file, options, next)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Discard()
	if err = snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := authenticatedstate.Open(ctx, file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	seen := 0
	cursor := ""
	for {
		page, err := reader.Range(ctx, authenticatedstate.RangeOptions{Cursor: cursor, MaxRows: 17, MaxBytes: 1 << 20, MaxPages: 512})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Rows {
			if len(row.Key) < 2 || int(row.Key[0]) > len(buckets) {
				t.Fatal("unknown bucket")
			}
			err := projection.db.View(func(tx *bbolt.Tx) error {
				value := tx.Bucket([]byte(buckets[int(row.Key[0])-1])).Get(row.Key[1:])
				if value == nil || !bytes.Equal(value, row.Value) {
					return fmt.Errorf("snapshot differs for %s/%x", buckets[int(row.Key[0])-1], row.Key[1:])
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			seen++
		}
		if page.Done {
			break
		}
		cursor = page.Cursor
	}
	if seen != sourceRows || seen < 400 {
		t.Fatal("complete set not preserved", seen, sourceRows)
	}
	ordinal := func(name string) byte {
		for i, b := range buckets {
			if b == name {
				return byte(i + 1)
			}
		}
		t.Fatal("bucket missing", name)
		return 0
	}
	balanceKey := BalanceKey{ProjectID: "prj_1", PeriodID: "2026-10-01", TimezoneVersion: 1}
	lookup := append([]byte{ordinal("balances")}, projectionBalanceKey(balanceKey)...)
	if value, found, err := reader.Get(ctx, lookup); err != nil || !found || len(value) == 0 {
		t.Fatal("actual balance missing", found, err)
	}
	for _, index := range []struct {
		bucket string
		key    []byte
	}{{"event_positions", projectionIDKey(projectionTestLifecycle(21, true)[1].EventID)}, {"pending", projectionIDKey("att_000021")}, {"attempts_by_request", projectionTuple("req_000021", "att_000021")}} {
		key := append([]byte{ordinal(index.bucket)}, index.key...)
		if _, found, err := reader.Get(ctx, key); err != nil || !found {
			t.Fatal("recovery/idempotency index absent", index.bucket, found, err)
		}
	}
	if _, found, err := reader.Get(ctx, append([]byte{ordinal("event_positions")}, projectionIDKey("invented-event")...)); err != nil || found {
		t.Fatal("absence proof failed", found, err)
	}
	other := options
	other.Context[0] ^= 1
	if owner, err := authenticatedstate.Open(ctx, file, other); err == nil || owner != nil {
		t.Fatal("other native cut accepted")
	}
	t.Logf("actual native sequence=%d; complete snapshot rows=%d; buckets=%d", anchor.Head.Sequence, seen, len(buckets))
}
