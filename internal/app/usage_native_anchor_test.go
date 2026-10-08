package app

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/akz142857/Halro/internal/replication"
)

func TestUsageNativeAnchorAdvancesDespiteRetiredLegacyCollector(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	installLegacyUsageForTest(t, r) // Explicit old checkpoint baseline.
	r.stopBackgroundWork()          // Freeze the old maintenance generation before baselines.
	r.saveUsageCheckpoint()
	oldWM, oldPayload, err := r.store.UsageCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	oldAnchor, err := r.store.LedgerChainCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	seedLedgerActivity(t, r, "native_after_checkpoint")
	if err := r.usageCollector.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.saveUsageCheckpoint() // CatchUp must fail, native protection must succeed.
	head, hash, ok := r.ledger.ChainHead()
	anchor, err := r.store.LedgerChainCheckpoint()
	if err != nil || !ok || anchor.Sequence <= oldAnchor.Sequence || anchor.Sequence != head.Sequence || anchor.Generation != head.Generation || anchor.Offset != head.Offset || anchor.Hash != hash {
		t.Fatal("native anchor depends on Collector", anchor, head, err)
	}
	wm, payload, err := r.store.UsageCheckpoint()
	if err != nil || wm != oldWM || !bytes.Equal(payload, oldPayload) {
		t.Fatal("failed derivative advanced checkpoint", wm, err)
	}
}

func TestUsageNativeAnchorDiskViewDoesNotWriteLegacyCheckpointAndDetectsDeletion(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	installLegacyUsageForTest(t, r) // Explicit old checkpoint baseline.
	r.stopBackgroundWork()          // Freeze the old maintenance generation before baselines.
	r.saveUsageCheckpoint()
	oldWM, oldPayload, err := r.store.UsageCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	seedLedgerActivity(t, r, "disk_native_tail")
	p := installDiskUsageQueries(t, r)
	r.saveUsageCheckpoint() // Aggregate is nil; disk is already committed locally.
	head, hash, ok := r.ledger.ChainHead()
	anchor, err := r.store.LedgerChainCheckpoint()
	if err != nil || !ok || anchor.Sequence != head.Sequence || anchor.Hash != hash {
		t.Fatal(anchor, head, err)
	}
	wm, payload, err := r.store.UsageCheckpoint()
	if err != nil || wm != oldWM || !bytes.Equal(payload, oldPayload) {
		t.Fatal("disk view fabricated a restart checkpoint", wm, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(r.config.LedgerPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyLedger(context.Background(), r.config); err == nil {
		t.Fatal("native deletion accepted after disk maintenance")
	}
}

func TestUsageNativeAnchorReplicaRoleNeverProtectsUnappliedDurableTail(t *testing.T) {
	r, _ := summaryRuntime(t, 1)
	installLegacyUsageForTest(t, r) // Explicit old checkpoint baseline.
	r.stopBackgroundWork()          // Freeze the old maintenance generation before baselines.
	r.saveUsageCheckpoint()
	oldAnchor, err := r.store.LedgerChainCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	oldWM, oldPayload, err := r.store.UsageCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	seedLedgerActivity(t, r, "durable_replica_suffix")
	installDiskUsageQueries(t, r)
	// A method-level role guard with real native/Metadata facts. Full HA
	// applied-prefix publication is separately unfinished, not inferred here.
	r.replication = &replicationRuntime{role: replication.RoleReplica}
	defer func() { r.replication = nil }()
	r.saveUsageCheckpoint()
	r.saveReplicaUsageCheckpoint()
	r.advanceLedgerChainCheckpoint()
	anchor, err := r.store.LedgerChainCheckpoint()
	if err != nil || anchor != oldAnchor {
		t.Fatal("Replica anchored durable suffix", anchor, err)
	}
	wm, payload, err := r.store.UsageCheckpoint()
	if err != nil || wm != oldWM || !bytes.Equal(payload, oldPayload) {
		t.Fatal("Replica disk fabricated checkpoint", wm, err)
	}
}
