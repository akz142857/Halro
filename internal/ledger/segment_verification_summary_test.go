package ledger

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func sealedVerificationFixture(t *testing.T) (*Log, string) {
	t.Helper()
	directory := t.TempDir()
	log := openChained(t, filepath.Join(directory, "ledger.wal"), nil)
	t.Cleanup(func() { _ = log.Close() })
	for generation := 1; generation <= 3; generation++ {
		appendReservations(t, log, generation*10, generation)
		if _, err := log.Roll(); err != nil {
			t.Fatal(err)
		}
	}
	return log, directory
}

func TestSealedVerificationSummaryAuthenticatesPlainAndCompressedHistory(t *testing.T) {
	log, directory := sealedVerificationFixture(t)
	if _, err := log.Compact(1); err != nil {
		t.Fatal(err)
	}
	summary, err := log.VerifySealedSummary(0)
	if err != nil || summary != (SegmentVerification{Generations: 3, Authenticated: 6}) {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	suffix, err := VerifySegmentsSummary(directory, testChainKey, 2)
	if err != nil || suffix != (SegmentVerification{Generations: 2, Authenticated: 5}) {
		t.Fatalf("suffix=%+v err=%v", suffix, err)
	}
	// A final-generation failure must not expose the two earlier successes.
	file := filepath.Join(directory, "ledger-3.wal")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	summary, err = VerifySegmentsSummary(directory, testChainKey, 0)
	if !errors.Is(err, ErrCorrupt) || summary != (SegmentVerification{}) {
		t.Fatalf("damaged suffix returned summary=%+v err=%v", summary, err)
	}
}

func TestSealedVerificationRejectsBadTailBeforeObservingReports(t *testing.T) {
	_, directory := sealedVerificationFixture(t)
	path := segmentManifestPath(directory)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, []byte(" trailing")...), 0600); err != nil {
		t.Fatal(err)
	}
	seen := 0
	err = verifySegments(directory, testChainKey, 0, func(ChainReport) error { seen++; return nil })
	if err == nil || seen != 0 {
		t.Fatalf("bad manifest err=%v seen=%d", err, seen)
	}
	summary, err := VerifySegmentsSummary(directory, testChainKey, 0)
	if err == nil || summary != (SegmentVerification{}) {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
}

func TestSealedVerificationRejectsManifestReplacementAfterTentativeReport(t *testing.T) {
	_, directory := sealedVerificationFixture(t)
	original, err := loadSegmentManifest(directory)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	err = verifySegments(directory, testChainKey, 0, func(ChainReport) error {
		seen++
		if seen == 1 {
			return saveSegmentManifest(directory, original)
		}
		return nil
	})
	if !errors.Is(err, ErrCorrupt) || seen == 0 {
		t.Fatalf("replacement err=%v seen=%d", err, seen)
	}
}

func TestSealedVerificationSummaryRejectsClosedLog(t *testing.T) {
	log, _ := sealedVerificationFixture(t)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	summary, err := log.VerifySealedSummary(0)
	if err == nil || summary != (SegmentVerification{}) {
		t.Fatalf("closed summary=%+v err=%v", summary, err)
	}
}

func TestRepairReplicaRollStreamsHistoricalDirectoryBeforePublishing(t *testing.T) {
	for _, test := range []struct {
		name    string
		renamed bool
		damage  string
	}{
		{name: "before-rename"}, {name: "after-rename", renamed: true},
		{name: "missing-old-generation", renamed: true, damage: "missing"},
		{name: "damaged-old-generation", renamed: true, damage: "damage"},
		{name: "bad-directory-tail", renamed: true, damage: "tail"},
		{name: "occupied-successor", renamed: true, damage: "successor"},
	} {
		t.Run(test.name, func(t *testing.T) {
			log, directory := sealedVerificationFixture(t)
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			manifest, err := loadSegmentManifest(directory)
			if err != nil {
				t.Fatal(err)
			}
			pending := manifest.Segments[2]
			manifest.Segments, manifest.Pending = manifest.Segments[:2], &pending
			if err := saveSegmentManifest(directory, manifest); err != nil {
				t.Fatal(err)
			}
			active := filepath.Join(directory, "ledger.wal")
			if !test.renamed {
				if err := os.Rename(filepath.Join(directory, pending.File), active); err != nil {
					t.Fatal(err)
				}
			} else if test.damage != "successor" {
				if err := os.Remove(active); err != nil {
					t.Fatal(err)
				}
			}
			switch test.damage {
			case "missing":
				if err := os.Remove(filepath.Join(directory, "ledger-1.wal")); err != nil {
					t.Fatal(err)
				}
			case "damage":
				file := filepath.Join(directory, "ledger-1.wal")
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-1] ^= 1
				if err := os.WriteFile(file, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "tail":
				raw, err := os.ReadFile(segmentManifestPath(directory))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(segmentManifestPath(directory), append(raw, []byte(" trailing")...), 0600); err != nil {
					t.Fatal(err)
				}
			case "successor":
				if err := os.WriteFile(active, []byte("occupied"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(segmentManifestPath(directory))
			if err != nil {
				t.Fatal(err)
			}
			err = RepairReplicaRoll(active, testChainKey, 3, 6)
			if test.damage != "" {
				if err == nil {
					t.Fatal("damaged history authorized repair")
				}
				after, readErr := os.ReadFile(segmentManifestPath(directory))
				if readErr != nil || string(after) != string(before) {
					t.Fatalf("failed repair changed manifest: %v", readErr)
				}
				if test.damage == "damage" || test.damage == "missing" {
					info, statErr := os.Stat(active)
					if statErr != nil || info.Size() != 0 {
						t.Fatalf("recoverable successor lost: %v", statErr)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			summary, err := VerifySegmentsSummary(directory, testChainKey, 0)
			want := SegmentVerification{Generations: 3, Authenticated: 6}
			if !test.renamed {
				want = SegmentVerification{Generations: 2, Authenticated: 3}
			}
			if err != nil || summary != want {
				t.Fatalf("summary=%+v want=%+v err=%v", summary, want, err)
			}
			replica, err := OpenWithOptions(active, NewStatus(), Options{ChainKey: testChainKey, Replica: true})
			if err != nil {
				t.Fatal(err)
			}
			defer replica.Close()
			head, partial, err := InspectReplayAuthenticated(active, testChainKey, nil)
			if err != nil || partial || head.Head.Sequence != 6 {
				t.Fatalf("head=%+v partial=%v err=%v", head, partial, err)
			}
		})
	}
}
