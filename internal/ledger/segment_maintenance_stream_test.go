package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestWalkSegmentsRejectsLateIndexDamageAndJoinsClose(t *testing.T) {
	t.Run("damaged later slot", func(t *testing.T) {
		log, directory := sealedVerificationFixture(t)
		seen := 0
		err := log.WalkSegments(context.Background(), func(segment Segment) error {
			seen++
			if segment.Generation == 1 {
				index, err := os.OpenFile(filepath.Join(directory, segmentDirectoryIndexName), os.O_RDWR, 0)
				if err != nil {
					return err
				}
				var damage [1]byte
				if _, err := index.ReadAt(damage[:], segmentDirectoryHeaderBytes+segmentDirectorySlotBytes+16); err != nil {
					return errors.Join(err, index.Close())
				}
				damage[0] ^= 1
				_, err = index.WriteAt(damage[:], segmentDirectoryHeaderBytes+segmentDirectorySlotBytes+16)
				return errors.Join(err, index.Close())
			}
			return nil
		})
		if !errors.Is(err, ErrTampered) || seen != 1 {
			t.Fatalf("seen=%d err=%v", seen, err)
		}
	})
	t.Run("Close waits actual consumer exit", func(t *testing.T) {
		log, _ := sealedVerificationFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		sentinel := errors.New("consumer stopped")
		walkDone := make(chan error, 1)
		go func() {
			walkDone <- log.WalkSegments(context.Background(), func(Segment) error { close(entered); <-release; return sentinel })
		}()
		<-entered
		closeStarted, closeDone := make(chan struct{}), make(chan error, 1)
		go func() { close(closeStarted); closeDone <- log.Close() }()
		<-closeStarted
		select {
		case err := <-closeDone:
			t.Fatalf("Close returned before consumer exit: %v", err)
		case <-time.After(25 * time.Millisecond):
		}
		close(release)
		if err := <-walkDone; !errors.Is(err, sentinel) {
			t.Fatal(err)
		}
		if err := <-closeDone; err != nil {
			t.Fatal(err)
		}
		if err := log.WalkSegments(context.Background(), func(Segment) error { return nil }); err == nil {
			t.Fatal("closed Log allowed a walk")
		}
	})
}

type stagingProbeContext struct {
	context.Context
	probe func()
}

func (c stagingProbeContext) Err() error { c.probe(); return c.Context.Err() }

func TestStageSegmentOwnedCopyRejectsDamageAndPreservesLateReplacement(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed native bytes", true: "changed output inode"}[replace], func(t *testing.T) {
			log, nativeDirectory := sealedVerificationFixture(t)
			segment := segmentsForTest(t, log)[0]
			source := filepath.Join(nativeDirectory, segment.File)
			destination := filepath.Join(t.TempDir(), segment.File)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			changed := false
			ctx := stagingProbeContext{base, func() {
				if changed {
					return
				}
				if _, err := os.Stat(destination); errors.Is(err, os.ErrNotExist) {
					return
				} else if err != nil {
					t.Fatal(err)
				}
				changed = true
				if replace {
					if err := os.Rename(destination, destination+".original"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(destination, []byte("foreign staging inode"), 0600); err != nil {
						t.Fatal(err)
					}
					cancel()
				} else {
					file, err := os.OpenFile(source, os.O_RDWR, 0)
					if err != nil {
						t.Fatal(err)
					}
					var first [1]byte
					if _, err := file.ReadAt(first[:], 0); err != nil {
						t.Fatal(err)
					}
					first[0] ^= 1
					if _, err := file.WriteAt(first[:], 0); err != nil {
						t.Fatal(err)
					}
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}}
			err := copySealedSegmentContext(ctx, source, destination, segment)
			if !changed || err == nil {
				t.Fatal("late change accepted", changed, err)
			}
			if replace {
				if !errors.Is(err, ownedfile.ErrReplaced) || !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if raw, err := os.ReadFile(destination); err != nil || string(raw) != "foreign staging inode" {
					t.Fatal("foreign removed", err)
				}
				if _, err := os.Stat(destination + ".original"); err != nil {
					t.Fatal("original unknown inode removed", err)
				}
			} else {
				if !errors.Is(err, ErrTampered) {
					t.Fatal(err)
				}
				if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed copy retained definite derivative", err)
				}
			}
		})
	}
}

func TestStageSegmentsEachCancellationDoesNotPublishPartialDirectory(t *testing.T) {
	log, _ := sealedVerificationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	staging := t.TempDir()
	seen := 0
	err := log.StageSegmentsEach(ctx, staging, func(name string) error { seen++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || seen != 1 {
		t.Fatalf("seen=%d err=%v", seen, err)
	}
	if _, err := os.Stat(segmentManifestPath(staging)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial directory was published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staging, "ledger-2.wal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copy continued after cancellation: %v", err)
	}
	summary, err := log.VerifySealedSummary(0)
	if err != nil || summary.Generations != 3 {
		t.Fatalf("native history damaged: %+v %v", summary, err)
	}
}

func TestStageSegmentsEachRestoresCompleteHistoryWithoutNameCollection(t *testing.T) {
	log, _ := sealedVerificationFixture(t)
	if _, err := log.Compact(2); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	if _, err := log.Snapshot(filepath.Join(staging, "ledger.wal")); err != nil {
		t.Fatal(err)
	}
	seen := 0
	if err := log.StageSegmentsEach(context.Background(), staging, func(string) error { seen++; return nil }); err != nil {
		t.Fatal(err)
	}
	if seen != 4 {
		t.Fatalf("staged=%d want three generations and manifest", seen)
	}
	report, partial, err := InspectReplayAuthenticated(filepath.Join(staging, "ledger.wal"), testChainKey, nil)
	if err != nil || partial || report.Head.Sequence != 6 || report.SealedGenerations != 3 {
		t.Fatalf("report=%+v partial=%v err=%v", report, partial, err)
	}
}
