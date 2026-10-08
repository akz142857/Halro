package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func orderingInspectionFixture(t *testing.T) (string, []byte, OrderingHeader, []OrderingRecord) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ordering.journal")
	key := bytes.Repeat([]byte{0x35}, 32)
	header := OrderingHeader{ClusterID: "production-a", Incarnation: "inc_01"}
	header.StoreCursors[StoreLedger-StoreLedger] = StoreCursor{Generation: 3, Sequence: 10}
	header.StoreHeads[StoreLedger-StoreLedger] = sha256.Sum256([]byte("seeded Ledger"))
	header.StoreCursors[StoreMetadata-StoreLedger] = StoreCursor{Generation: 7, Sequence: 2}
	header.StoreHeads[StoreMetadata-StoreLedger] = sha256.Sum256([]byte("seeded Metadata"))
	roll, err := (LedgerRollMetadata{Generation: 3, FirstSequence: 1, LastSequence: 12, Length: 1000, EndEpoch: 1}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	records := []OrderingRecord{
		testLeadershipRecord(1, 2, "leader"),
		{Kind: KindData, Store: StoreLedger, StoreGeneration: 3, StoreSequenceFirst: 11, StoreSequenceLast: 11},
		{Kind: KindData, Store: StoreLedger, StoreGeneration: 3, StoreSequenceFirst: 12, StoreSequenceLast: 12},
		{Kind: KindData, Store: StoreMetadata, StoreGeneration: 7, StoreSequenceFirst: 3, StoreSequenceLast: 3},
		{Kind: KindLedgerRoll, Metadata: roll},
		{Kind: KindData, Store: StoreLedger, StoreGeneration: 4, StoreSequenceFirst: 13, StoreSequenceLast: 13},
		{Kind: KindData, Store: StoreAudit, StoreGeneration: 1, StoreSequenceFirst: 1, StoreSequenceLast: 1},
	}
	j, err := OpenOrderingJournal(path, key, header, 0, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	for i, record := range records {
		record.Index, record.Term = uint64(i+1), 2
		record.FrameDigest = sha256.Sum256([]byte{byte(i + 1)})
		records[i], err = j.Append(record)
		if err != nil {
			t.Fatal(err)
		}
	}
	return path, key, header, records
}

func TestInspectOrderingJournalSeparatesAllowedCutFromFullAuthenticatedTail(t *testing.T) {
	path, key, header, records := orderingInspectionFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, through := range []uint64{0, 1, 3, 5, 6} {
		report, err := InspectOrderingJournal(context.Background(), path, key, header.ClusterID, header.Incarnation, 6, records[5].MAC, through)
		if err != nil {
			t.Fatal(through, err)
		}
		if report.Header != header || report.Allowed.Index != through || report.Head.Index != 7 || report.Head.MAC != records[6].MAC || report.Head.Stores[StoreAudit-StoreLedger].Sequence != 1 {
			t.Fatalf("cut %d: %+v", through, report)
		}
		if report.Durable.Index != 6 || report.Durable.Term != 2 || report.Durable.MAC != records[5].MAC {
			t.Fatal("member durable cut was replaced by physical tail", report)
		}
		wantLedger := StoreCursor{Generation: 3, Sequence: 10}
		if through >= 3 {
			wantLedger.Sequence = 12
		}
		if through >= 5 {
			wantLedger.Generation = 4
		}
		if through >= 6 {
			wantLedger.Sequence = 13
		}
		if report.Allowed.Stores[StoreLedger-StoreLedger] != wantLedger || report.Allowed.Stores[StoreAudit-StoreLedger] != (StoreCursor{}) {
			t.Fatalf("durable tail entered allowed cut %d: %+v", through, report)
		}
		if through == 0 && (report.Allowed.MAC != ([32]byte{}) || report.Allowed.Term != 0 || report.Allowed.Stores != header.StoreCursors) {
			t.Fatal("seeded index-zero baseline was lost", report)
		}
		if through > 0 && (report.Allowed.MAC != records[through-1].MAC || report.Allowed.Term != 2) {
			t.Fatal("wrong allowed MAC/term", report)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	now, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !os.SameFile(info, now) || info.Mode() != now.Mode() || !info.ModTime().Equal(now.ModTime()) {
		t.Fatal("read-only inspection changed journal")
	}
}

func TestInspectOrderingJournalRejectsUntrustedHistoryWithoutRepair(t *testing.T) {
	for _, scenario := range []string{"wrong-key", "wrong-cluster", "wrong-incarnation", "wrong-head", "zero-index-head", "missing-head", "beyond-member", "missing-suffix", "bad-header", "bad-excluded-tail", "partial-tail", "short-header"} {
		t.Run(scenario, func(t *testing.T) {
			path, key, header, records := orderingInspectionFixture(t)
			index, head, through := uint64(6), records[5].MAC, uint64(3)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "wrong-key":
				key[0] ^= 1
			case "wrong-cluster":
				header.ClusterID = "different"
			case "wrong-incarnation":
				header.Incarnation = "different"
			case "wrong-head":
				head[0] ^= 1
			case "zero-index-head":
				index, through = 0, 0
			case "missing-head":
				head = [32]byte{}
			case "beyond-member":
				through = 7
			case "missing-suffix":
				raw = raw[:len(raw)-2*orderingRecordBytes]
			case "bad-header":
				raw[4+orderingHeaderMACOffset] ^= 1
			case "bad-excluded-tail":
				raw[len(raw)-1] ^= 1
			case "partial-tail":
				raw = append(raw, 0x33, 0x44)
			case "short-header":
				raw = raw[:10]
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			report, err := InspectOrderingJournal(context.Background(), path, key, header.ClusterID, header.Incarnation, index, head, through)
			if err == nil || report != (OrderingInspection{}) {
				t.Fatal("untrusted scan returned a prefix", report, err)
			}
			if scenario == "partial-tail" && !strings.Contains(err.Error(), "partial tail") {
				t.Fatal(err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			now, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if !bytes.Equal(raw, after) || !os.SameFile(info, now) || !info.ModTime().Equal(now.ModTime()) {
				t.Fatal("failed inspection repaired or replaced journal")
			}
		})
	}
}

// Encode a valid MAC chain with an invalid semantic transition. Checking MACs
// alone must not turn a skipped range, wrong roll or invalid leadership into an
// allowed native cursor, including when the defect follows the requested cut.
func TestInspectOrderingJournalRejectsAuthenticatedSemanticDefects(t *testing.T) {
	for _, scenario := range []string{"index-gap", "term-regression", "data-new-term", "duplicate-leadership", "store-gap", "wrong-roll"} {
		t.Run(scenario, func(t *testing.T) {
			path, key, header, records := orderingInspectionFixture(t)
			switch scenario {
			case "index-gap":
				records[6].Index++
			case "term-regression":
				records[6].Term = 1
			case "data-new-term":
				records[6].Term = 3
			case "duplicate-leadership":
				records[6] = testLeadershipRecord(7, 2, "duplicate")
			case "store-gap":
				records[6].StoreSequenceFirst, records[6].StoreSequenceLast = 2, 2
			case "wrong-roll":
				roll, err := (LedgerRollMetadata{Generation: 3, FirstSequence: 1, LastSequence: 13, Length: 1000, EndEpoch: 1}).MarshalBinary()
				if err != nil {
					t.Fatal(err)
				}
				records[4].Metadata = roll
			}
			raw, err := header.MarshalBinary(key)
			if err != nil {
				t.Fatal(err)
			}
			var previous [32]byte
			for i, record := range records {
				record.PreviousMAC = previous
				encoded, mac, err := record.MarshalBinaryAndMAC(key, header.ClusterID, header.Incarnation)
				if err != nil {
					t.Fatal(err)
				}
				raw = append(raw, encoded...)
				records[i].MAC, previous = mac, mac
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if report, err := InspectOrderingJournal(context.Background(), path, key, header.ClusterID, header.Incarnation, 3, records[2].MAC, 2); err == nil || report != (OrderingInspection{}) {
				t.Fatal("semantic corruption beyond cut escaped", report, err)
			}
		})
	}
}

func TestInspectOrderingJournalCanceledScanDiscardsPrefix(t *testing.T) {
	path, key, header, records := orderingInspectionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controlled := &cursorCancelContext{Context: ctx, cancel: cancel}
	if report, err := InspectOrderingJournal(controlled, path, key, header.ClusterID, header.Incarnation, 6, records[5].MAC, 1); !errors.Is(err, context.Canceled) || report != (OrderingInspection{}) {
		t.Fatal("partial report escaped cancellation", report, err)
	}
	if report, err := InspectOrderingJournal(ctx, path, key, header.ClusterID, header.Incarnation, 6, records[5].MAC, 1); !errors.Is(err, context.Canceled) || report != (OrderingInspection{}) {
		t.Fatal("pre-canceled scan proceeded", report, err)
	}
}

type orderingInspectionMutationContext struct {
	context.Context
	checks int
	mutate func()
}

func (c *orderingInspectionMutationContext) Err() error {
	c.checks++
	if c.checks == 4 {
		c.mutate()
	}
	return c.Context.Err()
}

func TestInspectOrderingJournalRejectsConcurrentFileChange(t *testing.T) {
	for _, scenario := range []string{"append", "replace", "unlink", "same-size-write"} {
		t.Run(scenario, func(t *testing.T) {
			path, key, header, records := orderingInspectionFixture(t)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			controlled := &orderingInspectionMutationContext{Context: context.Background(), mutate: func() {
				var err error
				switch scenario {
				case "append":
					f, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
					if openErr != nil {
						t.Fatal(openErr)
					}
					_, err = f.Write([]byte{1})
					err = errors.Join(err, f.Close())
				case "replace":
					err = os.WriteFile(path+".next", raw, 0o600)
					if err == nil {
						err = os.Rename(path+".next", path)
					}
				case "unlink":
					err = os.Remove(path)
				case "same-size-write":
					raw[len(raw)-1] ^= 1
					err = os.WriteFile(path, raw, 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			if report, err := InspectOrderingJournal(controlled, path, key, header.ClusterID, header.Incarnation, 3, records[2].MAC, 1); err == nil || report != (OrderingInspection{}) {
				t.Fatal("changed file returned a prefix", report, err)
			}
		})
	}
}

func TestInspectOrderingJournalDoesNotCreateMissingOrFollowSymlink(t *testing.T) {
	path, key, header, records := orderingInspectionFixture(t)
	missing := filepath.Join(t.TempDir(), "absent", "ordering.journal")
	if report, err := InspectOrderingJournal(context.Background(), missing, key, header.ClusterID, header.Incarnation, 0, [32]byte{}, 0); !errors.Is(err, os.ErrNotExist) || report != (OrderingInspection{}) {
		t.Fatal(report, err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inspection created directory", err)
	}
	alias := path + ".alias"
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{alias, filepath.Dir(path)} {
		if report, err := InspectOrderingJournal(context.Background(), name, key, header.ClusterID, header.Incarnation, 6, records[5].MAC, 3); err == nil || report != (OrderingInspection{}) {
			t.Fatal("nonregular path accepted", report, err)
		}
	}
}

func TestInspectOrderingJournalHeaderOnlyPreservesSeededBaseline(t *testing.T) {
	path, key, header, _ := orderingInspectionFixture(t)
	raw, err := header.MarshalBinary(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := InspectOrderingJournal(context.Background(), path, key, header.ClusterID, header.Incarnation, 0, [32]byte{}, 0)
	if err != nil || report.Header != header || report.Allowed != report.Head || report.Allowed.Stores != header.StoreCursors || report.Head.Index != 0 {
		t.Fatal(report, err)
	}
}
