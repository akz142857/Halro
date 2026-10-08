package replication

import (
	"bytes"
	"errors"
	"testing"
)

func recoveryStreamFixture(t *testing.T) (*OrderingJournal, []LocalCommit, *fakeDurableSource) {
	t.Helper()
	primary, journal := newTestPrimary(t, &recordingOutbound{})
	t.Cleanup(func() { _ = journal.Close() })
	anchor, err := primary.RecordDurable(Frame{Kind: KindLeadershipEstablished})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("authenticated-source-frame")
	data, err := primary.RecordDurable(Frame{Kind: KindData, Store: StoreLedger, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	source := &fakeDurableSource{cursors: map[Store]StoreCursor{StoreLedger: {Generation: 1, Sequence: 1}}, payloads: map[string][]byte{sourceKey(StoreLedger, 1, 1, 1): payload}, truncated: map[Store]StoreCursor{}}
	return journal, []LocalCommit{anchor, data}, source
}

func TestPrimaryRecoveryStreamOwnsBorrowedBytesAndPublishesOnlyAfterCompleteValidation(t *testing.T) {
	journal, want, source := recoveryStreamFixture(t)
	if err := ReconcileLocalStores("production-a", "inc_01", 0, journal, source); err != nil {
		t.Fatal(err)
	}
	outbound := &recordingOutbound{}
	finished, persisted := false, 0
	coordinator, err := NewPrimaryCoordinatorStreaming("production-a", "inc_01", "halro-0", 7, 0, []string{"halro-1"}, journal, outbound, func(PrimaryProgress) error {
		if !finished || (persisted == 0 && len(outbound.frames) != 0) {
			t.Error("published before stream verification")
		}
		persisted++
		return nil
	}, func(consume func(LocalCommit) error) error {
		var buffer []byte
		for _, commit := range want {
			buffer = append(buffer[:0], commit.Encoded...)
			if err := consume(LocalCommit{Index: commit.Index, Encoded: buffer}); err != nil {
				return err
			}
			clear(buffer)
			if persisted != 0 || len(outbound.frames) != 0 {
				t.Error("side effect while suffix still borrowed")
			}
		}
		finished = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if persisted != 1 || len(outbound.frames) != 2 {
		t.Fatal(persisted, len(outbound.frames))
	}
	for i := range want {
		if !bytes.Equal(outbound.frames[i], want[i].Encoded) {
			t.Fatal("stream buffer reuse damaged owned frame", i)
		}
	}
	if got, err := coordinator.Acknowledge(testAcknowledgement("halro-1", 2)); err != nil || got != 2 {
		t.Fatal(got, err)
	}
}

func TestPrimaryRecoveryStreamRefusesInvalidOrIncompleteSuffixBeforeSideEffects(t *testing.T) {
	for _, stage := range []string{"nil", "producer-error", "late-corruption", "short", "extra", "ignored-extra", "gap", "ignored-consumer-error", "head-change", "state-fsync"} {
		t.Run(stage, func(t *testing.T) {
			journal, commits, _ := recoveryStreamFixture(t)
			outbound := &recordingOutbound{}
			persisted := 0
			stream := RecoveredCommitStream(func(consume func(LocalCommit) error) error {
				if stage == "gap" {
					commits[0].Index++
				}
				if stage == "ignored-consumer-error" {
					bad := commits[0]
					bad.Index++
					_ = consume(bad) // A producer cannot erase a failed validation.
				}
				for i, commit := range commits {
					if i == 1 && stage == "short" {
						break
					}
					if i == 1 && stage == "late-corruption" {
						commit.Encoded[len(commit.Encoded)-1] ^= 1
					}
					if err := consume(commit); err != nil {
						if stage == "ignored-consumer-error" {
							continue
						}
						return err
					}
				}
				switch stage {
				case "producer-error":
					return errors.New("source stopped after emitting prefix")
				case "extra":
					return consume(commits[1])
				case "ignored-extra":
					_ = consume(commits[1])
					return nil
				case "head-change":
					primary, err := NewPrimaryCoordinator("production-a", "inc_01", "halro-0", 7, 0, []string{"halro-1"}, journal, &recordingOutbound{}, func(PrimaryProgress) error { return nil }, commits...)
					if err != nil {
						return err
					}
					_, err = primary.RecordDurable(Frame{Kind: KindData, Store: StoreLedger, StoreGeneration: 1, StoreSequenceFirst: 2, StoreSequenceLast: 2, Payload: []byte("new-source")})
					return err
				}
				return nil
			})
			if stage == "nil" {
				stream = nil
			}
			_, err := NewPrimaryCoordinatorStreaming("production-a", "inc_01", "halro-0", 7, 0, []string{"halro-1"}, journal, outbound, func(PrimaryProgress) error {
				persisted++
				if stage == "state-fsync" {
					return errors.New("state fsync")
				}
				return nil
			}, stream)
			if err == nil {
				t.Fatal("invalid stream accepted")
			}
			wantPersist := 0
			if stage == "state-fsync" {
				wantPersist = 1
			}
			if persisted != wantPersist || len(outbound.frames) != 0 || len(outbound.notices) != 0 {
				t.Fatal("unverified suffix became visible", persisted, len(outbound.frames), len(outbound.notices))
			}
		})
	}
}

func TestRecoverLocalCommitsEachStopsAtConsumerFailureAndRetainsExactAuthenticatedRanges(t *testing.T) {
	journal, want, source := recoveryStreamFixture(t)
	if err := RecoverLocalCommitsEach("production-a", "inc_01", 0, journal, source, nil); err == nil {
		t.Fatal("nil consumer accepted")
	}
	stopped := errors.New("consumer stopped")
	consumed := 0
	if err := RecoverLocalCommitsEach("production-a", "inc_01", 0, journal, source, func(commit LocalCommit) error {
		consumed++
		if commit.Index != 1 || !bytes.Equal(commit.Encoded, want[0].Encoded) {
			t.Error("first frame differs")
		}
		return stopped
	}); !errors.Is(err, stopped) || consumed != 1 {
		t.Fatal(err, consumed)
	}
	consumed = 0
	if err := RecoverLocalCommitsEach("production-a", "inc_01", 0, journal, source, func(commit LocalCommit) error {
		if !bytes.Equal(commit.Encoded, want[consumed].Encoded) {
			t.Error("authenticated frame differs")
		}
		consumed++
		return nil
	}); err != nil || consumed != 2 {
		t.Fatal(err, consumed)
	}
	// Corruption in a later frame must fail the complete recovery. A consumer
	// may have borrowed a valid prefix, but cannot publish it before success.
	source.payloads[sourceKey(StoreLedger, 1, 1, 1)] = []byte("changed-source")
	consumed = 0
	if err := RecoverLocalCommitsEach("production-a", "inc_01", 0, journal, source, func(LocalCommit) error { consumed++; return nil }); err == nil || consumed != 1 {
		t.Fatal("corrupt suffix accepted", err, consumed)
	}
}
