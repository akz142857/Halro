//go:build linux

package authenticatedstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPhysicalRepackHeadroomBelowHalfKeepsSelectionAndBenefitGates(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	f := newRepackFixture(t)
	s, ctx := f.store, context.Background()
	for s.physicalExtent <= uint64(s.ceiling-3*s.appendLimit) {
		if _, err := s.Update(ctx, nextCut(int(s.physicalRevision)), func(tx *Transaction) error {
			return tx.Put([]byte("key_000001"), bytes.Repeat([]byte{byte(s.physicalRevision)}, 2048))
		}); err != nil {
			t.Fatal(err)
		}
	}
	if s.physicalExtent >= uint64(s.ceiling/2) {
		t.Fatal("fixture missed earlier physical demand window")
	}
	f.seal.Revision++
	var err error
	f.seal.Roots[0], err = s.SealRootReference(ctx, f.schema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.publisher.Publish(ctx, f.seal); err != nil {
		t.Fatal(err)
	}
	nativePath := filepath.Join(filepath.Dir(s.dir), "native-headroom")
	if err = os.WriteFile(nativePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	native, verify := repackPhysicalNativeFixture(t, nativePath)
	raw, err := json.Marshal(f.seal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnablePhysicalTerminalQualified(ctx, sha256.Sum256(raw), native, verify); err != nil {
		t.Fatal(err)
	}
	w, err := s.ReservePhysicalTerminals(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if s.physical.maxObservedTerminalCount != 2 {
		t.Fatal("original reservation demand was not retained")
	}
	oldEnd := s.physical.allocatedEnd
	before := f.options.Budget.Snapshot().Used
	wrong := f.seal
	wrong.Revision++
	if _, err = s.PrepareRepackForPhysicalHeadroom(ctx, f.publisher, f.guard, wrong, f.schema, f.reservation); !errors.Is(err, ErrSealIneligible) {
		t.Fatal("caller selection admitted", err)
	}
	if s.physical.allocatedEnd != oldEnd || f.options.Budget.Snapshot().Used != before {
		t.Fatal("unqualified demand retired targets or fees")
	}
	if candidate, err := s.PrepareRepack(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation); err != nil || candidate != nil {
		t.Fatal("ordinary half ceiling gate changed", err)
	}
	want := storeRows(t, s)
	oldExtent := s.physicalExtent
	candidate, err := s.PrepareRepackForPhysicalHeadroom(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation)
	if err != nil || candidate == nil {
		t.Fatal("qualified original headroom did not copy", err)
	}
	if s.physicalExtent > oldExtent-oldExtent/8 {
		t.Fatal("original authenticated benefit gate changed")
	}
	if err = candidate.Commit(); !errors.Is(err, ErrSealIneligible) {
		t.Fatal("unselected compact root retired original", err)
	}
	f.seal.Revision++
	f.seal.Roots[0], err = s.SealRootReference(ctx, f.schema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.publisher.Publish(ctx, f.seal); err != nil {
		t.Fatal(err)
	}
	if err = candidate.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := storeRows(t, s); !reflect.DeepEqual(got, want) {
		t.Fatal("reachable business rows changed")
	}
}

func TestPhysicalRepackHeadroomCannotWaiveCompactBenefit(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	f := newRepackFixture(t)
	s, ctx := f.store, context.Background()
	nativePath := filepath.Join(filepath.Dir(s.dir), "native-no-benefit")
	if err := os.WriteFile(nativePath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	native, verify := repackPhysicalNativeFixture(t, nativePath)
	raw, err := json.Marshal(f.seal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnablePhysicalTerminalQualified(ctx, sha256.Sum256(raw), native, verify); err != nil {
		t.Fatal(err)
	}
	w, err := s.ReservePhysicalTerminals(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	want := storeRows(t, s)
	oldRoot, oldPack := s.current, s.info
	if !s.physicalHeadroomExhaustedLocked() {
		t.Fatal("fixture did not exercise physical headroom")
	}
	candidate, err := s.PrepareRepackForPhysicalHeadroom(ctx, f.publisher, f.guard, f.seal, f.schema, f.reservation)
	if err != nil || candidate != nil {
		t.Fatal("physical demand waived original compact benefit", err)
	}
	if s.current != oldRoot || !os.SameFile(s.info, oldPack) || !reflect.DeepEqual(storeRows(t, s), want) {
		t.Fatal("nonbeneficial demand replaced selected source")
	}
}
