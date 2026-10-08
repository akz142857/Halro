package metadatajournal

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

type failingDurability struct {
	file      *os.File
	writeErr  error
	syncErr   error
	zeroWrite bool
}

func (d *failingDurability) Write(value []byte) (int, error) {
	if d.writeErr != nil {
		return 0, d.writeErr
	}
	if d.zeroWrite {
		return 0, nil
	}
	return d.file.Write(value)
}

func (d *failingDurability) Sync() error {
	if d.syncErr != nil {
		return d.syncErr
	}
	return d.file.Sync()
}

func TestMetadataDurableHookRunsAfterFsyncAndPoisonsOnFailure(t *testing.T) {
	_, log := newJournal(t)
	var captured DurableBatch
	if err := log.SetAfterDurable(func(batch DurableBatch) (uint64, error) {
		captured = batch
		return 0, errors.New("ordering disk full")
	}, func(context.Context, uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append([]Op{put("projects", "p1", "one")}); err == nil || !strings.Contains(err.Error(), "ordering disk full") {
		t.Fatalf("append error=%v", err)
	}
	if captured.Epoch != 1 || captured.FirstSequence != 1 || captured.LastSequence != 1 || len(captured.Frames) == 0 {
		t.Fatalf("captured=%#v", captured)
	}
	if _, err := log.Append([]Op{put("projects", "p2", "two")}); err == nil || !strings.Contains(err.Error(), "requires restart") {
		t.Fatalf("poison error=%v", err)
	}
	if err := log.SetAfterDurable(func(DurableBatch) (uint64, error) { return 1, nil }, func(context.Context, uint64) error { return nil }); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("second callback error=%v", err)
	}
}

type metadataAppendAdmissionFixture struct{ releases int }

func (a *metadataAppendAdmissionFixture) Release() error { a.releases++; return nil }

type metadataAdmissionFaultDurability struct {
	file *os.File
	mode string
}

func (d *metadataAdmissionFaultDurability) Write(value []byte) (int, error) {
	if d.mode == "write" {
		return 0, errors.New("injected native write failure")
	}
	return d.file.Write(value)
}
func (d *metadataAdmissionFaultDurability) Sync() error {
	if d.mode == "sync" {
		return errors.New("injected native sync failure")
	}
	return d.file.Sync()
}

func TestMetadataAdmissionRunsBeforeNativeWriteAndTransfersToOrderingHook(t *testing.T) {
	path, log := newJournal(t)
	initialInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	reserved := &metadataAppendAdmissionFixture{}
	var received AppendAdmission
	err = log.SetReplicationAdmissionAndAfterDurable(func(batch DurableBatch) (AppendAdmission, error) {
		info, statErr := os.Stat(path)
		if statErr != nil || info.Size() != initialInfo.Size() || len(batch.Frames) == 0 {
			t.Fatalf("metadata admission did not precede native write: info=%v err=%v", info, statErr)
		}
		return reserved, nil
	}, func(batch DurableBatch) (uint64, error) { received = batch.Admission; return 1, nil }, func(context.Context, uint64) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append([]Op{put("projects", "p1", "one")}); err != nil {
		t.Fatal(err)
	}
	if received != reserved || reserved.releases != 0 {
		t.Fatal("metadata did not transfer the admission to ordering", received, reserved.releases)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if reserved.releases != 0 {
		t.Fatal("metadata source released an ordering-owned admission", reserved.releases)
	}
}

func TestMetadataAdmissionRefusalLeavesNativeHeadAndFileUnchanged(t *testing.T) {
	path, log := newJournal(t)
	defer log.Close()
	beforeHead := log.Head()
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.SetReplicationAdmissionAndAfterDurable(
		func(DurableBatch) (AppendAdmission, error) { return nil, resourcebudget.ErrExhausted },
		func(DurableBatch) (uint64, error) { return 1, nil },
		func(context.Context, uint64) error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append([]Op{put("projects", "admission_refused", "value")}); !errors.Is(err, resourcebudget.ErrExhausted) {
		t.Fatalf("admission refusal error=%v", err)
	}
	afterHead := log.Head()
	afterInfo, err := os.Stat(path)
	if err != nil || beforeHead != afterHead || beforeInfo.Size() != afterInfo.Size() {
		t.Fatalf("refused append changed Metadata source: before=(%+v,%d) after=(%+v,%v,%v)", beforeHead, beforeInfo.Size(), afterHead, afterInfo, err)
	}
}

func TestMetadataUncertainNativeAppendRetainsAdmissionUntilClose(t *testing.T) {
	for _, mode := range []string{"write", "sync"} {
		t.Run(mode, func(t *testing.T) {
			previous := WrapDurability
			var fault *metadataAdmissionFaultDurability
			WrapDurability = func(file *os.File) DurabilityWriter {
				fault = &metadataAdmissionFaultDurability{file: file}
				return fault
			}
			t.Cleanup(func() { WrapDurability = previous })
			path, log := newJournal(t)
			reserved := &metadataAppendAdmissionFixture{}
			var afterCalls int
			if err := log.SetReplicationAdmissionAndAfterDurable(
				func(DurableBatch) (AppendAdmission, error) { return reserved, nil },
				func(DurableBatch) (uint64, error) { afterCalls++; return 1, nil },
				func(context.Context, uint64) error { return nil },
			); err != nil {
				t.Fatal(err)
			}
			fault.mode = mode
			if _, err := log.Append([]Op{put("projects", "uncertain_"+mode, "value")}); err == nil {
				t.Fatal("injected uncertain append unexpectedly succeeded")
			}
			if afterCalls != 0 || reserved.releases != 0 {
				t.Fatalf("uncertain append lost the source-owned admission: after=%d releases=%d", afterCalls, reserved.releases)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			if reserved.releases != 1 {
				t.Fatalf("closing uncertain source %s did not retire its in-process admission: releases=%d", path, reserved.releases)
			}
		})
	}
}

func TestMetadataReceiptWaitIsSeparateFromAppend(t *testing.T) {
	_, log := newJournal(t)
	waits := make(chan uint64, 1)
	if err := log.SetAfterDurable(func(batch DurableBatch) (uint64, error) {
		if batch.FirstSequence != 1 || batch.LastSequence != 1 {
			t.Fatalf("batch=%#v", batch)
		}
		return 17, nil
	}, func(_ context.Context, index uint64) error {
		waits <- index
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	receipt, err := log.AppendWithReceipt([]Op{put("projects", "p1", "one")})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Record.Sequence != 1 || receipt.ReplicationIndex != 17 {
		t.Fatalf("receipt=%#v", receipt)
	}
	select {
	case index := <-waits:
		t.Fatalf("append waited unexpectedly for %d", index)
	default:
	}
	if err := log.WaitConfirmed(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	if index := <-waits; index != 17 {
		t.Fatalf("waited index=%d", index)
	}
}

func TestMetadataJournalPoisonsAfterUncertainWriteOrSync(t *testing.T) {
	tests := []struct {
		name string
		wrap func(*os.File) DurabilityWriter
		want string
	}{
		{name: "write", want: "write failed", wrap: func(file *os.File) DurabilityWriter {
			return &failingDurability{file: file, writeErr: errors.New("write failed")}
		}},
		{name: "zero_progress", want: io.ErrShortWrite.Error(), wrap: func(file *os.File) DurabilityWriter {
			return &failingDurability{file: file, zeroWrite: true}
		}},
		{name: "sync", want: "sync failed", wrap: func(file *os.File) DurabilityWriter {
			return &failingDurability{file: file, syncErr: errors.New("sync failed")}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			previous := WrapDurability
			WrapDurability = test.wrap
			t.Cleanup(func() { WrapDurability = previous })
			_, log := newJournal(t)
			if _, err := log.Append([]Op{put("projects", "p1", "one")}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("first append error=%v, want %v", err, test.want)
			}
			if _, err := log.Append([]Op{put("projects", "p2", "two")}); err == nil || !strings.Contains(err.Error(), "requires restart") {
				t.Fatalf("second append error=%v", err)
			}
		})
	}
}
