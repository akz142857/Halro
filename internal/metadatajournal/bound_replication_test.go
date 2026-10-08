package metadatajournal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replicationrange"
)

func boundReplicationFixture(t *testing.T) (string, *Log, BoundReplicationAuthority) {
	t.Helper()
	path, l, b := newBoundJournal(t)
	return path, l, BoundReplicationAuthority{Baseline: b, FileRevision: 1, MaxFileBytes: 64 << 20}
}

func TestBoundReplicationReplicaRetryActualCloseDebtRemainsOwned(t *testing.T) {
	ctx := context.Background()
	path, primary, authority := boundReplicationFixture(t)
	header, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.Append([]Op{put("projects", "p", "v")}); err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	reader, err := NewBoundReplicationRangeReader(path, testKey(), 1<<20, authority)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := reader.Read(authority.Baseline.Epoch, 1, 1)
	if closeErr := reader.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	replicaPath := filepath.Join(t.TempDir(), "replica.journal")
	if err := os.WriteFile(replicaPath, header, 0600); err != nil {
		t.Fatal(err)
	}
	replica, err := OpenBoundReplicaContext(ctx, replicaPath, testKey(), authority)
	if err != nil {
		t.Fatal(err)
	}
	writer := replica.file
	defer writer.Close() // Test owns disposal after asserting permanent uncertainty.
	if err := replica.AppendReplicated(authority.Baseline.Epoch, 1, 1, frames); err != nil {
		t.Fatal(err)
	}
	var actual *os.File
	allocations := 0
	factory := func(path string, key []byte, maxBytes int64, a BoundReplicationAuthority) (*ReplicationRangeReader, error) {
		allocations++
		r, err := NewBoundReplicationRangeReader(path, key, maxBytes, a)
		if err != nil {
			return nil, err
		}
		if err := r.reader.Close(); err != nil {
			return nil, err
		}
		r.reader, err = replicationrange.New(path, key, maxBytes, func(input io.ReadSeeker, key []byte, initial replicationrange.Position, first, last uint64) (replicationrange.Range, error) {
			span, err := r.scan(input, key, initial, first, last)
			if section, ok := input.(*io.SectionReader); ok {
				outer, _, _ := section.Outer()
				actual = outer.(*os.File)
				if closeErr := actual.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
			}
			return span, err
		})
		return r, err
	}
	err = replica.appendReplicatedContext(ctx, authority.Baseline.Epoch, 1, 1, frames, factory)
	if !errors.Is(err, ownedfile.ErrRetirement) || actual == nil || replica.boundReaderDebt == nil || len(replica.boundReaderDebt.key) != KeySize {
		t.Fatal("retry lost actual descriptor/key debt", err)
	}
	for range 3 {
		if err := replica.appendReplicatedContext(ctx, authority.Baseline.Epoch, 1, 1, frames, factory); !errors.Is(err, ownedfile.ErrRetirement) {
			t.Fatal("uncertain reader admitted another retry", err)
		}
		// Borrowed Replica has no deletion authority, so Discard rejects even
		// before inspecting retirement; it must never erase the borrowed file.
		if !errors.Is(replica.Close(), ownedfile.ErrRetirement) || replica.Discard() == nil || !errors.Is(replica.HealthError(), ownedfile.ErrRetirement) || replica.file != writer || len(replica.key) != KeySize || allocations != 1 {
			t.Fatal("cleanup erased debt or repeated reader allocation", allocations)
		}
		if _, err := os.Stat(replicaPath); err != nil {
			t.Fatal("unknown reader close removed borrowed replica", err)
		}
	}
}

