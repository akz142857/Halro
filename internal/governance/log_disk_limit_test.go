package governance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestGovernanceJournalDiskLimitRejectsInvalidCeiling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.log")
	if log, err := OpenWithOptions(path, make([]byte, governanceHMACKeySize), Options{MaxDiskBytes: -1}); log != nil || !errors.Is(err, ErrJournalDiskLimitInvalid) {
		t.Fatal("negative disk ceiling accepted", log, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid disk ceiling created the journal", err)
	}
}

func TestGovernanceJournalDiskLimitPreflightsBatchAndAllowsRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "governance.log")
	key := make([]byte, governanceHMACKeySize)
	event := projectionEvent(1)
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	maxBytes := int64(frameHeaderSize + len(payload) + frameMACSize)
	log, err := OpenWithOptions(path, key, Options{MaxDiskBytes: maxBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	if _, err := log.AppendBatch(context.Background(), []Event{event, projectionEvent(2)}); !errors.Is(err, ErrJournalDiskLimit) {
		t.Fatal("oversized batch was not rejected before append", err)
	}
	if summary := log.Summary(); summary != (Summary{}) {
		t.Fatal("rejected batch advanced journal position", summary)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatal("rejected batch wrote a partial journal", info, err)
	}
	if _, err := log.Append(context.Background(), event); err != nil {
		t.Fatal("single event did not fit exact remaining ceiling", err)
	}
	before := log.Summary()
	if _, err := log.Append(context.Background(), projectionEvent(2)); !errors.Is(err, ErrJournalDiskLimit) {
		t.Fatal("append past the ceiling was accepted", err)
	}
	if after := log.Summary(); after != before {
		t.Fatal("capacity refusal changed the durable head", before, after)
	}
	if log.terminalErr != nil {
		t.Fatal("known capacity refusal poisoned the journal", log.terminalErr)
	}
	if _, err := log.Append(context.Background(), event); err != nil {
		t.Fatal("idempotent retry at the ceiling should not write", err)
	}
}

func TestGovernanceJournalDiskLimitRejectsExistingAndReplicatedGrowth(t *testing.T) {
	key := make([]byte, governanceHMACKeySize)
	dir := t.TempDir()
	primaryPath := filepath.Join(dir, "primary.log")
	primary, err := Open(primaryPath, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.Append(context.Background(), projectionEvent(1)); err != nil {
		t.Fatal(err)
	}
	head := primary.Summary()
	frames, err := ReadReplicationFrames(primaryPath, key, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}

	if log, err := OpenWithOptions(primaryPath, key, Options{MaxDiskBytes: head.Bytes - 1}); log != nil || !errors.Is(err, ErrJournalDiskLimit) {
		t.Fatal("existing journal larger than its configured ceiling was accepted", log, err)
	}

	replicaPath := filepath.Join(dir, "replica.log")
	if err := os.WriteFile(replicaPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	replica, err := OpenWithOptions(replicaPath, key, Options{Replica: true, MaxDiskBytes: int64(len(frames) - 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if err := replica.AppendReplicated(1, 1, frames); !errors.Is(err, ErrJournalDiskLimit) {
		t.Fatal("replicated batch exceeded the configured ceiling", err)
	}
	if summary := replica.Summary(); summary != (Summary{}) {
		t.Fatal("rejected replicated append advanced the journal", summary)
	}
	if info, err := os.Stat(replicaPath); err != nil || info.Size() != 0 {
		t.Fatal("rejected replicated append wrote bytes", info, err)
	}
}
