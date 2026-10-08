package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestAuthenticatedLedgerBuildRetainsSourceCreditOnActualReaderCloseFailure(t *testing.T) {
	path, expected, _ := authenticatedLedgerNativeFixture(t)
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "known-close", true: "actual-close-failure"}[uncertain], func(t *testing.T) {
			o := authenticatedLedgerTestOptions(t)
			before := o.Budget.Snapshot()
			output := filepath.Join(t.TempDir(), "root")
			var actual *os.File
			p, err := buildAuthenticatedLedgerProjection(context.Background(), path, testChainKey, output, expected, o, func(file *os.File) error {
				actual = file
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				if uncertain {
					return file.Close()
				}
				return nil
			})
			if actual == nil {
				t.Fatal("actual active native reader was not exercised")
			}
			if !uncertain {
				if err != nil || p == nil {
					t.Fatal(p, err)
				}
				if err := p.Discard(); err != nil {
					t.Fatal(err)
				}
				if after := o.Budget.Snapshot(); after.Used != before.Used {
					t.Fatal("known Close did not release fees", before, after)
				}
				return
			}
			var debt *authenticatedSourceDebt
			if p != nil || !errors.Is(err, ownedfile.ErrRetirement) || !errors.Is(err, os.ErrClosed) || !errors.As(err, &debt) {
				t.Fatal("actual Close uncertainty lost", p, err)
			}
			if after := o.Budget.Snapshot(); after.Used.RAM-before.Used.RAM < AuthenticatedLedgerSourceRAMBytes || after.Used.Disk != before.Used.Disk || after.NormalClaims-before.NormalClaims != 1 {
				t.Fatal("unpublished root must retire but actual reader job must remain charged", before, after)
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed unpublished root was retained", err)
			}
		})
	}
}
