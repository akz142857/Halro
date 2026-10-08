//go:build linux

package authenticatedstate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPhysicalRepeatPreparationValidatesOriginalControlsWithoutNewFees(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	f := newRepackFixture(t)
	s, ctx := f.store, context.Background()
	path := filepath.Join(filepath.Dir(s.dir), "native-repeat")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	native, verify := repackPhysicalNativeFixture(t, path)
	raw, err := json.Marshal(f.seal)
	if err != nil {
		t.Fatal(err)
	}
	selection := sha256.Sum256(raw)
	if found, err := s.ValidatePhysicalTerminalPreparation(ctx, selection, native, verify); found || err != nil {
		t.Fatal("fresh owner manufactured", found, err)
	}
	if _, err = s.EnablePhysicalTerminalQualified(ctx, selection, native, verify); err != nil {
		t.Fatal(err)
	}
	before := f.options.Budget.Snapshot()
	if found, err := s.ValidatePhysicalTerminalPreparation(ctx, selection, native, verify); !found || err != nil {
		t.Fatal("actual original owner was not reusable", found, err)
	}
	if after := f.options.Budget.Snapshot(); after.Used != before.Used || after.Owners != before.Owners {
		t.Fatal("read-only qualification charged/refunded fees", before, after)
	}
	wrong := selection
	wrong[0] ^= 1
	if found, err := s.ValidatePhysicalTerminalPreparation(ctx, wrong, native, verify); !found || !errors.Is(err, ErrSealIneligible) {
		t.Fatal("wrong independent selection admitted", found, err)
	}
	control := s.physical.receipt.files[s.physical.receipt.bank]
	var original [physicalReceiptBytes]byte
	if _, err = control.ReadAt(original[:], 0); err != nil {
		t.Fatal(err)
	}
	if _, err = control.WriteAt([]byte{original[0] ^ 1}, 0); err != nil {
		t.Fatal(err)
	}
	if found, err := s.ValidatePhysicalTerminalPreparation(ctx, selection, native, verify); !found || !errors.Is(err, ErrCorrupt) {
		t.Fatal("mutated same inode control admitted", found, err)
	}
	if _, err = control.WriteAt(original[:], 0); err != nil {
		t.Fatal(err)
	}
	if err = control.Sync(); err != nil {
		t.Fatal(err)
	}
	if after := f.options.Budget.Snapshot(); after.Used != before.Used || after.Owners != before.Owners {
		t.Fatal("failed qualification erased actual owner fees")
	}
	window, err := s.ReservePhysicalTerminals(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := s.ValidatePhysicalTerminalPreparation(ctx, selection, native, verify); !found || !errors.Is(err, ErrRecovery) {
		t.Fatal("live window was cleared by duplicate preparation", found, err)
	}
	if err = window.Close(); err != nil {
		t.Fatal(err)
	}
}
