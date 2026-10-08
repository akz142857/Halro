package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCompactedRetryAuthenticatesBothActualFilesBeforeRetirement(t *testing.T) {
	for _, mode := range []string{"complete", "plain-tamper", "packed-tamper", "plain-symlink", "plain-close", "packed-close", "late-packed", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "ledger.wal")
			log := openChained(t, path, nil)
			appendReservations(t, log, 1, 3)
			if _, err := log.Roll(); err != nil {
				t.Fatal(err)
			}
			log.maintenancePhase = func(at string) error {
				if at == "compact-manifest-published" {
					return errors.New("stop after actual compressed commit")
				}
				return nil
			}
			if _, err := log.Compact(1); err == nil {
				t.Fatal("boundary failure was ignored")
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			log = openChained(t, path, nil)
			defer log.Close()
			selected, found, err := log.lookupSegmentLocked(1)
			if err != nil || !found || !selected.Compressed {
				t.Fatal("committed compressed fact lost", selected, err)
			}
			plain := filepath.Join(directory, "ledger-1.wal")
			packed := filepath.Join(directory, selected.File)
			switch mode {
			case "plain-tamper", "packed-tamper":
				name := plain
				if mode == "packed-tamper" {
					name = packed
				}
				raw, err := os.ReadFile(name)
				if err != nil || len(raw) < 8 {
					t.Fatal(err)
				}
				raw[len(raw)-1] ^= 1
				if err := os.WriteFile(name, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "plain-symlink":
				if err := os.Rename(plain, plain+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(plain+".original", plain); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "plain-close" || mode == "packed-close" || mode == "late-packed" || mode == "cancel" {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				p, c, d, err := qualifyCompactedDuplicateWithPhase(ctx, directory, selected, testChainKey, func(source, compressed *os.File) error {
					switch mode {
					case "plain-close":
						return source.Close()
					case "packed-close":
						return compressed.Close()
					case "late-packed":
						if err := os.Rename(packed, packed+".original"); err != nil {
							return err
						}
						return os.WriteFile(packed, []byte("foreign"), 0600)
					default:
						cancel()
						return nil
					}
				})
				if err == nil || p != nil || c != nil || d != nil {
					t.Fatal("failed qualification leaked retirement authority", p, c, d, err)
				}
				if (mode == "plain-close" || mode == "packed-close") && !errors.Is(err, ErrSegmentRetirement) {
					t.Fatal("unknown actual Close lost", err)
				}
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatal("late cancellation ignored", err)
				}
				if _, err := os.Lstat(plain); err != nil {
					t.Fatal("unqualified duplicate retired", err)
				}
				return
			}
			result, err := log.Compact(1)
			if mode != "complete" {
				if err == nil || result != (Segment{}) {
					t.Fatal("unqualified duplicate accepted", result, err)
				}
				if _, err := os.Lstat(plain); err != nil {
					t.Fatal("unqualified plain representation removed", err)
				}
				return
			}
			if err != nil || result != selected {
				t.Fatal("qualified retry changed selected manifest", result, err)
			}
			if _, err := os.Lstat(plain); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("qualified duplicate was not retired", err)
			}
			if _, err := log.Compact(1); err != nil {
				t.Fatal("retirement retry is not idempotent", err)
			}
			ids, head := replayedEventIDs(t, log, Watermark{})
			if head.Sequence != 3 || len(ids) != 3 {
				t.Fatal("retirement changed native facts", ids, head)
			}
		})
	}
}
