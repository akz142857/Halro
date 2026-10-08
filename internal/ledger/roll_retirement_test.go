package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRollRefusesForeignNativeFilesAndKeepsActualCloseDebt(t *testing.T) {
	for _, mode := range []string{"source", "destination", "successor", "previous-close"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "ledger.wal")
			log := openChained(t, path, nil)
			defer log.Close()
			appendReservations(t, log, 1, 2)
			previous := log.file
			foreign := ""
			log.maintenancePhase = func(at string) error {
				switch {
				case mode == "source" && at == "roll-native-synced":
					if err := os.Rename(path, path+".original"); err != nil {
						return err
					}
					foreign = path
				case mode == "destination" && at == "roll-before-rename":
					foreign = filepath.Join(directory, "ledger-1.wal")
				case mode == "successor" && at == "roll-successor-created":
					if err := os.Rename(path, path+".original-successor"); err != nil {
						return err
					}
					foreign = path
				case mode == "previous-close" && at == "roll-before-previous-close":
					return previous.Close()
				default:
					return nil
				}
				return os.WriteFile(foreign, []byte("foreign"), 0600)
			}
			result, err := log.Roll()
			if err == nil || result != (SealResult{}) || log.Status().Load() == AccountingHealthy {
				t.Fatal("uncertain Roll returned success or stayed healthy", result, err)
			}
			if foreign != "" {
				raw, err := os.ReadFile(foreign)
				if err != nil || string(raw) != "foreign" {
					t.Fatal("Roll overwrote foreign native bytes", err)
				}
			}
			if _, err := log.Append(context.Background(), validReservation("after", "after")); err == nil {
				t.Fatal("failed native publication allowed business append")
			}
			if mode == "previous-close" {
				if !errors.Is(err, ErrSegmentRetirement) {
					t.Fatal("actual Close error was ignored", err)
				}
				if err := log.Close(); !errors.Is(err, ErrSegmentRetirement) {
					t.Fatal("closing successor erased old physical debt", err)
				}
				if err := log.Close(); !errors.Is(err, ErrSegmentRetirement) {
					t.Fatal("repeated Close erased old physical debt", err)
				}
			}
		})
	}
}
