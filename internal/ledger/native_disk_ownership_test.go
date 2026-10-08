package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/durable"
)

type nativeDiskTestToken struct {
	retired  int
	onRetire func() error
}

func (t *nativeDiskTestToken) RetireKnown() error {
	if t.onRetire != nil {
		if err := t.onRetire(); err != nil {
			return err
		}
	}
	t.retired++
	return nil
}

func TestNativeManifestDiskAdmissionPrecedesCreationAndReplacement(t *testing.T) {
	dir := t.TempDir()
	path := segmentManifestPath(dir)
	original := []byte("original authoritative manifest")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	reject := errors.New("shared native growth quota exhausted")
	renamed, err := saveSegmentManifestSourceWithDisk(dir, segmentManifestSource{nilSegments: true}, durable.SyncDirectory, func(bytes int64) (NativeFileDiskReservation, error) {
		if bytes != 64<<10 {
			t.Fatal("unexpected bounded manifest envelope", bytes)
		}
		return nil, reject
	})
	after, readErr := os.ReadFile(path)
	if renamed || !errors.Is(err, reject) || readErr != nil || !bytes.Equal(original, after) {
		t.Fatal("rejection changed native manifest", renamed, err, readErr)
	}
	if _, err := os.Stat(path + segmentTempSuffix); !os.IsNotExist(err) {
		t.Fatal("rejection created temporary", err)
	}
}

func TestNativeManifestOnlyKnownRetirementRefundsTemporaryDisk(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "known-retired", true: "directory-sync-uncertain"}[uncertain], func(t *testing.T) {
			dir := t.TempDir()
			injected := errors.New("real preparation stopped")
			calls := 0
			source := segmentManifestSource{each: func(func(Segment) error) error {
				calls++
				if calls == 2 {
					return injected
				}
				return nil
			}}
			token := &nativeDiskTestToken{onRetire: func() error {
				if _, err := os.Stat(segmentManifestPath(dir) + segmentTempSuffix); !os.IsNotExist(err) {
					t.Fatal("refund before created inode retired", err)
				}
				return nil
			}}
			syncErr := errors.New("directory durability unknown")
			syncDir := func(path string) error {
				if uncertain {
					return syncErr
				}
				return durable.SyncDirectory(path)
			}
			renamed, err := saveSegmentManifestSourceWithDisk(dir, source, syncDir, func(int64) (NativeFileDiskReservation, error) { return token, nil })
			if renamed || !errors.Is(err, injected) {
				t.Fatal(renamed, err)
			}
			if uncertain {
				var debt *nativeFileDiskDebt
				if token.retired != 0 || !errors.Is(err, syncErr) || !errors.As(err, &debt) || debt.reservation != token {
					t.Fatal("uncertain directory barrier lost actual token", token.retired, err)
				}
			} else if token.retired != 1 {
				t.Fatal("known created temporary was not refunded", token.retired)
			}
		})
	}
}

