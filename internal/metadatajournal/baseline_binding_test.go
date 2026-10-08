package metadatajournal

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testBinding() BaselineBinding {
	b := BaselineBinding{Version: 1, Epoch: 9, KeyGeneration: 2, Schema: 45, Rows: 17}
	for _, p := range []*[32]byte{&b.Identity, &b.Catalog, &b.Scope, &b.Cut, &b.RootDigest} {
		*p = sha256.Sum256([]byte{byte(len(p[:])), byte(len(p[:]))})
	}
	b.RootID[0] = 13
	return b
}
func newBoundJournal(t *testing.T) (string, *Log, BaselineBinding) {
	t.Helper()
	binding := testBinding()
	path := filepath.Join(t.TempDir(), "metadata.journal")
	l, err := StartBoundEpoch(context.Background(), path, testKey(), EpochHeader{Epoch: 9, PreviousEpoch: 8, Reason: "test-baseline", Baseline: &binding, FileRevision: 1}, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	})
	return path, l, binding
}

func TestBoundEpochRequiresIndependentRootBeforeRepairAndClonesBinding(t *testing.T) {
	path, l, binding := newBoundJournal(t)
	for range 3 {
		record, err := l.Append([]Op{put("projects", "p", "v")})
		if err != nil || record.FormatVersion != 2 {
			t.Fatal(record, err)
		}
	}
	wanted := l.Head()
	if wanted.Baseline != binding || wanted.FileRevision != 1 || wanted.FormatVersion != 2 {
		t.Fatal(wanted)
	}
	if _, err := ReadReplicationFrames(path, testKey(), 9, 1, 3); err == nil {
		t.Fatal("legacy replication reader exported bound frames")
	}
	if _, _, err := ReplicationCursor(path, testKey()); err == nil {
		t.Fatal("unqualified cursor advertised bound state")
	}
	if _, err := ReplicationHeadAt(path, testKey(), 9, 0); err == nil {
		t.Fatal("unqualified head lookup accepted bound state")
	}
	if err := TruncateReplicationTail(path, testKey(), 9, 0); err == nil {
		t.Fatal("unqualified tail truncation accepted bound state")
	}
	if err := RepairReplicaTail(path, testKey(), 9, 3); err == nil {
		t.Fatal("unqualified repair accepted bound state")
	}
	rangeReader, err := NewReplicationRangeReader(path, testKey(), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rangeReader.Read(9, 1, 3); err == nil {
		t.Fatal("unqualified range reader accepted bound state")
	}
	rangeReader.Close()
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
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Open(path, testKey()); err == nil || got != nil {
		t.Fatal("ordinary Open accepted bound state", got, err)
	}
	for _, shape := range []string{"identity", "epoch", "key generation", "root digest", "row count", "scope", "cut", "revision", "disk"} {
		t.Run(shape, func(t *testing.T) {
			expected := binding
			revision, limit := uint64(1), int64(64<<20)
			switch shape {
			case "identity":
				expected.Identity[0] ^= 1
			case "epoch":
				expected.Epoch++
			case "key generation":
				expected.KeyGeneration++
			case "root digest":
				expected.RootDigest[0] ^= 1
			case "row count":
				expected.Rows++
			case "scope":
				expected.Scope[0] ^= 1
			case "cut":
				expected.Cut[0] ^= 1
			case "revision":
				revision++
			case "disk":
				limit = 1
			}
			got, err := OpenBoundContext(context.Background(), path, testKey(), expected, revision, limit)
			if err == nil || got != nil {
				t.Fatal("wrong expectation accepted", got, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("wrong expectation repaired source", err)
			}
		})
	}
	got, err := OpenBoundContext(context.Background(), path, testKey(), binding, 1, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got.Head() != wanted {
		t.Fatal(got.Head(), wanted)
	}
	if err := got.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Discard(); err != nil {
		t.Fatal(err)
	}
}

func TestBoundTrimRetainsAssociationAndRevisionsAcrossNoopFullCutAndAppend(t *testing.T) {
	path, l, binding := newBoundJournal(t)
	for range 8 {
		if _, err := l.Append([]Op{put("projects", "p", "value")}); err != nil {
			t.Fatal(err)
		}
	}
	full := l.Head()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	initial, _ := os.ReadFile(path)
	zero, err := Trim(path, testKey(), 0)
	if err != nil || zero != full {
		t.Fatal(zero, err)
	}
	unchanged, _ := os.ReadFile(path)
	if !bytes.Equal(initial, unchanged) {
		t.Fatal("zero trim rewrote native source")
	}
	first, err := Trim(path, testKey(), 3)
	if err != nil || first.Baseline != binding || first.FileRevision != 2 || first.Hash != full.Hash || first.TrimmedThrough != 3 {
		t.Fatal(first, err)
	}
	old, _ := os.ReadFile(path)
	noop, err := Trim(path, testKey(), 3)
	now, _ := os.ReadFile(path)
	if err != nil || noop != first || !bytes.Equal(old, now) {
		t.Fatal("repeated cut rewrote revision", noop, err)
	}
	if got, err := OpenBoundContext(context.Background(), path, testKey(), binding, 1, 64<<20); err == nil || got != nil {
		t.Fatal("stale physical revision accepted", got, err)
	}
	last, err := Trim(path, testKey(), 8)
	if err != nil || last.FileRevision != 3 || last.Baseline != binding || last.Hash != full.Hash {
		t.Fatal(last, err)
	}
	reader, err := OpenBoundContext(context.Background(), path, testKey(), binding, 3, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	visited := false
	if err := reader.ReplayRangeValidated(0, 8, func(Record) error { visited = true; return nil }); err == nil || visited {
		t.Fatal("trim gap treated as replayable", visited, err)
	}
	record, err := reader.Append([]Op{put("projects", "later", "v")})
	if err != nil || record.Sequence != 9 || record.FormatVersion != 2 {
		t.Fatal(record, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	verified, err := Verify(path, testKey())
	if err != nil || verified.Baseline != binding || verified.FileRevision != 3 || verified.Sequence != 9 {
		t.Fatal(verified, err)
	}
	// The old publisher's deletion receipt names a replaced inode. Refusal is
	// required; physical rewrite receipt handoff is a separate owner protocol.
	if err := l.Discard(); err == nil {
		t.Fatal("old publisher deleted rewritten source")
	}
}

func TestBoundControlRejectsSignedMalformedNoncanonicalAndMixedVersions(t *testing.T) {
	binding := testBinding()
	base := EpochHeader{Epoch: 9, PreviousEpoch: 8, Reason: "test-baseline", Baseline: &binding, FileRevision: 1}
	for _, shape := range []string{"missing binding", "wrong binding epoch", "wrong payload epoch", "zero revision", "unknown field", "whitespace", "duplicate field", "legacy envelope", "reserved bytes", "mixed operations"} {
		t.Run(shape, func(t *testing.T) {
			h := base
			b := binding
			h.Baseline = &b
			switch shape {
			case "missing binding":
				h.Baseline = nil
			case "wrong binding epoch":
				b.Epoch++
			case "wrong payload epoch":
				h.Epoch++
			case "zero revision":
				h.FileRevision = 0
			}
			payload, err := json.Marshal(h)
			if err != nil {
				t.Fatal(err)
			}
			switch shape {
			case "unknown field":
				payload = append(payload[:len(payload)-1], []byte(",\"other\":1}")...)
			case "whitespace":
				payload = append([]byte(" "), payload...)
			case "duplicate field":
				payload = append(payload[:len(payload)-1], []byte(",\"file_revision\":1}")...)
			}
			version := uint8(2)
			if shape == "legacy envelope" {
				version = 1
			}
			frame, hash := encodeFrameVersion(version, testKey(), KindEpoch, 9, 0, [32]byte{}, payload)
			if shape == "reserved bytes" {
				frame[6] = 1
				mac := hmac.New(sha256.New, testKey())
				mac.Write(frame[:len(frame)-frameMACSize])
				copy(frame[len(frame)-frameMACSize:], mac.Sum(nil))
			}
			if shape == "mixed operations" {
				p, err := encodePayload(KindOperations, []Op{put("projects", "p", "v")}, EpochHeader{}, TrimAnchor{})
				if err != nil {
					t.Fatal(err)
				}
				suffix, _ := encodeFrame(testKey(), KindOperations, 9, 1, hash, p)
				frame = append(frame, suffix...)
			}
			path := filepath.Join(t.TempDir(), "metadata.journal")
			if err := os.WriteFile(path, frame, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Verify(path, testKey()); !errors.Is(err, ErrCorrupt) {
				t.Fatal("signed malformed control accepted", err)
			}
		})
	}
}

func TestBoundTrimSourceReplacementAndCancellationPreservePublishedFile(t *testing.T) {
	for _, shape := range []string{"replacement", "cancel after source", "cancel after sync"} {
		t.Run(shape, func(t *testing.T) {
			path, l, _ := newBoundJournal(t)
			for range 3 {
				if _, err := l.Append([]Op{put("projects", "p", "v")}); err != nil {
					t.Fatal(err)
				}
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := trimContext(ctx, path, testKey(), 1, func(phase string) {
				if phase == "source-verified" && shape == "replacement" {
					other := path + ".foreign"
					if err := os.WriteFile(other, []byte("foreign"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(other, path); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "source-verified" && shape == "cancel after source" || phase == "candidate-synced" && shape == "cancel after sync" {
					cancel()
				}
			})
			if err == nil {
				t.Fatal("failed trim published")
			}
			raw, _ := os.ReadFile(path)
			if shape == "replacement" {
				if string(raw) != "foreign" {
					t.Fatal("foreign source replaced")
				}
			} else if !bytes.Equal(before, raw) {
				t.Fatal("canceled trim changed source")
			}
			matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".metadata-journal-*"))
			if err != nil || len(matches) != 0 {
				t.Fatal("candidate leaked", matches, err)
			}
		})
	}
}

func TestBoundAppendAndCreateLimitsRefuseBeforeChangingBytes(t *testing.T) {
	path, l, _ := newBoundJournal(t)
	before, _ := os.ReadFile(path)
	for _, ops := range [][]Op{make([]Op, 1025), {{Kind: OpPut, Path: make([]string, 33), Key: []byte("k"), Value: []byte("v")}}, {{Kind: OpPut, Path: []string{"projects"}, Key: []byte("k"), Value: make([]byte, MaxPayloadSize)}}} {
		if _, err := l.Append(ops); err == nil {
			t.Fatal("unbounded encoding accepted")
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("refused encoding changed source")
		}
	}
	l.maxBytes = l.Head().Offset
	if _, err := l.Append([]Op{put("projects", "p", "v")}); err == nil {
		t.Fatal("disk ceiling ignored")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("disk refusal changed source")
	}
	binding := testBinding()
	binding.Rows++
	if got, err := StartBoundEpoch(context.Background(), path, testKey(), EpochHeader{Epoch: 9, PreviousEpoch: 8, Reason: "replace", Baseline: &binding, FileRevision: 1}, 64<<20); err == nil || got != nil {
		t.Fatal("exclusive create replaced existing journal", got, err)
	}
	if err := l.Discard(); err != nil {
		t.Fatal(err)
	}
}

func TestBoundOperationDecoderRejectsSignedCollectionAndCodecOverflow(t *testing.T) {
	for _, shape := range []string{"operation count", "path depth", "key length", "value length", "unknown field", "duplicate field", "whitespace"} {
		t.Run(shape, func(t *testing.T) {
			binding := testBinding()
			header := EpochHeader{Epoch: 9, PreviousEpoch: 8, Reason: "fixture", Baseline: &binding, FileRevision: 1}
			control, _ := json.Marshal(header)
			first, hash := encodeFrameVersion(2, testKey(), KindEpoch, 9, 0, [32]byte{}, control)
			ops := []Op{put("projects", "p", "value")}
			switch shape {
			case "operation count":
				ops = make([]Op, 1025)
				for i := range ops {
					ops[i] = put("projects", "p", "v")
				}
			case "path depth":
				ops[0].Path = make([]string, 33)
				for i := range ops[0].Path {
					ops[0].Path[i] = "projects"
				}
			case "key length":
				ops[0].Key = make([]byte, 32769)
			case "value length":
				ops[0].Value = make([]byte, (3<<20)+1)
			}
			payload, err := json.Marshal(ops)
			if err != nil {
				t.Fatal(err)
			}
			switch shape {
			case "unknown field":
				payload = bytes.Replace(payload, []byte(`"kind":"put"`), []byte(`"kind":"put","other":1`), 1)
			case "duplicate field":
				payload = bytes.Replace(payload, []byte(`"kind":"put"`), []byte(`"kind":"put","kind":"put"`), 1)
			case "whitespace":
				payload = append([]byte(" "), payload...)
			}
			suffix, _ := encodeFrameVersion(2, testKey(), KindOperations, 9, 1, hash, payload)
			path := filepath.Join(t.TempDir(), "metadata.journal")
			if err := os.WriteFile(path, append(first, suffix...), 0600); err != nil {
				t.Fatal(err)
			}
			visited := false
			if _, err := Replay(path, testKey(), func(r Record) error {
				if r.Kind == KindOperations {
					visited = true
				}
				return nil
			}); !errors.Is(err, ErrCorrupt) || visited {
				t.Fatal("oversized signed op visited", visited, err)
			}
		})
	}
}

func TestBoundTrimStreamsLargeRetainedSuffixAndPreservesEveryNativeFact(t *testing.T) {
	path, l, binding := newBoundJournal(t)
	const count = 160
	for n := range count {
		if _, err := l.Append([]Op{{Kind: OpPut, Path: []string{"projects"}, Key: []byte("p"), Value: bytes.Repeat([]byte{byte(n)}, 64<<10)}}); err != nil {
			t.Fatal(err)
		}
	}
	full := l.Head()
	if full.Offset < 8<<20 {
		t.Fatal("fixture does not exceed one frame limit", full)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	trimmed, err := Trim(path, testKey(), 1)
	if err != nil || trimmed.Hash != full.Hash || trimmed.Baseline != binding || trimmed.FileRevision != 2 {
		t.Fatal(trimmed, err)
	}
	seen := 0
	if _, err := Replay(path, testKey(), func(r Record) error {
		if r.Kind != KindOperations {
			return nil
		}
		want := uint64(seen + 2)
		if r.Sequence != want || len(r.Ops) != 1 || !bytes.Equal(r.Ops[0].Value, bytes.Repeat([]byte{byte(want - 1)}, 64<<10)) {
			t.Fatal("retained fact differs", r.Sequence, want)
		}
		seen++
		return nil
	}); err != nil || seen != count-1 {
		t.Fatal(seen, err)
	}
}

func TestIncompleteControlFrameNeverGrantsOrdinaryRepair(t *testing.T) {
	for _, version := range []uint8{1, 2} {
		payload, _ := json.Marshal(EpochHeader{Epoch: 1, Reason: "incomplete"})
		frame, _ := encodeFrameVersion(version, testKey(), KindEpoch, 1, 0, [32]byte{}, payload)
		for _, size := range []int{3, frameHeaderSize + 1, len(frame) - 1} {
			path := filepath.Join(t.TempDir(), "metadata.journal")
			prefix := frame[:size]
			if err := os.WriteFile(path, prefix, 0600); err != nil {
				t.Fatal(err)
			}
			if got, err := Open(path, testKey()); !errors.Is(err, ErrCorrupt) || got != nil {
				t.Fatal("incomplete control was repaired", version, size, got, err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(after, prefix) {
				t.Fatal("incomplete control changed on disk")
			}
		}
	}
}
