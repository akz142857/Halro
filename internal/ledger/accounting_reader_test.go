package ledger

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestAccountingLeaseReaderStatePositionAbsenceAndErrors(t *testing.T) {
	s := NewState()
	record := Record{Generation: 1, Sequence: 1, Offset: 100, Event: validReservation("evt_lease_reader", "att_reader")}
	if err := s.Apply(record); err != nil {
		t.Fatal(err)
	}
	actual, found, err := s.ReadLease(context.Background(), "att_reader")
	if err != nil || !found || !reflect.DeepEqual(record, actual) {
		t.Fatal(actual, found, err)
	}
	if _, found, err := s.ReadLease(context.Background(), "absent"); err != nil || found {
		t.Fatal(found, err)
	}
	head, hash, err := s.ReadPosition(context.Background())
	if err != nil || head != s.Watermark() || hash != ([32]byte{}) {
		t.Fatal("legacy reader fabricated hash or lost position", head, hash, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.ReadLease(cancelled, "att_reader"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, _, err := s.ReadPosition(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var absent *State
	if _, _, err := absent.ReadLease(context.Background(), "att_reader"); err == nil {
		t.Fatal("nil State read succeeded")
	}
	if _, _, err := absent.ReadPosition(context.Background()); err == nil {
		t.Fatal("nil State position succeeded")
	}
	var missing *LedgerProjection
	if _, _, err := missing.ReadPosition(context.Background()); err == nil {
		t.Fatal("nil projection position succeeded")
	}
}
