package metadatastate

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/resourcebudget"
	bbolt "go.etcd.io/bbolt"
)

func linkedDeltaCandidate(t *testing.T) (*Baseline, BaselineOptions, string) {
	b, o, key, path := epochCandidate(t)
	if _, err := b.StartNewEpoch(context.Background(), key, path, 8, 16<<20); err != nil {
		t.Fatal(err)
	}
	return b, o, path
}

func deltaRows(t *testing.T, b *Baseline) []authenticatedstate.Row {
	t.Helper()
	pin, err := b.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	var rows []authenticatedstate.Row
	cursor := ""
	for {
		p, err := pin.Range(context.Background(), authenticatedstate.RangeOptions{Cursor: cursor, MaxRows: 64, MaxBytes: 8 << 20, MaxPages: 4096})
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, p.Rows...)
		if p.Done {
			return rows
		}
		cursor = p.Cursor
	}
}

func TestDeltaComplete1024OperationFramePublishesOnceAndMatchesNativeFacts(t *testing.T) {
	b, _, path := linkedDeltaCandidate(t)
	ctx := context.Background()
	before, err := b.store.RootIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old, err := b.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	ops := make([]metadatajournal.Op, 1024)
	for i := range ops {
		ops[i] = metadatajournal.Op{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte(fmt.Sprintf("bulk_%04d", i)), Value: []byte(fmt.Sprintf("v_%04d", i))}
	}
	record, err := b.AppendFrame(ctx, ops)
	if err != nil || record.Sequence != 1 {
		t.Fatal(record, err)
	}
	after, err := b.store.RootIdentity(ctx)
	if err != nil || after.Rows != before.Rows+1024 || after.Revision != before.Revision+1 || after.Cut != deltaCut(b.position, record) {
		t.Fatal(after, before, err)
	}
	if _, found, err := old.Get(ctx, mustDeltaKey(t, []string{"projects"}, []byte("bulk_0000"), false)); err != nil || found {
		t.Fatal("old pin imported new frame", found, err)
	}
	rows := deltaRows(t, b)
	values := map[string][]byte{}
	for _, r := range rows {
		values[string(r.Key)] = r.Value
	}
	for _, op := range ops {
		if !bytes.Equal(values[string(mustDeltaKey(t, op.Path, op.Key, false))], op.Value) {
			t.Fatal("missing whole-frame value", op.Key)
		}
	}
	for name, value := range map[string]uint64{"metadata_journal_epoch": 8, "applied_journal_sequence": 1} {
		raw := values[string(mustDeltaKey(t, []string{"meta"}, []byte(name), false))]
		if len(raw) != 8 || binary.BigEndian.Uint64(raw) != value {
			t.Fatal(name, raw)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := b.journal.ReplayRangeValidatedContext(ctx, 0, 1, func(actual metadatajournal.Record) error {
		count++
		if actual.Hash != record.Hash || !reflect.DeepEqual(actual.Ops, ops) {
			return ErrSource
		}
		return nil
	}); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestDelta1024DeepCreateOpsPreserveExistingSequencesWithinOneFrame(t *testing.T) {
	b, _, _ := linkedDeltaCandidate(t)
	path := []string{"deployment_price_timeline", "p\x00/"}
	for len(path) < 32 {
		path = append(path, fmt.Sprintf("level_%02d", len(path)))
	}
	ops := make([]metadatajournal.Op, 1024)
	for i := range ops {
		ops[i] = metadatajournal.Op{Kind: metadatajournal.OpCreateBucket, Path: append([]string(nil), path...)}
	}
	before, err := b.store.RootIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if record, err := b.AppendFrame(context.Background(), ops); err != nil || record.Sequence != 1 {
		t.Fatal(record.Sequence, err)
	}
	after, err := b.store.RootIdentity(context.Background())
	if err != nil || after.Rows != before.Rows+30 || after.Revision != before.Revision+1 {
		t.Fatal(before, after, err)
	}
	pin, err := b.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	for i := 1; i < len(path); i++ {
		value, found, err := pin.Get(context.Background(), mustDeltaKey(t, path[:i+1], nil, true))
		expected := uint64(0)
		if i == 1 {
			expected = 17
		}
		if err != nil || !found || len(value) != 8 || binary.BigEndian.Uint64(value) != expected {
			t.Fatal(i, value, found, err)
		}
	}
}

func TestDeltaReplayRejectsClassifiedButForbiddenExcludedTailBeforeAnyRootUpdate(t *testing.T) {
	for _, op := range []metadatajournal.Op{
		{Kind: metadatajournal.OpPut, Path: []string{"usage_daily_rollup"}, Key: []byte("local")},
		{Kind: metadatajournal.OpPut, Path: []string{"meta"}, Key: []byte("schema_version"), Value: []byte("foreign-schema")},
	} {
		t.Run(fmt.Sprint(op.Path, op.Key), func(t *testing.T) {
			b, _, _ := linkedDeltaCandidate(t)
			if _, err := b.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("allowed-prefix"), Value: []byte("value")}}); err != nil {
				t.Fatal(err)
			}
			if _, err := b.journal.Append([]metadatajournal.Op{op}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(b.output + "/active.root")
			if err != nil {
				t.Fatal(err)
			}
			if head, err := b.ReplayTo(context.Background(), b.journal.Head(), 1); err == nil || head != (metadatajournal.Head{}) {
				t.Fatal(head, err)
			}
			after, err := os.ReadFile(b.output + "/active.root")
			if err != nil || !bytes.Equal(before, after) || b.ready.Load() {
				t.Fatal("invalid excluded domain facts accepted", err)
			}
		})
	}
}

func mustDeltaKey(t *testing.T, path []string, key []byte, bucket bool) []byte {
	t.Helper()
	p := make([][]byte, len(path))
	for i, v := range path {
		p[i] = []byte(v)
	}
	k, err := RowKey(p, key, bucket)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestDeltaFourOperationSemanticsMatchActualBboltIncludingDeleteRecreate(t *testing.T) {
	b, _, _ := linkedDeltaCandidate(t)
	ctx := context.Background()
	db, err := bbolt.Open(t.TempDir()+"/reference.db", 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Build the independent reference by replaying the actual authenticated
	// backup's logical fixture, not by using the root reducer being tested.
	if err := db.Update(func(tx *bbolt.Tx) error {
		root, err := tx.CreateBucket([]byte("deployment_price_timeline"))
		if err != nil {
			return err
		}
		child, err := root.CreateBucket([]byte{'p', 0, '/'})
		if err != nil {
			return err
		}
		if err := child.SetSequence(17); err != nil {
			return err
		}
		for i := 0; i < 130; i++ {
			if err := child.Put([]byte(fmt.Sprintf("key-%03d", i)), []byte(fmt.Sprintf("price-%03d", i))); err != nil {
				return err
			}
		}
		if err := child.Put([]byte{0, 'k'}, []byte{}); err != nil {
			return err
		}
		_, err = root.CreateBucket([]byte("empty"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	frames := [][]metadatajournal.Op{
		{{Kind: metadatajournal.OpCreateBucket, Path: []string{"deployment_price_timeline", "p\x00/"}}, {Kind: metadatajournal.OpPut, Path: []string{"deployment_price_timeline", "p\x00/"}, Key: []byte("new"), Value: []byte{}}, {Kind: metadatajournal.OpDelete, Path: []string{"deployment_price_timeline", "absent"}, Key: []byte("missing")}},
		{{Kind: metadatajournal.OpDeleteBucket, Path: []string{"deployment_price_timeline", "p\x00/"}}, {Kind: metadatajournal.OpCreateBucket, Path: []string{"deployment_price_timeline", "p\x00/", "deep", "empty"}}, {Kind: metadatajournal.OpPut, Path: []string{"deployment_price_timeline", "p\x00/", "deep"}, Key: []byte{0, 'k'}, Value: []byte("recreated")}, {Kind: metadatajournal.OpDelete, Path: []string{"deployment_price_timeline", "p\x00/", "deep"}, Key: []byte("not-there")}},
		{{Kind: metadatajournal.OpDeleteBucket, Path: []string{"deployment_price_timeline", "p\x00/", "deep"}}, {Kind: metadatajournal.OpDeleteBucket, Path: []string{"deployment_price_timeline", "p\x00/", "deep"}}, {Kind: metadatajournal.OpCreateBucket, Path: []string{"deployment_price_timeline", "p\x00/", "deep"}}},
	}
	for index, ops := range frames {
		if _, err := b.AppendFrame(ctx, ops); err != nil {
			t.Fatal(index, err)
		}
		if err := db.Update(func(tx *bbolt.Tx) error { return applyReferenceOps(tx, ops) }); err != nil {
			t.Fatal(index, err)
		}
		var want []authenticatedstate.Row
		if err := db.View(func(tx *bbolt.Tx) error {
			it := newBaselineIterator(ctx, tx)
			for {
				r, found, err := it.next()
				if err != nil {
					return err
				}
				if !found {
					return nil
				}
				want = append(want, authenticatedstate.Row{Key: bytes.Clone(r.Key), Value: bytes.Clone(r.Value)})
			}
		}); err != nil {
			t.Fatal(err)
		}
		lo, hi, _ := BucketRange([][]byte{[]byte("deployment_price_timeline")})
		var got []authenticatedstate.Row
		for _, r := range deltaRows(t, b) {
			if bytes.Compare(r.Key, lo) >= 0 && bytes.Compare(r.Key, hi) < 0 {
				got = append(got, r)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("bbolt/root semantics diverged", index, got, want)
		}
	}
}

func applyReferenceOps(tx *bbolt.Tx, ops []metadatajournal.Op) error {
	for _, op := range ops {
		var current *bbolt.Bucket
		for i, name := range op.Path {
			var err error
			if i == 0 {
				if op.Kind == metadatajournal.OpCreateBucket {
					current, err = tx.CreateBucketIfNotExists([]byte(name))
				} else {
					current = tx.Bucket([]byte(name))
				}
			} else {
				if current == nil {
					break
				}
				if op.Kind == metadatajournal.OpCreateBucket {
					current, err = current.CreateBucketIfNotExists([]byte(name))
				} else {
					current = current.Bucket([]byte(name))
				}
			}
			if err != nil {
				return err
			}
			if current == nil {
				break
			}
		}
		switch op.Kind {
		case metadatajournal.OpCreateBucket:
		case metadatajournal.OpPut:
			if current == nil {
				return ErrSource
			}
			if err := current.Put(op.Key, op.Value); err != nil {
				return err
			}
		case metadatajournal.OpDelete:
			if current != nil {
				if err := current.Delete(op.Key); err != nil {
					return err
				}
			}
		case metadatajournal.OpDeleteBucket:
			if current == nil {
				continue
			}
			if len(op.Path) == 1 {
				if err := tx.DeleteBucket([]byte(op.Path[0])); err != nil {
					return err
				}
			} else {
				p := tx.Bucket([]byte(op.Path[0]))
				for _, n := range op.Path[1 : len(op.Path)-1] {
					p = p.Bucket([]byte(n))
				}
				if err := p.DeleteBucket([]byte(op.Path[len(op.Path)-1])); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func TestDeltaRejectsLateSemanticErrorAndAppendCapacityBeforeNative(t *testing.T) {
	for _, mode := range []string{"missing-put-bucket", "value-as-bucket", "bucket-as-value", "append-limit"} {
		t.Run(mode, func(t *testing.T) {
			b, _, path := linkedDeltaCandidate(t)
			if mode == "append-limit" {
				h, _, _ := baselineArchive(t, true, nil, nil)
				o := baselineOptions(t)
				o.MaxAppendBytes = 64 << 10
				var err error
				key := bytes.Repeat([]byte{51}, 32)
				b, err = BuildFromBackup(context.Background(), h, key, privateSource(t), t.TempDir()+"/root", o)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := b.Discard(); err != nil {
						t.Error(err)
					}
				})
				path = privateSource(t) + ".journal"
				if _, err := b.StartNewEpoch(context.Background(), key, path, 8, 16<<20); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(path)
			root, _ := os.ReadFile(b.output + "/active.root")
			ops := []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("first"), Value: []byte("not-published")}}
			switch mode {
			case "missing-put-bucket":
				ops = append(ops, metadatajournal.Op{Kind: metadatajournal.OpPut, Path: []string{"deployment_price_timeline", "missing"}, Key: []byte("k")})
			case "value-as-bucket":
				ops = append(ops, metadatajournal.Op{Kind: metadatajournal.OpCreateBucket, Path: []string{"projects", "fixture"}})
			case "bucket-as-value":
				ops = append(ops, metadatajournal.Op{Kind: metadatajournal.OpPut, Path: []string{"deployment_price_timeline"}, Key: []byte("empty")})
			case "append-limit":
				// A larger bounded frame exhausts configured append bytes before WAL.
				ops[0].Value = bytes.Repeat([]byte{'x'}, 128<<10)
			}
			if record, err := b.AppendFrame(context.Background(), ops); err == nil || record.Hash != ([32]byte{}) {
				t.Fatal("bad frame accepted", record, err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) || b.journal.Head().Sequence != 0 {
				t.Fatal("native advanced before full preparation")
			}
			if b.ready.Load() {
				t.Fatal("attempted candidate remained ready")
			}
			// No partial domain root was published.
			published, _ := os.ReadFile(b.output + "/active.root")
			if !bytes.Equal(root, published) {
				t.Fatal("partial domain root published")
			}
		})
	}
}

func TestDeltaPreflightClassificationAndLimitsLeaveCandidateAndFeesUntouched(t *testing.T) {
	for _, op := range []metadatajournal.Op{
		{Kind: metadatajournal.OpPut, Path: []string{"usage_daily_rollup"}, Key: []byte("k")},
		{Kind: metadatajournal.OpPut, Path: []string{"meta"}, Key: []byte("applied_journal_sequence")},
		{Kind: metadatajournal.OpPut, Path: []string{"unknown"}, Key: []byte("k")},
		{Kind: metadatajournal.OpDeleteBucket, Path: []string{"meta"}},
		{Kind: metadatajournal.OpPut, Path: []string{"meta"}, Key: []byte("schema_version"), Value: []byte("bad")},
		{Kind: metadatajournal.OpCreateBucket, Path: []string{"projects", string([]byte{255})}},
	} {
		t.Run(fmt.Sprint(op.Kind, op.Path, op.Key), func(t *testing.T) {
			b, o, path := linkedDeltaCandidate(t)
			before, _ := os.ReadFile(path)
			fees := o.Budget.Snapshot().Used
			root, _ := b.store.RootIdentity(context.Background())
			if _, err := b.AppendFrame(context.Background(), []metadatajournal.Op{op}); err == nil {
				t.Fatal("invalid op accepted")
			}
			after, _ := os.ReadFile(path)
			now, err := b.store.RootIdentity(context.Background())
			if err != nil || now != root || !bytes.Equal(before, after) || !b.ready.Load() || o.Budget.Snapshot().Used != fees {
				t.Fatal("preflight changed owner", err)
			}
		})
	}
}

func TestDeltaReplayAuthenticatesExcludedTailAndAppliesOnlyExplicitPrefix(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			b, _, path := linkedDeltaCandidate(t)
			ctx := context.Background()
			for i := 1; i <= 3; i++ {
				if _, err := b.journal.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte(fmt.Sprintf("replay_%d", i)), Value: []byte("value")}}); err != nil {
					t.Fatal(err)
				}
			}
			head := b.journal.Head()
			before, _ := os.ReadFile(b.output + "/active.root")
			if corrupt {
				f, err := os.OpenFile(path, os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteAt([]byte{0xff}, head.Offset-1); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			applied, err := b.ReplayTo(ctx, head, 1)
			if corrupt {
				if err == nil || applied != (metadatajournal.Head{}) || b.ready.Load() {
					t.Fatal(applied, err)
				}
				after, _ := os.ReadFile(b.output + "/active.root")
				if !bytes.Equal(before, after) {
					t.Fatal("excluded corrupt tail applied prefix")
				}
				return
			}
			if err != nil || applied.Sequence != 1 || applied.Offset >= head.Offset {
				t.Fatal(applied, err)
			}
			pin, err := b.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 3; i++ {
				_, found, err := pin.Get(ctx, mustDeltaKey(t, []string{"projects"}, []byte(fmt.Sprintf("replay_%d", i)), false))
				if err != nil || found != (i == 1) {
					t.Fatal(i, found, err)
				}
			}
			pin.Close()
			if final, err := b.ReplayTo(ctx, head, 3); err != nil || final != head {
				t.Fatal(final, head, err)
			}
		})
	}
}

func TestDeltaCloseCancelsAndJoinsActualPreparationAndDurablePhase(t *testing.T) {
	for _, stage := range []string{"candidate-prepared", "native-durable"} {
		t.Run(stage, func(t *testing.T) {
			b, o, _ := linkedDeltaCandidate(t)
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				_, err := b.appendFrame(context.Background(), []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("close"), Value: []byte("value")}}, func(at string) {
					if at == stage {
						close(entered)
						<-release
					}
				})
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("phase not entered")
			}
			closed := make(chan error, 1)
			go func() { closed <- b.Close() }()
			select {
			case err := <-closed:
				t.Fatal("close did not join", err)
			case <-time.After(30 * time.Millisecond):
			}
			fees := o.Budget.Snapshot()
			if fees.Classes[resourcebudget.IO].RAM < DeltaRAMBytes || fees.Used.Disk != o.MaxDiskBytes+16<<20 {
				t.Fatal("early fee retirement", fees)
			}
			close(release)
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if err := <-closed; err != nil {
				t.Fatal(err)
			}
			if b.journal.Head().Sequence != map[string]uint64{"candidate-prepared": 0, "native-durable": 1}[stage] {
				t.Fatal("wrong actual durable position", b.journal.Head())
			}
			if _, err := b.AppliedHead(); !errors.Is(err, authenticatedstate.ErrClosed) {
				t.Fatal(err)
			}
			if err := b.Discard(); err != nil {
				t.Fatal(err)
			}
			if o.Budget.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal("cleanup did not retire fees", o.Budget.Snapshot())
			}
		})
	}
}
