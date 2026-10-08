package metadatajournal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguredDiskLimitBoundsLocalAndReplicatedAppends(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.journal")
	source, err := StartEpoch(sourcePath, testKey(), EpochHeader{Epoch: 1, Reason: "source"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Append([]Op{put("projects", "p", "value")}); err != nil {
		t.Fatal(err)
	}
	frames, err := ReadReplicationFrames(sourcePath, testKey(), 1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	sourceSize := source.Head().Offset
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	localPath := filepath.Join(t.TempDir(), "local.journal")
	local, err := StartEpochWithLimit(localPath, testKey(), EpochHeader{Epoch: 1, Reason: "source"}, sourceSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.Append([]Op{put("projects", "p", "value")}); err != nil {
		t.Fatal("append that reaches the exact file ceiling was rejected:", err)
	}
	initial := local.Head()
	if initial.Offset != sourceSize {
		t.Fatalf("exact-limit append ended at %d bytes, want %d", initial.Offset, sourceSize)
	}
	if _, err := local.Append([]Op{put("projects", "p2", "another")}); err == nil {
		t.Fatal("append beyond the file ceiling succeeded")
	}
	if got := local.Head(); got != initial {
		t.Fatalf("refused append advanced head: got %+v, want %+v", got, initial)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(localPath); err != nil || info.Size() != initial.Offset {
		t.Fatalf("refused append changed file size: info=%v err=%v", info, err)
	}

	replicaPath := filepath.Join(t.TempDir(), "replica.journal")
	created, err := StartEpochWithLimit(replicaPath, testKey(), EpochHeader{Epoch: 1, Reason: "source"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ceiling := created.Head().Offset
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	replica, err := OpenReplicaContextWithLimit(t.Context(), replicaPath, testKey(), ceiling)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if err := replica.AppendReplicated(1, 1, 1, frames); err == nil {
		t.Fatal("replicated append beyond the file ceiling succeeded")
	}
	if got := replica.Head(); got.Sequence != 0 || got.Offset != ceiling {
		t.Fatalf("refused replicated append advanced head: %+v", got)
	}
	if info, err := os.Stat(replicaPath); err != nil || info.Size() != ceiling {
		t.Fatalf("refused replicated append changed file size: info=%v err=%v", info, err)
	}
}

func TestConfiguredDiskLimitRejectsExistingOversizedJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.journal")
	log, err := StartEpoch(path, testKey(), EpochHeader{Epoch: 1, Reason: "initialize"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append([]Op{put("projects", "p", "value")}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenContextWithLimit(t.Context(), path, testKey(), 1); err == nil {
		t.Fatal("oversized authenticated journal opened")
	}
	if _, err := OpenContextWithLimit(t.Context(), path, testKey(), -1); err == nil {
		t.Fatal("negative journal ceiling was accepted")
	}
}

func TestConfiguredDiskLimitSurvivesSafePrefixTrimAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.journal")
	log, err := StartEpoch(path, testKey(), EpochHeader{Epoch: 1, Reason: "initialize"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append([]Op{put("projects", "large", strings.Repeat("x", 8<<10))}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append([]Op{put("projects", "small", "retained")}); err != nil {
		t.Fatal(err)
	}
	limit := log.Head().Offset
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	head, err := TrimContextWithLimit(t.Context(), path, testKey(), 1, limit)
	if err != nil {
		t.Fatal("safe trim within the configured ceiling failed:", err)
	}
	if head.TrimmedThrough != 1 || head.Sequence != 2 || head.Offset > limit {
		t.Fatalf("trim did not preserve the expected bounded journal head: %+v limit=%d", head, limit)
	}
	reopened, err := OpenContextWithLimit(t.Context(), path, testKey(), limit)
	if err != nil {
		t.Fatal("trimmed journal did not reopen under the same ceiling:", err)
	}
	defer reopened.Close()
	if got := reopened.Head(); got != head {
		t.Fatalf("reopened head changed: got %+v, want %+v", got, head)
	}
}
