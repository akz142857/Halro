package governance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

type governanceAppendAdmissionFixture struct{ releases int }

func (a *governanceAppendAdmissionFixture) Release() error { a.releases++; return nil }

type governanceAdmissionFaultDurability struct {
	file *os.File
	mode string
}

func (d *governanceAdmissionFaultDurability) Write(value []byte) (int, error) {
	if d.mode == "write" {
		return 0, errors.New("injected native write failure")
	}
	return d.file.Write(value)
}
func (d *governanceAdmissionFaultDurability) Sync() error {
	if d.mode == "sync" {
		return errors.New("injected native sync failure")
	}
	return d.file.Sync()
}

func replicationHookEvent(id string) Event {
	return Event{
		EventID: id, ProjectID: "prj_1", WorkUnitID: "wku_1", DefinitionID: "odef_1", DefinitionVersion: 1,
		OutcomeID: "out_1", Value: "accepted", ReporterKeyID: "key_1", ObservedAt: time.Now().UTC(), IngestedAt: time.Now().UTC(),
		Revision: 1, IdempotencyKeyHash: shaLabel("one"), RequestFingerprint: shaLabel("two"),
	}
}

func TestGovernanceDurableHookCapturesOneFsyncedBatchAndPoisonsOnFailure(t *testing.T) {
	var captured DurableBatch
	log, err := OpenWithOptions(filepath.Join(t.TempDir(), "governance.log"), []byte("0123456789abcdef0123456789abcdef"), Options{
		AfterDurable: func(batch DurableBatch) error {
			captured = batch
			return errors.New("ordering disk full")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, err := log.AppendBatch(context.Background(), []Event{replicationHookEvent("gov_1"), replicationHookEvent("gov_2")}); err == nil || !strings.Contains(err.Error(), "ordering disk full") {
		t.Fatalf("append error=%v", err)
	}
	if captured.FirstSequence != 1 || captured.LastSequence != 2 || len(captured.Frames) == 0 {
		t.Fatalf("captured=%#v", captured)
	}
	if _, err := log.Append(context.Background(), replicationHookEvent("gov_3")); err == nil || !strings.Contains(err.Error(), "requires restart") {
		t.Fatalf("poison error=%v", err)
	}
}

func TestGovernanceAdmissionRunsBeforeNativeWriteAndTransfersToOrderingHook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "governance.log")
	reserved := &governanceAppendAdmissionFixture{}
	var received AppendAdmission
	log, err := OpenWithOptions(path, []byte("0123456789abcdef0123456789abcdef"), Options{
		BeforeDurable: func(batch DurableBatch) (AppendAdmission, error) {
			info, statErr := os.Stat(path)
			if statErr != nil || info.Size() != 0 || len(batch.Frames) == 0 {
				t.Fatalf("Governance admission did not precede native write: info=%v err=%v", info, statErr)
			}
			return reserved, nil
		},
		AfterDurable: func(batch DurableBatch) error { received = batch.Admission; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), replicationHookEvent("gov_admission")); err != nil {
		t.Fatal(err)
	}
	if received != reserved || reserved.releases != 0 {
		t.Fatal("Governance did not transfer the admission to ordering", received, reserved.releases)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if reserved.releases != 0 {
		t.Fatal("Governance source released an ordering-owned admission", reserved.releases)
	}
}

func TestGovernanceAdmissionRefusalLeavesNativeHeadAndFileUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "governance.log")
	log, err := OpenWithOptions(path, []byte("0123456789abcdef0123456789abcdef"), Options{
		BeforeDurable: func(DurableBatch) (AppendAdmission, error) { return nil, resourcebudget.ErrExhausted },
		AfterDurable:  func(DurableBatch) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	beforeHead := log.Summary()
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), replicationHookEvent("gov_admission_refused")); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatalf("admission refusal error=%v", err)
	}
	afterHead := log.Summary()
	afterInfo, err := os.Stat(path)
	if err != nil || beforeHead != afterHead || beforeInfo.Size() != afterInfo.Size() {
		t.Fatalf("refused append changed Governance source: before=(%+v,%d) after=(%+v,%v,%v)", beforeHead, beforeInfo.Size(), afterHead, afterInfo, err)
	}
}

func TestGovernanceUncertainNativeAppendRetainsAdmissionUntilClose(t *testing.T) {
	for _, mode := range []string{"write", "sync"} {
		t.Run(mode, func(t *testing.T) {
			reserved := &governanceAppendAdmissionFixture{}
			var afterCalls int
			log, err := OpenWithOptions(filepath.Join(t.TempDir(), "governance.log"), []byte("0123456789abcdef0123456789abcdef"), Options{
				BeforeDurable: func(DurableBatch) (AppendAdmission, error) { return reserved, nil },
				AfterDurable:  func(DurableBatch) error { afterCalls++; return nil },
				WrapDurability: func(file *os.File) DurabilityWriter {
					return &governanceAdmissionFaultDurability{file: file, mode: mode}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(context.Background(), replicationHookEvent("uncertain_"+mode)); err == nil {
				t.Fatal("injected uncertain append unexpectedly succeeded")
			}
			if afterCalls != 0 || reserved.releases != 0 {
				t.Fatalf("uncertain append lost the source-owned admission: after=%d releases=%d", afterCalls, reserved.releases)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			if reserved.releases != 1 {
				t.Fatal("closing uncertain source did not retire its in-process admission", reserved.releases)
			}
		})
	}
}
