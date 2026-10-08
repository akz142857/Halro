package ledger

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

func TestLedgerAuthenticatedResumePreservesActualHashAcrossCompressedAndRolledHistory(t *testing.T) {
	path, report, records := prefixBuildFixture(t)
	for _, i := range []int{0, 29, 64, 79, 149} {
		r := records[i]
		p, err := BuildLedgerProjectionPrefix(context.Background(), path, testChainKey, filepath.Join(t.TempDir(), "projection.db"), report, ProjectionPrefix{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash})
		if err != nil {
			t.Fatal(err)
		}
		log := openChained(t, path, nil)
		anchor, err := p.ReadReplayAnchor(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		head, err := log.ReplayAuthenticated(context.Background(), anchor, func(next Record) error {
			if next.Sequence <= r.Sequence || next.Hash == ([32]byte{}) {
				t.Fatalf("resume emitted untrusted record: %v", next)
			}
			count++
			return p.Apply(context.Background(), next)
		})
		if err != nil || head != report.Head || count != len(records)-i-1 {
			t.Fatal(head, count, err)
		}
		applied, hash, err := p.ReadPosition(context.Background())
		// A final empty roll has no event to advance the semantic owner generation.
		if err != nil || applied.Sequence != report.Head.Sequence || hash != report.ChainHash {
			t.Fatal(applied, hash, err)
		}
		_ = p.Close()
		_ = log.Close()
	}
}

func tamperResumeFrame(t *testing.T, path string, offset int64) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frame := raw[int(offset):]
	n := int(binary.BigEndian.Uint32(frame[16:20]))
	payload := frame[chainHeaderSize : chainHeaderSize+n]
	payload[n/2] ^= 0x20
	crc := crc32.NewIEEE()
	_, _ = crc.Write(frame[4:20])
	_, _ = crc.Write(payload)
	binary.BigEndian.PutUint32(frame[20:24], crc.Sum32())
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerAuthenticatedResumeRejectsCRCRepairedActiveAndSealedFrames(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "sealed"}[sealed], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.wal")
			log := openChained(t, path, nil)
			for i := 1; i <= 2; i++ {
				if _, err := log.Append(context.Background(), validReservation("evt_"+string(rune('0'+i)), "att_"+string(rune('0'+i)))); err != nil {
					t.Fatal(err)
				}
			}
			var first Record
			report, partial, err := InspectReplayAuthenticated(path, testChainKey, func(r Record) error {
				if r.Sequence == 1 {
					first = r
				}
				return nil
			})
			if err != nil || partial {
				t.Fatal(partial, err)
			}
			p, err := BuildLedgerProjectionPrefix(context.Background(), path, testChainKey, filepath.Join(t.TempDir(), "projection.db"), report, ProjectionPrefix{Head: Watermark{first.Generation, first.Offset, first.Sequence}, Hash: first.Hash})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			defer log.Close()
			changed := path
			if sealed {
				segment, err := log.Roll()
				if err != nil {
					t.Fatal(err)
				}
				changed = filepath.Join(filepath.Dir(path), segment.Sealed.File)
			}
			tamperResumeFrame(t, changed, first.Offset)
			anchor, err := p.ReadReplayAnchor(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			if _, err := log.ReplayAuthenticated(context.Background(), anchor, func(Record) error { calls++; return nil }); !errors.Is(err, ErrTampered) || calls != 0 {
				t.Fatal(calls, err)
			}
			if log.Status().Load() != AccountingRecoveryRequired {
				t.Fatal(log.Status().Load())
			}
			if _, err := log.ReplayAuthenticated(context.Background(), anchor, nil); err == nil {
				t.Fatal("integrity failure reopened by retry")
			}
		})
	}
}

func TestLedgerAuthenticatedResumeCallerFailureIsRetryableAndDoesNotPoisonNative(t *testing.T) {
	path, report, records := prefixBuildFixture(t)
	r := records[64]
	log := openChained(t, path, nil)
	defer log.Close()
	anchor := ReplayAnchor{Head: Watermark{r.Generation, r.Offset, r.Sequence}, Hash: r.Hash, Epoch: r.Epoch}
	injected := errors.New("projection rejected")
	if _, err := log.ReplayAuthenticated(context.Background(), anchor, func(Record) error { return injected }); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if log.Status().Load() != AccountingHealthy {
		t.Fatal(log.Status().Load())
	}
	if head, err := log.ReplayAuthenticated(context.Background(), anchor, nil); err != nil || head != report.Head {
		t.Fatal(head, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := log.ReplayAuthenticated(ctx, anchor, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
