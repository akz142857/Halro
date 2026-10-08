package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/replication"
)

func TestReplicationHelloUsesQualifiedActualMetadataVersion(t *testing.T) {
	for _, scenario := range []struct {
		name             string
		bound, qualified bool
		want             uint16
	}{{"format1", false, false, 1}, {"format2-qualified", true, true, 2}, {"format2-no-authority", true, false, 0}} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			key := bytes.Repeat([]byte{0x31}, 32)
			path := filepath.Join(dir, "metadata.journal")
			var log *metadatajournal.Log
			var authority *metadatajournal.BoundReplicationAuthority
			var err error
			if scenario.bound {
				b := metadatajournal.BaselineBinding{Version: 1, Epoch: 9, KeyGeneration: 2, Schema: 45, Rows: 17}
				for i, p := range []*[32]byte{&b.Identity, &b.Catalog, &b.Scope, &b.Cut, &b.RootDigest} {
					*p = sha256.Sum256([]byte{byte(i + 1)})
				}
				b.RootID[0] = 13
				log, err = metadatajournal.StartBoundEpoch(ctx, path, key, metadatajournal.EpochHeader{Epoch: 9, Reason: "independent-test-baseline", Baseline: &b, FileRevision: 1}, 64<<20)
				if scenario.qualified {
					authority = &metadatajournal.BoundReplicationAuthority{Baseline: b, FileRevision: 1, MaxFileBytes: 64 << 20}
				}
			} else {
				log, err = metadatajournal.StartEpoch(path, key, metadatajournal.EpochHeader{Epoch: 1, Reason: "format1-test"})
			}
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			source, err := replication.NewNativeSource(replication.NativeSourceOptions{LedgerPath: filepath.Join(dir, "ledger.wal"), AuditPath: filepath.Join(dir, "audit.log"), GovernancePath: filepath.Join(dir, "governance.log"), MetadataPath: path, LedgerKey: key, AuditKey: key, GovernanceKey: key, MetadataKey: key, MetadataAuthority: authority})
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			state := replication.MemberState{Version: replication.StateVersion, ClusterID: "production-a", Incarnation: "inc_01", NodeID: "halro-0", Role: replication.RolePrimary, Term: 7, PromisedTerm: 7, Peers: []replication.StatePeer{{Name: "halro-1", Address: "halro-1.internal:9910", SPKISHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}
			publisher, err := replication.NewStatePublisher(filepath.Join(dir, "cluster", "state.json"), key, state)
			if err != nil {
				t.Fatal(err)
			}
			r := &replicationRuntime{publisher: publisher, source: source}
			hello, err := r.localHello()
			if scenario.want == 0 {
				if err == nil {
					t.Fatal("unqualified candidate advertised format2", hello)
				}
				return
			}
			if err != nil || hello.Metadata != (replication.VersionRange{Current: scenario.want, Minimum: scenario.want, Maximum: scenario.want}) {
				t.Fatal(hello, err)
			}
			if err := hello.Validate(); err != nil {
				t.Fatal(err)
			}
			peer := hello
			peer.NodeID = "halro-1"
			peer.Role = replication.RoleReplica
			peer.Nonce[0] ^= 1
			if err := replication.ValidatePeerHello(hello, peer, "halro-1", func(string, [replication.NonceBytes]byte) bool { return false }); err != nil {
				t.Fatal(err)
			}
			peer.Metadata = replication.VersionRange{Current: 3 - scenario.want, Minimum: 3 - scenario.want, Maximum: 3 - scenario.want}
			if err := replication.ValidatePeerHello(hello, peer, "halro-1", func(string, [replication.NonceBytes]byte) bool { return false }); err == nil {
				t.Fatal("mixed actual native formats negotiated")
			}
		})
	}
}
