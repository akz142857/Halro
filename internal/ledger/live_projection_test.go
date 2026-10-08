package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLiveProjectionFullAuthenticatedReferenceAcrossRoll(t *testing.T) {
	ctx := context.Background()
	log, err := OpenWithOptions(filepath.Join(t.TempDir(), "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	empty, err := log.BuildFreshProjection(ctx, filepath.Join(t.TempDir(), "empty-reference.db"))
	if err != nil {
		t.Fatal("first-run empty reference", err)
	}
	emptyHead, emptyHash, err := empty.ReadPosition(ctx)
	if err != nil || emptyHead != (Watermark{Generation: 1}) || emptyHash != ([32]byte{}) {
		t.Fatal(emptyHead, err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		for _, event := range projectionTestLifecycle(i+1, false) {
			if _, err := log.Append(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := log.Roll(); err != nil {
			t.Fatal(err)
		}
	}
	head, hash, _ := log.ChainHead()
	p, err := log.BuildFreshProjection(ctx, filepath.Join(t.TempDir(), "reference.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	actual, actualHash, err := p.ReadPosition(ctx)
	if err != nil || actual != head || actualHash != hash {
		t.Fatal(actual, head, err)
	}
	for _, event := range projectionTestLifecycle(3, false) {
		if _, err := log.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if actual, actualHash, err := p.ReadPosition(ctx); err != nil || actual != head || actualHash != hash {
		t.Fatal("private reference changed with live suffix", actual, err)
	}
}

func TestLiveProjectionRefusesCanceledClosedMemberAndTamperedSource(t *testing.T) {
	for _, fault := range []string{"cancel", "closed", "member", "tamper"} {
		t.Run(fault, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.wal")
			log, err := OpenWithOptions(path, NewStatus(), Options{ChainKey: testChainKey})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = log.Close() })
			for _, event := range projectionTestLifecycle(1, false) {
				if _, err := log.Append(context.Background(), event); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch fault {
			case "cancel":
				cancel()
			case "closed":
				if err := log.Close(); err != nil {
					t.Fatal(err)
				}
			case "member":
				log.options.AfterDurable = func(DurableBatch) (uint64, error) { return 0, nil }
			case "tamper":
				file, err := os.OpenFile(path, os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteAt([]byte{0xff}, 20); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}
			output := filepath.Join(t.TempDir(), "reference.db")
			p, err := log.BuildFreshProjection(ctx, output)
			if err == nil || p != nil {
				t.Fatal("invalid native reference published", p, err)
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refusal created derivative", err)
			}
			if fault == "tamper" && log.Status().Load() != AccountingRecoveryRequired {
				t.Fatal("corrupt source did not latch health")
			}
		})
	}
}