func TestNativeManifestSuccessAndDeletedOpenOldInodeKeepAllFees(t *testing.T) {
	dir := t.TempDir()
	path := segmentManifestPath(dir)
	first := &nativeDiskTestToken{}
	if renamed, err := saveSegmentManifestSourceWithDisk(dir, segmentManifestSource{nilSegments: true}, durable.SyncDirectory, func(int64) (NativeFileDiskReservation, error) { return first, nil }); err != nil || !renamed {
		t.Fatal(renamed, err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second := &nativeDiskTestToken{}
	if renamed, err := saveSegmentManifestSourceWithDisk(dir, segmentManifestSource{each: func(func(Segment) error) error { return nil }}, durable.SyncDirectory, func(int64) (NativeFileDiskReservation, error) { return second, nil }); err != nil || !renamed {
		t.Fatal(renamed, err)
	}
	actual := make([]byte, len(before))
	if _, err := old.ReadAt(actual, 0); err != nil || !bytes.Equal(actual, before) {
		t.Fatal("actual unlinked old descriptor no longer readable", err)
	}
	if first.retired != 0 || second.retired != 0 {
		t.Fatal("namespace replacement refunded old-open/published fee")
	}
}

func TestNativeCompressionDiskAdmissionAndCreatedRetirement(t *testing.T) {
	data := bytes.Repeat([]byte("actual compressed ledger bytes"), 2048)
	digest := sha256.Sum256(data)
	checksum := hex.EncodeToString(digest[:])
	for _, outcome := range []string{"reject", "checksum-failure", "published"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "ledger-1.wal")
			destination := filepath.Join(dir, "ledger-1.seg.gz")
			if err := os.WriteFile(source, data, 0600); err != nil {
				t.Fatal(err)
			}
			token := &nativeDiskTestToken{}
			reject := errors.New("shared quota refusal")
			reserve := func(bytes int64) (NativeFileDiskReservation, error) {
				if bytes <= int64(len(data)) {
					t.Fatal("missing gzip expansion envelope")
				}
				if outcome == "reject" {
					return nil, reject
				}
				return token, nil
			}
			wantChecksum := checksum
			if outcome == "checksum-failure" {
				wantChecksum = hex.EncodeToString(make([]byte, 32))
			}
			artifact, err := compressSegmentFileOwnedWithDisk(context.Background(), source, destination, wantChecksum, int64(len(data)), reserve)
			if outcome == "reject" {
				if !errors.Is(err, reject) {
					t.Fatal(err)
				}
				if _, err := os.Stat(destination + segmentTempSuffix); !os.IsNotExist(err) {
					t.Fatal("rejection created temporary")
				}
			} else if outcome == "checksum-failure" {
				if !errors.Is(err, ErrCorrupt) || token.retired != 1 || artifact != nil {
					t.Fatal("known gzip retirement did not refund", err, token.retired)
				}
				if _, err := os.Stat(destination + segmentTempSuffix); !os.IsNotExist(err) {
					t.Fatal("retired temporary still named", err)
				}
			} else {
				if err != nil || artifact == nil || token.retired != 0 {
					t.Fatal("published gzip lost fee", err, token.retired)
				}
				if err := artifact.discard(); err != nil || token.retired != 1 {
					t.Fatal("actual created gzip discard did not refund", err, token.retired)
				}
			}
			if actual, err := os.ReadFile(source); err != nil || !bytes.Equal(actual, data) {
				t.Fatal("auxiliary admission changed original native plaintext", err)
			}
		})
	}
}

func pendingDiskRollFixture(t *testing.T, renamed bool) (string, []byte) {
	t.Helper()
	seedPath := filepath.Join(t.TempDir(), "seed", "ledger.wal")
	seed, err := OpenWithOptions(seedPath, NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Append(context.Background(), validReservation("evt_disk_repair", "att_disk_repair")); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	rolled, err := seed.Roll()
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.wal")
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	if err := saveSegmentManifest(dir, segmentManifest{Pending: &rolled.Sealed}); err != nil {
		t.Fatal(err)
	}
	if renamed {
		if err := os.Rename(path, filepath.Join(dir, rolled.Sealed.File)); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := os.ReadFile(segmentManifestPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	return path, manifest
}

func TestNativeStartupPendingRollManifestRequiresDiskBeforeRepair(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-rename", true: "after-rename"}[renamed], func(t *testing.T) {
			path, original := pendingDiskRollFixture(t, renamed)
			reject := errors.New("startup native auxiliary capacity exhausted")
			l, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, ReserveNativeFileDisk: func(int64) (NativeFileDiskReservation, error) { return nil, reject }})
			if l != nil || !errors.Is(err, reject) {
				if l != nil {
					l.Close()
				}
				t.Fatal("unfunded startup repaired pending manifest", err)
			}
			actual, err := os.ReadFile(segmentManifestPath(filepath.Dir(path)))
			if err != nil || !bytes.Equal(actual, original) {
				t.Fatal("startup quota refusal changed old authority", err)
			}
			var tokens []*nativeDiskTestToken
			l, err = OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, ReserveNativeFileDisk: func(int64) (NativeFileDiskReservation, error) {
				token := &nativeDiskTestToken{}
				tokens = append(tokens, token)
				return token, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			retired := 0
			for _, token := range tokens {
				retired += token.retired
			}
			if retired != 1 || len(tokens) < 3 {
				t.Fatal("only actual retired pending temporary index may refund", retired, len(tokens))
			}
			manifest, err := loadSegmentManifest(filepath.Dir(path))
			if err != nil || manifest.Pending != nil {
				t.Fatal("funded startup failed to resolve manifest", manifest, err)
			}
		})
	}
}

