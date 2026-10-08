package ledger

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func stagingDiskLogFixture(t *testing.T) (*Log, string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.wal")
	l, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := l.Append(context.Background(), validReservation("evt_staging_disk", "att_staging_disk")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return l, path, raw
}

func TestNativeSnapshotDiskAdmissionAndExactSuccessfulSource(t *testing.T) {
	l, source, raw := stagingDiskLogFixture(t)
	path := filepath.Join(t.TempDir(), "snapshot.wal")
	reject := errors.New("whole staging quota exhausted")
	if head, err := l.SnapshotWithDisk(path, func(int64) (NativeFileDiskReservation, error) { return nil, reject }); !errors.Is(err, reject) || head != (Watermark{}) {
		t.Fatal("unfunded snapshot accepted", head, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("unfunded snapshot created source", err)
	}
	token := &nativeDiskTestToken{}
	head, err := l.SnapshotWithDisk(path, func(bytes int64) (NativeFileDiskReservation, error) {
		if bytes < int64(len(raw)) {
			t.Fatal("short staging envelope")
		}
		return token, nil
	})
	if err != nil || head.Sequence != 1 || token.retired != 0 {
		t.Fatal("successful snapshot fee lost", head, err, token.retired)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, raw) {
		t.Fatal("snapshot differs from actual native prefix", err)
	}
	if current, err := os.ReadFile(source); err != nil || !bytes.Equal(current, raw) {
		t.Fatal("snapshot changed native authority", err)
	}
	if _, _, err := ReplicationCursor(path, testChainKey); err != nil {
		t.Fatal("actual staged frame failed MAC", err)
	}
}

func TestNativeSnapshotKnownCopyFailureRetiresOnlyItsCreationReceipt(t *testing.T) {
	l, source, _ := stagingDiskLogFixture(t)
	path := filepath.Join(t.TempDir(), "snapshot.wal")
	token := &nativeDiskTestToken{onRetire: func() error {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("refund before actual output retirement", err)
		}
		return nil
	}}
	_, err := l.SnapshotWithDisk(path, func(int64) (NativeFileDiskReservation, error) {
		if err := os.Truncate(source, 0); err != nil {
			t.Fatal(err)
		}
		return token, nil
	})
	if err == nil || token.retired != 1 {
		t.Fatal("known snapshot copy/close retirement lost refund", err, token.retired)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed created snapshot survives", err)
	}
}

func TestNativeStagingDiskFundsPayloadAndManifestWithoutBorrowingNativeQuota(t *testing.T) {
	l, _, _ := stagingDiskLogFixture(t)
	if result, err := l.Roll(); err != nil || !result.Rolled {
		t.Fatal(result, err)
	}
	for _, stage := range []string{"payload-refusal", "manifest-refusal", "consumer-failure", "success"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			calls, consumed := 0, 0
			token := &nativeDiskTestToken{}
			reject := errors.New("fixed staging quota exhausted")
			consumeErr := errors.New("caller stopped after actual published payload")
			reserve := func(int64) (NativeFileDiskReservation, error) {
				calls++
				if stage == "payload-refusal" || stage == "manifest-refusal" && calls == 2 {
					return nil, reject
				}
				return token, nil
			}
			err := l.StageSegmentsEachWithDisk(context.Background(), dir, reserve, func(string) error {
				consumed++
				if stage == "consumer-failure" {
					return consumeErr
				}
				return nil
			})
			if stage == "success" {
				if err != nil || calls != 2 || consumed != 2 {
					t.Fatal("staging did not fund both outputs", err, calls, consumed)
				}
				if _, err := VerifySegmentsSummary(dir, testChainKey, 0); err != nil {
					t.Fatal("actual staged manifest/payload failed authentication", err)
				}
			} else if stage == "consumer-failure" {
				if !errors.Is(err, consumeErr) || consumed != 1 || calls != 1 {
					t.Fatal(err, consumed, calls)
				}
			} else if !errors.Is(err, reject) {
				t.Fatal("unfunded staging accepted", err)
			}
			if token.retired != 0 {
				t.Fatal("published output/consumer failure refunded staging fee")
			}
			if stage == "payload-refusal" {
				names, err := os.ReadDir(dir)
				if err != nil || len(names) != 0 {
					t.Fatal("unfunded payload created temporary", names, err)
				}
			}
			if stage == "manifest-refusal" {
				if _, err := os.Stat(filepath.Join(dir, "segments.json")); !os.IsNotExist(err) {
					t.Fatal("unfunded staged manifest published", err)
				}
			}
		})
	}
}

func TestNativeStagingKnownChecksumFailureClosesThenRetiresItsOnlyOutput(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "sealed.wal")
	destination := filepath.Join(t.TempDir(), "staged.wal")
	bytes := []byte("actual sealed representation with an invalid authenticated descriptor checksum")
	if err := os.WriteFile(source, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	token := &nativeDiskTestToken{onRetire: func() error {
		if _, err := os.Stat(destination); !os.IsNotExist(err) {
			t.Fatal("refund before captured inode unlink", err)
		}
		return nil
	}}
	err := copySealedSegmentWithDisk(context.Background(), source, destination, Segment{StoredLength: int64(len(bytes)), StoredChecksum: "wrong-checksum"}, func(int64) (NativeFileDiskReservation, error) { return token, nil })
	if !errors.Is(err, ErrTampered) || token.retired != 1 {
		t.Fatal("failed physical staging copy refunded before known retirement or leaked quota", err, token.retired)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("failed derivative survived actual retirement", err)
	}
	if actual, err := os.ReadFile(source); err != nil || string(actual) != string(bytes) {
		t.Fatal("cleanup removed native source", err)
	}
}