func TestBoundReplicationForeignMACValidReplacementCannotReusePrefix(t *testing.T) {
	ctx := context.Background()
	path, l, a := boundReplicationFixture(t)
	if _, err := l.Append([]Op{put("projects", "p", "v")}); err != nil {
		t.Fatal(err)
	}
	reader, err := NewBoundReplicationRangeReader(path, testKey(), 16<<20, a)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.Read(9, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	foreign := a.Baseline
	foreign.RootDigest[0] ^= 1
	replacement := path + ".foreign"
	other, err := StartBoundEpoch(ctx, replacement, testKey(), EpochHeader{Epoch: 9, Reason: "different-qualified-test-root", Baseline: &foreign, FileRevision: 1}, a.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Append([]Op{put("projects", "p", "v")}); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := BoundReplicationCursor(ctx, path, testKey(), a); err == nil {
		t.Fatal("foreign valid-MAC source accepted")
	}
	if frames, err := reader.Read(9, 1, 1); err == nil || frames != nil {
		t.Fatal("cached prefix crossed foreign inode", err)
	}
}

func TestBoundReplicationCancelledAdmittedReadJoinsCloseBeforeKeyRetirement(t *testing.T) {
	path, l, a := boundReplicationFixture(t)
	if _, err := l.Append([]Op{put("projects", "p", "v")}); err != nil {
		t.Fatal(err)
	}
	r, err := NewBoundReplicationRangeReader(path, testKey(), 16<<20, a)
	if err != nil {
		t.Fatal(err)
	}
	// Replace only the native scanner in this fixture. The real range reader
	// still owns the actual open FD and its Close/returned-copy authentication.
	if err := r.reader.Close(); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	r.reader, err = replicationrange.New(path, testKey(), 16<<20, func(input io.ReadSeeker, key []byte, p replicationrange.Position, first, last uint64) (replicationrange.Range, error) {
		close(entered)
		<-release
		return r.scan(input, key, p, first, last)
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	readDone := make(chan error, 1)
	go func() {
		frames, err := r.ReadContext(ctx, 9, 1, 1)
		if frames != nil {
			err = errors.Join(err, errors.New("cancelled read returned frames"))
		}
		readDone <- err
	}()
	<-entered
	cancel()
	closeDone := make(chan error, 1)
	go func() { closeDone <- r.Close() }()
	select {
	case err := <-closeDone:
		t.Fatal("Close bypassed admitted native scanner", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := <-readDone; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if r.ctx != nil {
		t.Fatal("reader retained callback context")
	}
	if frames, err := r.Read(9, 1, 1); frames != nil || !errors.Is(err, replicationrange.ErrClosed) {
		t.Fatal(frames, err)
	}
}

type boundPartialReplicaWrite struct{ file *os.File }

func (w *boundPartialReplicaWrite) Write(raw []byte) (int, error) {
	n, err := w.file.Write(raw[:len(raw)/2])
	return n, errors.Join(io.ErrUnexpectedEOF, err)
}
func (w *boundPartialReplicaWrite) Sync() error { return w.file.Sync() }

func TestBoundReplicationInterruptedReplicaWriteRequiresActualCloseAndOrderedRepair(t *testing.T) {
	ctx := context.Background()
	path, l, a := boundReplicationFixture(t)
	r, err := l.Append([]Op{put("projects", "p", "v")})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewBoundReplicationRangeReader(path, testKey(), 16<<20, a)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	frames, err := reader.Read(9, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "replica.journal")
	if err := os.WriteFile(copyPath, data[:r.Offset], 0600); err != nil {
		t.Fatal(err)
	}
	replica, err := OpenBoundReplicaContext(ctx, copyPath, testKey(), a)
	if err != nil {
		t.Fatal(err)
	}
	before := replica.Head()
	replica.durability = &boundPartialReplicaWrite{replica.file}
	if err := replica.AppendReplicated(9, 1, 1, frames); err == nil || replica.Head() != before || replica.HealthError() == nil {
		t.Fatal("partial native write falsely committed", replica.Head(), err)
	}
	if err := replica.AppendReplicated(9, 1, 1, frames); err == nil {
		t.Fatal("uncertain owner accepted retry")
	}
	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBoundReplicaContext(ctx, copyPath, testKey(), a); err == nil {
		t.Fatal("Open repaired without ordering permission")
	}
	if err := RepairBoundReplicaTail(ctx, copyPath, testKey(), a, 9, 0); err != nil {
		t.Fatal(err)
	}
	repaired, err := OpenBoundReplicaContext(ctx, copyPath, testKey(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer repaired.Close()
	if err := repaired.AppendReplicated(9, 1, 1, frames); err != nil {
		t.Fatal(err)
	}
	if repaired.Head() != l.Head() {
		t.Fatal(repaired.Head(), l.Head())
	}
}

func TestBoundReplicationIndependentAuthorityAndPhysicalFrames(t *testing.T) {
	ctx := context.Background()
	path, l, a := boundReplicationFixture(t)
	var records []Record
	for i := 0; i < 3; i++ {
		r, err := l.Append([]Op{put("projects", "p", "v")})
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	if version, err := l.ReplicationVersion(); err == nil || version != 0 {
		t.Fatal("private format2 advertised without replication qualification", version, err)
	}
	for _, name := range []string{"identity", "epoch", "key-generation", "schema", "catalog", "scope", "cut", "root-id", "digest", "rows", "revision", "ceiling"} {
		t.Run(name, func(t *testing.T) {
			wrong := a
			switch name {
			case "identity":
				wrong.Baseline.Identity[0] ^= 1
			case "epoch":
				wrong.Baseline.Epoch++
			case "key-generation":
				wrong.Baseline.KeyGeneration++
			case "schema":
				wrong.Baseline.Schema++
			case "catalog":
				wrong.Baseline.Catalog[0] ^= 1
			case "scope":
				wrong.Baseline.Scope[0] ^= 1
			case "cut":
				wrong.Baseline.Cut[0] ^= 1
			case "root-id":
				wrong.Baseline.RootID[0] ^= 1
			case "digest":
				wrong.Baseline.RootDigest[0] ^= 1
			case "rows":
				wrong.Baseline.Rows++
			case "revision":
				wrong.FileRevision++
			case "ceiling":
				wrong.MaxFileBytes = l.Head().Offset - 1
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := BoundReplicationCursor(ctx, path, testKey(), wrong); err == nil {
				t.Fatal("cursor accepted candidate-derived permission")
			}
			reader, err := NewBoundReplicationRangeReader(path, testKey(), 16<<20, wrong)
			if err == nil {
				if raw, err := reader.Read(9, 1, 3); err == nil || raw != nil {
					t.Fatal("foreign frames escaped", err)
				}
				reader.Close()
			}
			if opened, err := OpenBoundReplicaContext(ctx, path, testKey(), wrong); err == nil || opened != nil {
				t.Fatal("foreign replica accepted", opened, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("permission refusal changed source", err)
			}
		})
	}
	gen, seq, err := BoundReplicationCursor(ctx, path, testKey(), a)
	if err != nil || gen != 9 || seq != 3 {
		t.Fatal(gen, seq, err)
	}
	for i, r := range records {
		digest, err := BoundReplicationHeadAt(ctx, path, testKey(), a, 9, uint64(i+1))
		if err != nil || digest != r.Hash {
			t.Fatal(i, digest, err)
		}
	}
	reader, err := NewBoundReplicationRangeReader(path, testKey(), 16<<20, a)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	frames, err := reader.Read(9, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replicaPath := filepath.Join(t.TempDir(), "replica.journal")
	if err := os.WriteFile(replicaPath, data[:records[0].Offset], 0600); err != nil {
		t.Fatal(err)
	}
	replica, err := OpenBoundReplicaContext(ctx, replicaPath, testKey(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	before := replica.Head()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := replica.AppendReplicatedContext(cancelled, 9, 1, 3, frames); !errors.Is(err, context.Canceled) || replica.Head() != before {
		t.Fatal("cancelled append changed head", err)
	}
	bad := bytes.Clone(frames)
	bad[len(bad)-1] ^= 1
	if err := replica.AppendReplicated(9, 1, 3, bad); err == nil || replica.Head() != before {
		t.Fatal("tampered batch changed head", err)
	}
	if err := replica.AppendReplicated(9, 1, 3, frames); err != nil {
		t.Fatal(err)
	}
	if err := replica.AppendReplicated(9, 1, 3, frames); err != nil {
		t.Fatal("exact retry", err)
	}
	if replica.Head() != l.Head() {
		t.Fatal(replica.Head(), l.Head())
	}
	if _, err := replica.Append([]Op{put("projects", "local", "denied")}); err == nil {
		t.Fatal("replica semantic append accepted")
	}
	if v, err := replica.ReplicationVersion(); err != nil || v != 2 {
		t.Fatal(v, err)
	}
	if raw, err := reader.ReadContext(cancelled, 9, 1, 3); !errors.Is(err, context.Canceled) || raw != nil {
		t.Fatal("cancelled reader returned bytes", err)
	}
}

func TestBoundReplicationRepairRequiresIndependentOrderingPrefix(t *testing.T) {
	ctx := context.Background()
	path, l, a := boundReplicationFixture(t)
	r, err := l.Append([]Op{put("projects", "p", "v")})
	if err != nil {
		t.Fatal(err)
	}
	complete := l.Head()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("HM")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		a        BoundReplicationAuthority
		sequence uint64
		cancel   bool
	}{
		{"ordering", a, 0, false}, {"cancel", a, 1, true}, {"binding", func() BoundReplicationAuthority { v := a; v.Baseline.RootDigest[0] ^= 1; return v }(), 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ctx
			if tc.cancel {
				var stop context.CancelFunc
				c, stop = context.WithCancel(ctx)
				stop()
			}
			if err := RepairBoundReplicaTail(c, path, testKey(), tc.a, 9, tc.sequence); err == nil {
				t.Fatal("unqualified repair accepted")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("failed repair removed bytes")
			}
		})
	}
	if _, err := OpenBoundReplicaContext(ctx, path, testKey(), a); err == nil {
		t.Fatal("replica open repaired before ordering qualification")
	}
	if err := RepairBoundReplicaTail(ctx, path, testKey(), a, 9, 1); err != nil {
		t.Fatal(err)
	}
	actual, err := Verify(path, testKey())
	if err != nil || actual != complete {
		t.Fatal(actual, complete, err)
	}
	if err := TruncateBoundReplicationTail(ctx, path, testKey(), a, 9, 0); err != nil {
		t.Fatal(err)
	}
	if _, seq, err := BoundReplicationCursor(ctx, path, testKey(), a); err != nil || seq != 0 {
		t.Fatal(seq, err)
	}
	if _, err := BoundReplicationHeadAt(ctx, path, testKey(), a, 9, 1); err == nil {
		t.Fatal("truncated sequence accepted", r)
	}
}

func TestBoundReplicationQualifiedPrimaryUsesAdmissionOrderingAndConfirmation(t *testing.T) {
	ctx := context.Background()
	path, l, a := boundReplicationFixture(t)
	admission := &metadataAppendAdmissionFixture{}
	wrong := a
	wrong.FileRevision++
	if err := l.SetBoundReplicationAdmissionAndAfterDurable(wrong, func(DurableBatch) (AppendAdmission, error) { return admission, nil }, func(DurableBatch) (uint64, error) { return 7, nil }, func(context.Context, uint64) error { return nil }); err == nil {
		t.Fatal("wrong qualification installed")
	}
	var order []string
	if err := l.SetBoundReplicationAdmissionAndAfterDurable(a, func(DurableBatch) (AppendAdmission, error) {
		order = append(order, "admit")
		if _, seq, err := BoundReplicationCursor(ctx, path, testKey(), a); err != nil || seq != 0 {
			t.Fatal(seq, err)
		}
		return admission, nil
	}, func(b DurableBatch) (uint64, error) {
		order = append(order, "ordered")
		if b.Admission != admission {
			t.Fatal("admission transfer")
		}
		if _, seq, err := BoundReplicationCursor(ctx, path, testKey(), a); err != nil || seq != 1 {
			t.Fatal("ordering before native barrier", seq, err)
		}
		return 7, admission.Release()
	}, func(_ context.Context, index uint64) error {
		order = append(order, "confirmed")
		if index != 7 {
			t.Fatal(index)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if v, err := l.ReplicationVersion(); err != nil || v != 2 {
		t.Fatal(v, err)
	}
	receipt, err := l.AppendWithReceipt([]Op{put("projects", "p", "v")})
	if err != nil || receipt.ReplicationIndex != 7 {
		t.Fatal(receipt, err)
	}
	if err := l.WaitConfirmed(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if admission.releases != 1 || len(order) != 3 || order[0] != "admit" || order[1] != "ordered" || order[2] != "confirmed" {
		t.Fatal(order, admission)
	}
	if err := l.SetBoundReplicationAdmissionAndAfterDurable(a, func(DurableBatch) (AppendAdmission, error) { return nil, nil }, func(DurableBatch) (uint64, error) { return 8, nil }, func(context.Context, uint64) error { return nil }); err == nil {
		t.Fatal("second permission replaced live tenure")
	}
}
