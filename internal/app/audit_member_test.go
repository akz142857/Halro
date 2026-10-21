package app

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

// On an HA member the audit checkpoint is node-derived and a Replica never
// advances it, so `audit verify` asks only that it name a point on the
// verified chain.
func TestMemberAuditCheckpointMustBeAPointOnTheChain(t *testing.T) {
	key := bytes.Repeat([]byte{0x41}, 32)
	path := filepath.Join(t.TempDir(), "audit.log")
	log, err := audit.Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"evt_1", "evt_2", "evt_3"} {
		if _, err := log.Append(context.Background(), audit.Event{
			EventID: id, OccurredAt: time.Now().UTC(), ActorType: "system", Action: "test", Outcome: "success",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	records := map[uint64]audit.Record{}
	summary, err := audit.VerifyWithVisitor(path, key, func(record audit.Record) error {
		records[record.Sequence] = record
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	behind := records[2]
	cases := []struct {
		name       string
		checkpoint boltstore.AuditCheckpoint
		at         audit.Record
		ok         bool
	}{
		{"at the end", boltstore.AuditCheckpoint{Records: 3, Bytes: summary.Bytes, LastHash: summary.LastHash}, records[3], true},
		{"behind, on the chain", boltstore.AuditCheckpoint{Records: 2, Bytes: behind.Offset, LastHash: behind.Hash}, behind, true},
		{"empty", boltstore.AuditCheckpoint{}, audit.Record{}, true},
		{"past the end", boltstore.AuditCheckpoint{Records: 4, Bytes: summary.Bytes + 1, LastHash: summary.LastHash}, audit.Record{}, false},
		{"another hash", boltstore.AuditCheckpoint{Records: 2, Bytes: behind.Offset, LastHash: records[1].Hash}, behind, false},
		{"another length", boltstore.AuditCheckpoint{Records: 2, Bytes: behind.Offset - 1, LastHash: behind.Hash}, behind, false},
	}
	for _, c := range cases {
		if got := auditCheckpointIsPrefix(c.checkpoint, summary, c.at); got != c.ok {
			t.Fatalf("%s: prefix=%t, want %t", c.name, got, c.ok)
		}
	}
}
