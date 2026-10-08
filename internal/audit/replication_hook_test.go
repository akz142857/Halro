package audit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

type auditAppendAdmissionFixture struct{ releases int }

func (a *auditAppendAdmissionFixture) Release() error { a.releases++; return nil }

type auditAdmissionFaultDurability struct {
	file *os.File
	mode string
}

func (d *auditAdmissionFaultDurability) Write(value []byte) (int, error) {
	if d.mode == "write" {
		return 0, errors.New("injected native write failure")
	}
	return d.file.Write(value)
}
func (d *auditAdmissionFaultDurability) Sync() error {
	if d.mode == "sync" {
		return errors.New("injected native sync failure")
	}
	return d.file.Sync()
}

func TestAuditAdmissionRunsBeforeNativeWriteAndTransfersToOrderingHook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	reserved := &auditAppendAdmissionFixture{}
	var received AppendAdmission
	log, err := OpenWithOptions(path, randomKey(t), Options{
		BeforeDurable: func(batch DurableBatch) (AppendAdmission, error) {
			info, statErr := os.Stat(path)
			if statErr != nil || info.Size() != 0 || len(batch.Frames) == 0 {
				t.Fatalf("audit admission did not precede native write: info=%v err=%v", info, statErr)
			}
			return reserved, nil
		},
		AfterDurable: func(batch DurableBatch) error { received = batch.Admission; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(context.Background(), validEvent(1, "one")); err != nil {
		t.Fatal(err)
	}
	if received != reserved || reserved.releases != 0 {
		t.Fatal("audit did not transfer the admission to ordering", received, reserved.releases)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if reserved.releases != 0 {
		t.Fatal("audit source released an ordering-owned admission", reserved.releases)
	}
}

func TestAuditAdmissionRefusalLeavesNativeHeadAndFileUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	log, err := OpenWithOptions(path, randomKey(t), Options{
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
	if _, err := log.Append(context.Background(), validEvent(1, "admission_refused")); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatalf("admission refusal error=%v", err)
	}
	afterHead := log.Summary()
	afterInfo, err := os.Stat(path)
	if err != nil || beforeHead != afterHead || beforeInfo.Size() != afterInfo.Size() {
		t.Fatalf("refused append changed Audit source: before=(%+v,%d) after=(%+v,%v,%v)", beforeHead, beforeInfo.Size(), afterHead, afterInfo, err)
	}
}

func TestAuditUncertainNativeAppendRetainsAdmissionUntilClose(t *testing.T) {
	for _, mode := range []string{"write", "sync"} {
		t.Run(mode, func(t *testing.T) {
			reserved := &auditAppendAdmissionFixture{}
			var afterCalls int
			log, err := OpenWithOptions(filepath.Join(t.TempDir(), "audit.log"), randomKey(t), Options{
				BeforeDurable: func(DurableBatch) (AppendAdmission, error) { return reserved, nil },
				AfterDurable:  func(DurableBatch) error { afterCalls++; return nil },
				WrapDurability: func(file *os.File) DurabilityWriter {
					return &auditAdmissionFaultDurability{file: file, mode: mode}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(context.Background(), validEvent(1, "uncertain_"+mode)); err == nil {
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

func TestAuditDurableHookCapturesOneFsyncedBatchAndPoisonsOnFailure(t *testing.T) {
	var captured DurableBatch
	log, err := OpenWithOptions(filepath.Join(t.TempDir(), "audit.log"), randomKey(t), Options{
		AfterDurable: func(batch DurableBatch) error {
			captured = batch
			return errors.New("ordering disk full")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	events := []Event{validEvent(1, "one"), validEvent(2, "two")}
	if _, err := log.AppendBatch(context.Background(), events); err == nil || !strings.Contains(err.Error(), "ordering disk full") {
		t.Fatalf("append error=%v", err)
	}
	if captured.FirstSequence != 1 || captured.LastSequence != 2 || len(captured.Frames) == 0 {
		t.Fatalf("captured=%#v", captured)
	}
	if _, err := log.Append(context.Background(), validEvent(3, "three")); err == nil || !strings.Contains(err.Error(), "requires restart") {
		t.Fatalf("poison error=%v", err)
	}
}
