package app

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/audit"
	"os"
	"testing"
	"time"
)

func TestAdminAuditPauseJoinsActualIOAndCannotUndoRetirement(t *testing.T) {
	r, _, _ := openBootstrappedRuntime(t)
	master, err := unlockMasterKey(context.Background(), r.config)
	if err != nil {
		t.Fatal(err)
	}
	key, err := loadAuditHMACKey(r.store, r.vault, master)
	clear(master)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	if err = r.audit.Close(); err != nil {
		t.Fatal(err)
	}
	var blocked *projectedWriterBlockedDurability
	log, err := audit.OpenWithOptions(r.config.AuditPath(), key, audit.Options{WrapDurability: func(f *os.File) audit.DurabilityWriter {
		blocked = &projectedWriterBlockedDurability{file: f, entered: make(chan struct{}), release: make(chan struct{})}
		blocked.block.Store(true)
		return blocked
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.audit = log
	released := false
	defer func() {
		if !released {
			close(blocked.release)
		}
	}()
	done := make(chan error, 1)
	go func() {
		done <- r.appendAdminAudit("system", "pause", "bounded.audit", "gateway", "local", "success", "")
	}()
	<-blocked.entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	resume, err := r.pauseAdminAuditBatches(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || resume == nil {
		t.Fatal("pause assumed writer exit", err)
	}
	if err = resume(); !errors.Is(err, errAdminAuditBatchUnavailable) {
		t.Fatal("resume before actual IO joined", err)
	}
	if err = r.appendAdminAudit("system", "new", "bounded.audit", "gateway", "local", "success", ""); !errors.Is(err, errAdminAuditBatchUnavailable) {
		t.Fatal("new enqueue during pause", err)
	}
	close(blocked.release)
	released = true
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = resume(); err != nil {
		t.Fatal("known joined resume", err)
	}
	if err = resume(); !errors.Is(err, errAdminAuditBatchUnavailable) {
		t.Fatal("duplicate resume", err)
	}
	resume, err = r.pauseAdminAuditBatches(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = r.closeAdminAuditBatches(); err != nil {
		t.Fatal(err)
	}
	if err = resume(); !errors.Is(err, errAdminAuditBatchUnavailable) {
		t.Fatal("retired owner resurrected", err)
	}
}
