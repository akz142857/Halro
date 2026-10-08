package ledger

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestLedgerAuthenticatedGenerationReplayVisitsEachAllowedFrameOnceAcrossManyRolls(t *testing.T) {
	const count = 24
	ctx := context.Background()
	log := openChained(t, filepath.Join(t.TempDir(), "ledger.wal"), nil)
	defer log.Close()
	var sealed []Segment // bounded fixture data, never used by production replay
	for i := 1; i <= count; i++ {
		if _, err := log.Append(ctx, validReservation(fmt.Sprintf("evt_many_%d", i), fmt.Sprintf("att_many_%d", i))); err != nil {
			t.Fatal(err)
		}
		if i < count {
			roll, err := log.Roll()
			if err != nil {
				t.Fatal(err)
			}
			sealed = append(sealed, roll.Sealed)
			if i%3 == 0 {
				if _, err := log.Compact(uint64(i)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	p := projectionTestStore(t)
	seen := map[uint64]int{}
	for gen := uint64(1); gen <= count; gen++ {
		anchor, err := p.ReadReplayAnchor(ctx)
		if err != nil {
			t.Fatal(err)
		}
		head, err := log.ReplayAuthenticatedGeneration(ctx, anchor, gen, func(r Record) error {
			if r.Generation != gen {
				t.Fatalf("generation %d inspected later/older file %d", gen, r.Generation)
			}
			seen[r.Sequence]++
			return p.Apply(ctx, r)
		})
		if err != nil || head.Generation != gen || head.Sequence != gen {
			t.Fatal(gen, head, err)
		}
		if gen < count {
			if err := log.WithVerifiedProjectionRoll(ctx, sealed[gen-1], func(r VerifiedProjectionRoll) error { return p.ApplyVerifiedRoll(ctx, r) }); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(seen) != count {
		t.Fatal(seen)
	}
	for _, n := range seen {
		if n != 1 {
			t.Fatal("future suffix repeatedly inspected", seen)
		}
	}
	anchor, err := p.ReadReplayAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	remaining := 0
	head, err := log.ReplayAuthenticated(ctx, anchor, func(Record) error { remaining++; return nil })
	nativeHead, nativeHash, _ := log.ChainHead()
	if err != nil || remaining != 0 || head != nativeHead || anchor.Hash != nativeHash {
		t.Fatal(head, remaining, anchor, err)
	}
}

func TestLedgerAuthenticateAppliedAnchorMatchesRecordAndRollButDoesNotGuess(t *testing.T) {
	path, _, records := prefixBuildFixture(t)
	ctx := context.Background()
	log := openChained(t, path, nil)
	defer log.Close()
	for _, r := range []Record{records[0], records[64], records[149]} {
		head := Watermark{r.Generation, r.Offset, r.Sequence}
		anchor, err := log.AuthenticateAppliedAnchor(ctx, head)
		if err != nil || anchor != (ReplayAnchor{Head: head, Hash: r.Hash, Epoch: r.Epoch}) {
			t.Fatal(anchor, r, err)
		}
	}
	r := records[29]
	head := Watermark{Generation: 2, Sequence: r.Sequence}
	if anchor, err := log.AuthenticateAppliedAnchor(ctx, head); err != nil || anchor != (ReplayAnchor{Head: head, Hash: r.Hash, Epoch: r.Epoch}) {
		t.Fatal(anchor, err)
	}
	bad := Watermark{Generation: 2, Sequence: r.Sequence + 1}
	if _, err := log.AuthenticateAppliedAnchor(ctx, bad); err == nil {
		t.Fatal("wrong roll sequence guessed")
	}
	r = records[64]
	bad = Watermark{r.Generation, r.Offset - 1, r.Sequence}
	if _, err := log.AuthenticateAppliedAnchor(ctx, bad); err == nil {
		t.Fatal("mid-frame position guessed")
	}
	if log.Status().Load() != AccountingHealthy {
		t.Fatal("caller position failure poisoned native", log.Status().Load())
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := log.AuthenticateAppliedAnchor(ctx, Watermark{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestLedgerAuthenticatedGenerationRejectsWrongIntermediateEpochDeclaration(t *testing.T) {
	for _, declared := range []uint8{0, frameVersionLedgerIntegrity} {
		for _, bounded := range []bool{false, true} {
			t.Run(fmt.Sprintf("declared-%d/bounded-%t", declared, bounded), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "ledger.wal")
				log := openChained(t, path, nil)
				ctx := context.Background()
				if _, err := log.Append(ctx, validReservation("evt_epoch_1", "att_epoch_1")); err != nil {
					t.Fatal(err)
				}
				roll, err := log.Roll()
				if err != nil || roll.Sealed.EndEpoch != frameVersionRunAttribution {
					t.Fatal(roll, err)
				}
				if _, err := log.Append(ctx, validReservation("evt_epoch_2", "att_epoch_2")); err != nil {
					t.Fatal(err)
				}
				if err := log.Close(); err != nil {
					t.Fatal(err)
				}
				roll.Sealed.EndEpoch = declared
				if err := saveSegmentManifest(filepath.Dir(path), segmentManifest{Segments: []Segment{roll.Sealed}}); err != nil {
					t.Fatal(err)
				}
				if cut, report, err := InspectProjectionCursor(ctx, path, testChainKey, 1, 1); !errors.Is(err, ErrCorrupt) || cut != (ProjectionPrefix{}) || report != (ChainReport{}) {
					t.Fatal("fresh cut lookup accepted wrong epoch declaration", cut, report, err)
				}
				log = openChained(t, path, nil)
				defer log.Close()
				var replayErr error
				if bounded {
					_, replayErr = log.ReplayAuthenticatedGeneration(ctx, ReplayAnchor{}, 1, nil)
				} else {
					_, replayErr = log.ReplayAuthenticated(ctx, ReplayAnchor{}, nil)
				}
				if !errors.Is(replayErr, ErrCorrupt) || log.Status().Load() != AccountingRecoveryRequired {
					t.Fatal("intermediate epoch omitted/misstated", replayErr, log.Status().Load())
				}
				if _, err := log.ReplayAuthenticated(ctx, ReplayAnchor{}, nil); err == nil {
					t.Fatal("failed generation reused")
				}
			})
		}
	}
}