func TestNativeReplicaRollRepairFundsSuccessorAndResolvedManifest(t *testing.T) {
	for _, stage := range []string{"before-rename-manifest-refusal", "successor-refusal", "manifest-refusal-after-successor", "complete"} {
		t.Run(stage, func(t *testing.T) {
			renamed := stage != "before-rename-manifest-refusal"
			path, original := pendingDiskRollFixture(t, renamed)
			reject := errors.New("replica repair auxiliary quota exhausted")
			calls := 0
			tokens := []*nativeDiskTestToken{}
			reserve := func(bytes int64) (NativeFileDiskReservation, error) {
				calls++
				if bytes != 64<<10 {
					t.Fatal("unexpected successor/manifest envelope", bytes)
				}
				if stage == "successor-refusal" || stage == "before-rename-manifest-refusal" || stage == "manifest-refusal-after-successor" && calls == 2 {
					return nil, reject
				}
				token := &nativeDiskTestToken{}
				tokens = append(tokens, token)
				return token, nil
			}
			err := RepairReplicaRollWithDisk(path, testChainKey, 1, 1, reserve)
			if stage == "complete" {
				if err != nil || calls != 2 {
					t.Fatal("funded repair failed", err, calls)
				}
				manifest, err := loadSegmentManifest(filepath.Dir(path))
				if err != nil || manifest.Pending != nil || len(manifest.Segments) != 1 {
					t.Fatal(manifest, err)
				}
			} else {
				if !errors.Is(err, reject) {
					t.Fatal("unfunded repair accepted", err)
				}
				actual, err := os.ReadFile(segmentManifestPath(filepath.Dir(path)))
				if err != nil || !bytes.Equal(actual, original) {
					t.Fatal("repair quota failure changed old manifest", err)
				}
				if stage == "successor-refusal" {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatal("unfunded repair created successor", err)
					}
				}
				if stage == "manifest-refusal-after-successor" {
					info, err := os.Stat(path)
					if err != nil || info.Size() != 0 {
						t.Fatal("funded recoverable successor was removed", err)
					}
				}
			}
			for _, token := range tokens {
				if token.retired != 0 {
					t.Fatal("recoverable authority/manifest fee refunded")
				}
			}
		})
	}
}

func TestNativeMissingActiveCreationRequiresDiskBeforeIO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.wal")
	reject := errors.New("no native creation capacity")
	l, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, ReserveNativeFileDisk: func(bytes int64) (NativeFileDiskReservation, error) {
		if bytes != 64<<10 {
			t.Fatal("unexpected new authority envelope", bytes)
		}
		return nil, reject
	}})
	if l != nil || !errors.Is(err, reject) {
		t.Fatal("unfunded native Open created source", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("unfunded source exists", err)
	}
	token := &nativeDiskTestToken{}
	l, err = OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, ReserveNativeFileDisk: func(int64) (NativeFileDiskReservation, error) { return token, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if token.retired != 0 {
		t.Fatal("native Close refunded named authority")
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatal("funded empty native creation missing", err)
	}
}
