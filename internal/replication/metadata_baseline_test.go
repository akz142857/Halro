package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/governance"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

func metadataBoundaryFixture(t *testing.T) (*NativeSource, *OrderingJournal, *MetadataBaselineAuthority, NativeSourceOptions, *metadatajournal.Log, []byte) {
	t.Helper()
	old, journal, _, _ := nativeRangeFixture(t)
	options := old.options
	n, term, mac := journal.Head()
	cursors, err := journal.StoreCursors()
	if err != nil {
		t.Fatal(err)
	}
	binding := metadatajournal.BaselineBinding{Version: 1, Epoch: 2, KeyGeneration: 1, Schema: 45, Rows: 2}
	for i, p := range []*[32]byte{&binding.Identity, &binding.Catalog, &binding.Scope, &binding.Cut, &binding.RootDigest} {
		*p = sha256.Sum256([]byte{byte(i + 1)})
	}
	binding.RootID[0] = 1
	permission := metadatajournal.BoundReplicationAuthority{Baseline: binding, FileRevision: 1, MaxFileBytes: 64 << 20}
	newPath := filepath.Join(t.TempDir(), "metadata-successor.journal")
	successor, err := metadatajournal.StartBoundEpoch(context.Background(), newPath, options.MetadataKey, metadatajournal.EpochHeader{Epoch: 2, Reason: "actual-ordering-boundary-test", Baseline: &binding, FileRevision: 1}, permission.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := successor.Close(); err != nil {
			t.Error(err)
		}
	})
	claims := MetadataBaselineClaims{Version: 1, ClusterID: "production-a", Incarnation: "inc_01", Primary: "halro-0", Members: []string{"halro-0", "halro-1", "halro-2"}, Term: term, Index: n, OrderingHead: mac, Cursors: cursors, New: permission, HeaderHash: successor.Head().Hash, BackupID: "backup", ManifestDigest: "manifest", CiphertextDigest: sha256.Sum256([]byte("ciphertext")), PreparationDigest: sha256.Sum256([]byte("receipt"))}
	for store := StoreLedger; store <= StoreMetadata; store++ {
		claims.Heads[store-StoreLedger], err = old.HeadAt(store, cursors[store-StoreLedger])
		if err != nil {
			t.Fatal(err)
		}
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	signed, err := SignMetadataBaselineCarrier(claims, key)
	if err != nil {
		t.Fatal(err)
	}
	member := validMemberState()
	authority, err := QualifyMetadataBaselineCarrier(signed, key, member)
	if err != nil {
		t.Fatal(err)
	}
	options.MetadataBaseline = &MetadataBaselineSourceOptions{Authority: authority, Path: newPath, Key: options.MetadataKey}
	source, err := NewNativeSource(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	return source, journal, authority, options, successor, signed
}

func TestMetadataBaselineIndependentCarrierRejectsScopeAndTampering(t *testing.T) {
	_, _, a, _, _, signed := metadataBoundaryFixture(t)
	key := []byte("0123456789abcdef0123456789abcdef")
	for _, change := range []func(*MemberState){func(s *MemberState) { s.Incarnation = "foreign" }, func(s *MemberState) { s.Term++; s.PromisedTerm++ }, func(s *MemberState) { s.Peers = s.Peers[:1] }, func(s *MemberState) { s.Role = RoleReplica }, func(s *MemberState) { s.PromisedTerm++ }} {
		state := validMemberState()
		change(&state)
		if _, err := QualifyMetadataBaselineCarrier(signed, key, state); err == nil {
			t.Fatal("foreign scope acquired authority")
		}
	}
	bad := bytes.Clone(signed)
	bad[20] ^= 1
	if _, err := QualifyMetadataBaselineCarrier(bad, key, validMemberState()); err == nil {
		t.Fatal("tampering accepted")
	}
	claims := a.Claims()
	claims.Members[0] = "foreign"
	if a.Claims().Members[0] != "halro-0" {
		t.Fatal("borrowed claims mutated authority")
	}
}

func TestMetadataBaselineHelloBindsIndependentCarrierBeforeCallback(t *testing.T) {
	local := testHello("halro-0", RolePrimary, 1)
	local.Protocol = VersionRange{Current: 2, Minimum: 2, Maximum: 2}
	local.Metadata = VersionRange{Current: 1, Minimum: 1, Maximum: 2}
	local.MetadataBaselineAuthority = sha256.Sum256([]byte("independent actual group carrier"))
	peer := local
	peer.NodeID = "halro-1"
	peer.Role = RoleReplica
	peer.Nonce[0]++
	encoded, err := local.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalHello(encoded)
	if err != nil || decoded != local {
		t.Fatal(decoded, err)
	}
	if err := validatePeerHelloCompatibility(local, peer, "halro-1"); err != nil {
		t.Fatal(err)
	}
	peer.MetadataBaselineAuthority[0] ^= 1
	if err := validatePeerHelloCompatibility(local, peer, "halro-1"); err == nil {
		t.Fatal("foreign approved carrier reached authenticated callback")
	}
	peer = testHello("halro-1", RoleReplica, 2)
	if err := validatePeerHelloCompatibility(local, peer, "halro-1"); err == nil {
		t.Fatal("legacy wire1 member joined maintenance")
	}
	local.MetadataBaselineAuthority = [32]byte{}
	if _, err := local.MarshalBinary(); err == nil {
		t.Fatal("wire2 declaration without independent authority accepted")
	}
}

func TestMetadataBaselineActualSourceSinkOrderingAndConfirmation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	source, journal, a, options, successor, _ := metadataBoundaryFixture(t)
	n := a.Claims().Index
	if v, err := source.MetadataFormatVersion(); err != nil || v != 1 {
		t.Fatal(v, err)
	}
	if _, err := source.Read(StoreMetadata, 2, 1, 1); err == nil {
		t.Fatal("candidate presence granted new source authority")
	}
	metadata, err := a.Descriptor().MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	record := OrderingRecord{Kind: KindMetadataBaseline, Index: n + 1, Term: 7, ConfirmedIndex: n, Metadata: metadata}
	if err := source.ActivateMetadataBaseline(journal, n+1); err == nil {
		t.Fatal("absent ordering granted successor")
	}
	if err := source.ValidateMetadataBaselineOrdering(journal, record); err != nil {
		t.Fatal(err)
	}
	replicaJournalPath := filepath.Join(t.TempDir(), "ordering.journal")
	oldOrder, err := os.ReadFile(journal.file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replicaJournalPath, oldOrder, 0600); err != nil {
		t.Fatal(err)
	}
	replicaJournal, err := OpenExistingOrderingJournal(replicaJournalPath, []byte("0123456789abcdef0123456789abcdef"), "production-a", "inc_01", n, a.Claims().OrderingHead)
	if err != nil {
		t.Fatal(err)
	}
	defer replicaJournal.Close()
	replicaPath := filepath.Join(t.TempDir(), "metadata-successor.journal")
	header, err := os.ReadFile(options.MetadataBaseline.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replicaPath, header, 0600); err != nil {
		t.Fatal(err)
	}
	replicaOptions := options
	replicaOptions.MetadataBaseline = &MetadataBaselineSourceOptions{Authority: a, Path: replicaPath, Key: options.MetadataKey}
	replicaSource, err := NewNativeSource(replicaOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer replicaSource.Close()
	replicaSuccessor, err := metadatajournal.OpenBoundReplicaContext(ctx, replicaPath, options.MetadataKey, a.Claims().New)
	if err != nil {
		t.Fatal(err)
	}
	defer replicaSuccessor.Close()
	ll, err := ledger.OpenWithOptions(options.LedgerPath, ledger.NewStatus(), ledger.Options{Replica: true, ChainKey: options.LedgerKey})
	if err != nil {
		t.Fatal(err)
	}
	defer ll.Close()
	al, err := audit.OpenWithOptions(options.AuditPath, options.AuditKey, audit.Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer al.Close()
	gl, err := governance.OpenWithOptions(options.GovernancePath, options.GovernanceKey, governance.Options{Replica: true})
	if err != nil {
		t.Fatal(err)
	}
	defer gl.Close()
	ml, err := metadatajournal.OpenReplicaContext(ctx, options.MetadataPath, options.MetadataKey)
	if err != nil {
		t.Fatal(err)
	}
	defer ml.Close()
	sink, err := NewNativeSink(ll, al, gl, ml)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.ConfigureMetadataBaseline(a, replicaSource, replicaSuccessor); err != nil {
		t.Fatal(err)
	}
	receiver, err := NewReplicaReceiver("production-a", "inc_01", "halro-1", "halro-0", 7, 7, n, n, replicaJournal, sink, ReplicaReceiverOptions{Projection: ProjectionState{Index: n, MetadataEpoch: 1, MetadataSequence: 2}})
	if err != nil {
		t.Fatal(err)
	}
	outbound := &recordingOutbound{}
	primary, err := NewPrimaryCoordinatorFromSource("production-a", "inc_01", "halro-0", 7, n, []string{"halro-1", "halro-2"}, journal, outbound, func(PrimaryProgress) error { return nil }, source, DefaultPrimaryWindowLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Stop()
	frames := make(chan []byte, 4)
	if err := primary.PreparePeer("halro-1", n, n, func(b []byte) error { frames <- bytes.Clone(b); return nil }); err != nil {
		t.Fatal(err)
	}
	completed := make(chan error, 1)
	go func() { _, err := primary.RecordMetadataBaseline(ctx, a); completed <- err }()
	var encoded []byte
	for {
		select {
		case encoded = <-frames:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if _, err := UnmarshalFrame(encoded); err == nil {
			break
		}
		if _, err := UnmarshalCommitNotice(encoded); err != nil {
			t.Fatal(err)
		}
	}
	boundary, err := UnmarshalFrame(encoded)
	if err != nil || boundary.Kind != KindMetadataBaseline || boundary.Index != n+1 || boundary.ConfirmedIndex != n {
		t.Fatal(boundary, err)
	}
	select {
	case err := <-completed:
		t.Fatal("completed before actual Replica ACK", err)
	default:
	}
	bad := boundary
	bad.Metadata = bytes.Clone(boundary.Metadata)
	bad.Metadata[127] ^= 1
	wrong, err := bad.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Receive(wrong); err == nil {
		t.Fatal("wrong independent authority digest accepted")
	}
	if index, _, _ := replicaJournal.Head(); index != n {
		t.Fatal("rejected boundary advanced actual ordering", index)
	}
	ack, err := receiver.Receive(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if ack.DurableIndex != n+1 {
		t.Fatal(ack)
	}
	if v, err := replicaSource.MetadataFormatVersion(); err != nil || v != 2 {
		t.Fatal(v, err)
	}
	if confirmed, err := primary.Acknowledge(ack); err != nil || confirmed != n+1 {
		t.Fatal(confirmed, err)
	}
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	cursor, err := journal.StoreCursors()
	if err != nil {
		t.Fatal(err)
	}
	expected := a.Claims().Cursors
	expected[StoreMetadata-StoreLedger] = StoreCursor{Generation: 2}
	if cursor != expected {
		t.Fatal("boundary changed another native cursor", cursor, expected)
	}
	if err := source.Truncate(StoreMetadata, StoreCursor{Generation: 1, Sequence: 1}); err == nil {
		t.Fatal("read-only history mutated after baseline")
	}
	if _, err := source.Read(StoreMetadata, 1, 1, 2); err != nil {
		t.Fatal("old ordered history unavailable", err)
	}
	if err := successor.SetBoundReplicationAdmissionAndAfterDurable(a.Claims().New, func(b metadatajournal.DurableBatch) (metadatajournal.AppendAdmission, error) {
		return primary.ReserveNativeSourceUnconfirmed(len(b.Frames))
	}, func(b metadatajournal.DurableBatch) (uint64, error) {
		c, err := primary.RecordMetadataBatch(b)
		return c.Index, err
	}, primary.WaitConfirmed); err != nil {
		t.Fatal(err)
	}
	receipt, err := successor.AppendWithReceipt([]metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("new"), Value: []byte("v")}})
	if err != nil || receipt.ReplicationIndex != n+2 {
		t.Fatal(receipt, err)
	}
	for {
		select {
		case encoded = <-frames:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if _, err := UnmarshalFrame(encoded); err == nil {
			break
		}
		if _, err := UnmarshalCommitNotice(encoded); err != nil {
			t.Fatal(err)
		}
	}
	ack, err = receiver.Receive(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if replicaSuccessor.Head() != successor.Head() {
		t.Fatal("ACK before exact successor native fsync")
	}
	if confirmed, err := primary.Acknowledge(ack); err != nil || confirmed != n+2 {
		t.Fatal(confirmed, err)
	}
	if err := successor.WaitConfirmed(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err := ReconstructRangeEach("production-a", "inc_01", 1, n+2, journal, source, func(LocalCommit) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := RecoverReplicaStores(replicaJournal, replicaSource); err != nil {
		t.Fatal(err)
	}
}
