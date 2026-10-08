package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

func boundNativeMetadataFixture(t *testing.T) (NativeSourceOptions, *metadatajournal.Log, metadatajournal.BoundReplicationAuthority, []byte) {
	t.Helper()
	original, _, _, _ := nativeRangeFixture(t)
	options := original.options
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x31}, 32)
	options.LedgerKey, options.AuditKey, options.GovernanceKey, options.MetadataKey = key, key, key, key
	b := metadatajournal.BaselineBinding{Version: 1, Epoch: 9, KeyGeneration: 2, Schema: 45, Rows: 17}
	for i, p := range []*[32]byte{&b.Identity, &b.Catalog, &b.Scope, &b.Cut, &b.RootDigest} {
		*p = sha256.Sum256([]byte{byte(i + 1)})
	}
	b.RootID[0] = 13
	a := metadatajournal.BoundReplicationAuthority{Baseline: b, FileRevision: 1, MaxFileBytes: 64 << 20}
	options.MetadataPath = filepath.Join(t.TempDir(), "bound-primary.journal")
	options.MetadataAuthority = &a
	l, err := metadatajournal.StartBoundEpoch(context.Background(), options.MetadataPath, key, metadatajournal.EpochHeader{Epoch: 9, Reason: "independently-qualified-test-baseline", Baseline: &b, FileRevision: 1}, a.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	})
	header, err := os.ReadFile(options.MetadataPath)
	if err != nil {
		t.Fatal(err)
	}
	return options, l, a, header
}

func TestBoundMetadataNativeSourceSinkAndActualOrderingConfirmation(t *testing.T) {
	ctx := context.Background()
	options, native, a, header := boundNativeMetadataFixture(t)
	source, err := NewNativeSource(options)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	// The caller's permission is borrowed only during construction.
	options.MetadataAuthority.Baseline.Identity[0] ^= 1
	if v, err := source.MetadataFormatVersion(); err != nil || v != 2 {
		t.Fatal(v, err)
	}
	replicaPath := filepath.Join(t.TempDir(), "bound-replica.journal")
	if err := os.WriteFile(replicaPath, header, 0600); err != nil {
		t.Fatal(err)
	}
	replica, err := metadatajournal.OpenBoundReplicaContext(ctx, replicaPath, options.MetadataKey, a)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	ledgerReplica, err := ledger.OpenWithOptions(options.LedgerPath, ledger.NewStatus(), ledger.Options{Replica: true, ChainKey: options.LedgerKey})
	if err != nil {
		t.Fatal(err)
	}
	defer ledgerReplica.Close()
	auditReplica, err := audit.OpenWithOptions(options.AuditPath, options.AuditKey, audit.Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer auditReplica.Close()
	governanceReplica, err := governance.OpenWithOptions(options.GovernancePath, options.GovernanceKey, governance.Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer governanceReplica.Close()
	sink, err := NewNativeSink(ledgerReplica, auditReplica, governanceReplica, replica)
	if err != nil {
		t.Fatal(err)
	}
	outbound := &recordingOutbound{}
	primary, primaryJournal := newTestPrimary(t, outbound)
	defer primaryJournal.Close()
	receiver, replicaJournal := newTestReceiver(t, sink)
	defer replicaJournal.Close()
	if _, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished}); err != nil {
		t.Fatal(err)
	}
	if err := native.SetBoundReplicationAdmissionAndAfterDurable(a, func(batch metadatajournal.DurableBatch) (metadatajournal.AppendAdmission, error) {
		return primary.ReserveNativeSourceUnconfirmed(len(batch.Frames))
	}, func(batch metadatajournal.DurableBatch) (uint64, error) {
		commit, err := primary.RecordMetadataBatch(batch)
		return commit.Index, err
	}, primary.WaitConfirmed); err != nil {
		t.Fatal(err)
	}
	receipt, err := native.AppendWithReceipt([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("p"), Value: []byte("value")}})
	if err != nil || receipt.ReplicationIndex != 2 {
		t.Fatal(receipt, err)
	}
	if cursor, err := source.Cursor(StoreMetadata); err != nil || cursor != (StoreCursor{Generation: 9, Sequence: 1}) {
		t.Fatal(cursor, err)
	}
	if digest, err := source.HeadAt(StoreMetadata, StoreCursor{Generation: 9, Sequence: 1}); err != nil || digest != receipt.Record.Hash {
		t.Fatal(digest, err)
	}
	var reconstructed []LocalCommit
	if err := ReconstructRangeEach("production-a", "inc_01", 1, 2, primaryJournal, source, func(c LocalCommit) error { reconstructed = append(reconstructed, c); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(reconstructed) != 2 || !bytes.Equal(reconstructed[1].Encoded, outbound.frames[1]) {
		t.Fatal("ordered digest changed during native reconstruction")
	}
	for _, commit := range reconstructed {
		ack, err := receiver.Receive(commit.Encoded)
		if err != nil {
			t.Fatal(err)
		}
		if ack.DurableIndex != commit.Index {
			t.Fatal(ack)
		}
		if commit.Index == 2 {
			if replica.Head() != native.Head() {
				t.Fatal("ACK preceded native durability", replica.Head(), native.Head())
			}
			if index, _, _ := replicaJournal.Head(); index != 2 {
				t.Fatal("ACK preceded ordering durability", index)
			}
		}
		if _, err := primary.Acknowledge(ack); err != nil {
			t.Fatal(err)
		}
	}
	if err := native.WaitConfirmed(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(reconstructed[1].Encoded); err != nil {
		t.Fatal("exact receiver retransmission", err)
	}
	frame, err := UnmarshalFrame(reconstructed[1].Encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Persist(frame); err != nil {
		t.Fatal("exact native sink retry", err)
	}
	bad := frame
	bad.Payload = bytes.Clone(frame.Payload)
	bad.Payload[len(bad.Payload)-1] ^= 1
	if err := sink.Persist(bad); err == nil {
		t.Fatal("native sink accepted conflicting authenticated-order bytes")
	}
	if _, err := replica.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("local"), Value: []byte("bad")}}); err == nil {
		t.Fatal("Replica acquired semantic write authority")
	}
}

