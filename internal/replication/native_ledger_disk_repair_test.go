package replication

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replicationrange"
)

type repairFileDiskToken struct{ retired int }

func (t *repairFileDiskToken) RetireKnown() error { t.retired++; return nil }

func TestNativeSourceReplicaRollRepairKeepsSharedDiskOwnerAndUnknownDebt(t *testing.T) {
	for _, stage := range []string{"first-refusal", "later-refusal", "success"} {
		t.Run(stage, func(t *testing.T) {
			oldSource, _, _, _ := nativeRangeFixture(t)
			options := oldSource.options
			options.LedgerKey = bytes.Clone(options.LedgerKey)
			options.AuditKey = bytes.Clone(options.AuditKey)
			options.GovernanceKey = bytes.Clone(options.GovernanceKey)
			options.MetadataKey = bytes.Clone(options.MetadataKey)
			if err := oldSource.Close(); err != nil {
				t.Fatal(err)
			}
			log, err := ledger.OpenWithOptions(options.LedgerPath, ledger.NewStatus(), ledger.Options{ChainKey: options.LedgerKey})
			if err != nil {
				t.Fatal(err)
			}
			rolled, err := log.Roll()
			if err != nil || !rolled.Rolled {
				t.Fatal(rolled, err)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(options.LedgerPath); err != nil {
				t.Fatal(err)
			}
			pending, err := json.Marshal(struct {
				Version  int              `json:"version"`
				Segments []ledger.Segment `json:"segments"`
				Pending  *ledger.Segment  `json:"pending"`
			}{Version: 1, Pending: &rolled.Sealed})
			if err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(filepath.Dir(options.LedgerPath), "segments.json")
			if err := os.WriteFile(manifest, pending, 0600); err != nil {
				t.Fatal(err)
			}
			reject := errors.New("shared native file quota refused")
			calls := 0
			token := &repairFileDiskToken{}
			options.ReserveLedgerFileDisk = func(int64) (ledger.NativeFileDiskReservation, error) {
				calls++
				if stage == "first-refusal" || stage == "later-refusal" && calls == 2 {
					return nil, reject
				}
				return token, nil
			}
			source, err := NewNativeSource(options)
			if err != nil {
				t.Fatal(err)
			}
			err = source.RepairReplicaLedgerRoll(StoreCursor{Generation: 1, Sequence: 2})
			if stage == "success" {
				if err != nil || calls != 2 || token.retired != 0 {
					t.Fatal("native source bypassed shared file provider", err, calls, token.retired)
				}
				cursor, err := source.Cursor(StoreLedger)
				if err != nil || cursor != (StoreCursor{Generation: 2, Sequence: 2}) {
					t.Fatal(cursor, err)
				}
				if err := source.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, reject) {
					t.Fatal("native source bypassed quota refusal", err)
				}
				actual, err := os.ReadFile(manifest)
				if err != nil || !bytes.Equal(actual, pending) {
					t.Fatal("native source refusal changed authority", err)
				}
				if stage == "later-refusal" {
					if source.retirement.Load() == nil || token.retired != 0 {
						t.Fatal("recoverable successor fee debt discarded")
					}
					if _, err := source.Cursor(StoreLedger); !errors.Is(err, replicationrange.ErrClosed) {
						t.Fatal("uncertain repair kept source authority open", err)
					}
					if err := source.Close(); !errors.Is(err, ledger.ErrSegmentRetirement) || !errors.Is(err, reject) {
						t.Fatal("source Close lost actual repair fee cause", err)
					}
				} else if err := source.Close(); err != nil {
					t.Fatal("definite first refusal poisoned source", err)
				}
			}
		})
	}
}