func TestBoundMetadataNativeSourceDefaultRefusesAndQualifiedRepairRevokesReaders(t *testing.T) {
	options, native, a, _ := boundNativeMetadataFixture(t)
	for range 2 {
		if _, err := native.Append([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("p"), Value: []byte("v")}}); err != nil {
			t.Fatal(err)
		}
	}
	legacy := options
	legacy.MetadataAuthority = nil
	unqualified, err := NewNativeSource(legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer unqualified.Close()
	if _, err := unqualified.MetadataFormatVersion(); err == nil {
		t.Fatal("default source advertised format2")
	}
	if _, err := unqualified.Read(StoreMetadata, 9, 1, 1); err == nil {
		t.Fatal("default source exported format2")
	}
	source, err := NewNativeSource(options)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	reader, release := acquiredNativeReader(t, source)
	defer release()
	if _, err := reader.Read(StoreMetadata, 9, 1, 2); err != nil {
		t.Fatal(err)
	}
	revision, err := reader.sourceRevision()
	if err != nil {
		t.Fatal(err)
	}
	if err := native.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(options.MetadataPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("HM"))
	f.Close()
	before, err := os.ReadFile(options.MetadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.RepairReplicaTail(StoreMetadata, StoreCursor{Generation: 9, Sequence: 1}); err == nil {
		t.Fatal("repair accepted wrong ordering prefix")
	}
	after, _ := os.ReadFile(options.MetadataPath)
	if !bytes.Equal(before, after) {
		t.Fatal("failed repair changed source")
	}
	if err := reader.deliverAtRevision(revision, func() error { t.Fatal("old source revision delivered after repair attempt"); return nil }); !errors.Is(err, errSourceRevisionChanged) {
		t.Fatal(err)
	}
	if err := source.RepairReplicaTail(StoreMetadata, StoreCursor{Generation: 9, Sequence: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(StoreMetadata, 9, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := source.Truncate(StoreMetadata, StoreCursor{Generation: 9, Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(StoreMetadata, 9, 2, 2); err == nil {
		t.Fatal("truncated frames survived reader reset")
	}
	wrong := a
	wrong.Baseline.RootDigest[0] ^= 1
	foreign := options
	foreign.MetadataAuthority = &wrong
	refused, err := NewNativeSource(foreign)
	if err != nil {
		t.Fatal(err)
	}
	defer refused.Close()
	if _, err := refused.Cursor(StoreMetadata); err == nil {
		t.Fatal("foreign independent authority accepted")
	}
}
