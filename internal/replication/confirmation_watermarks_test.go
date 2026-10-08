package replication

import (
	"errors"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
)

// This test-only reference retains the old per-index ACK algorithm. It is
// independent of the new scalar implementation and compares acceptance plus
// state after every valid and invalid operation, including failed registration.
type indexedConfirmationReference struct {
	confirmed, registered, anchor uint64
	pending                       map[uint64]map[string]bool
}

func newIndexedConfirmationReference(confirmed uint64, established bool) *indexedConfirmationReference {
	m := &indexedConfirmationReference{confirmed: confirmed, registered: confirmed, pending: make(map[uint64]map[string]bool)}
	if established {
		m.anchor = confirmed
	}
	return m
}

func (m *indexedConfirmationReference) register(frame Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	if frame.ClusterID != "production-a" || frame.Incarnation != "inc_01" || frame.Term != 7 || frame.Index != m.registered+1 {
		return errors.New("registration identity or continuity")
	}
	if m.anchor == 0 {
		if frame.Kind != KindLeadershipEstablished {
			return errors.New("missing anchor")
		}
		m.anchor = frame.Index
	} else if frame.Kind == KindLeadershipEstablished {
		return errors.New("second anchor")
	}
	m.pending[frame.Index] = make(map[string]bool)
	m.registered = frame.Index
	return nil
}

func (m *indexedConfirmationReference) acknowledge(ack Acknowledgement) (uint64, error) {
	if err := ack.Validate(); err != nil {
		return m.confirmed, err
	}
	if ack.ClusterID != "production-a" || ack.Incarnation != "inc_01" || ack.Term != 7 || (ack.NodeID != "halro-1" && ack.NodeID != "halro-2") {
		return m.confirmed, errors.New("acknowledgement identity or membership")
	}
	if ack.Index <= m.confirmed {
		return m.confirmed, nil
	}
	if ack.Index > m.registered {
		return m.confirmed, errors.New("future acknowledgement")
	}
	for index := m.confirmed + 1; index <= ack.Index; index++ {
		entry, ok := m.pending[index]
		if !ok {
			return m.confirmed, errors.New("unregistered acknowledgement")
		}
		entry[ack.NodeID] = true
	}
	for {
		next, ok := m.pending[m.confirmed+1]
		if !ok || len(next) < 1 {
			break
		}
		if m.confirmed < m.anchor && m.confirmed+1 != m.anchor {
			break
		}
		delete(m.pending, m.confirmed+1)
		m.confirmed++
	}
	return m.confirmed, nil
}

func TestConfirmationWatermarksMatchIndexedReferenceAcrossMixedTenureOperations(t *testing.T) {
	for _, baseline := range []struct {
		confirmed   uint64
		established bool
	}{{0, false}, {100, false}, {100, true}} {
		for seed := int64(1); seed <= 8; seed++ {
			tracker, err := NewConfirmationTracker("production-a", "inc_01", 7, baseline.confirmed, []string{"halro-1", "halro-2"}, baseline.established)
			if err != nil {
				t.Fatal(err)
			}
			reference := newIndexedConfirmationReference(baseline.confirmed, baseline.established)
			random := rand.New(rand.NewSource(seed))
			for step := 0; step < 10000; step++ {
				if random.Intn(3) == 0 {
					kind := KindData
					if reference.anchor == 0 {
						kind = KindLeadershipEstablished
					}
					frame := testConfirmationFrame(reference.registered+1, kind)
					switch random.Intn(8) {
					case 1:
						frame.Index++ // Gap must not change registration.
					case 2:
						frame.Index--
					case 3:
						frame.Term--
					case 4:
						frame.ClusterID = "another-cluster"
					case 5:
						frame.Incarnation = "another-incarnation"
					case 6:
						frame = testConfirmationFrame(frame.Index, KindLeadershipEstablished)
					}
					got, want := tracker.Register(frame), reference.register(frame)
					if (got == nil) != (want == nil) {
						t.Fatalf("baseline=%+v seed=%d step=%d Register got=%v want=%v", baseline, seed, step, got, want)
					}
				} else {
					ack := testAcknowledgement("halro-1", uint64(random.Int63n(int64(reference.registered+4))))
					if random.Intn(2) == 0 {
						ack.NodeID = "halro-2"
					}
					switch random.Intn(10) {
					case 1:
						ack.Term--
					case 2:
						ack.ClusterID = "another-cluster"
					case 3:
						ack.Incarnation = "another-incarnation"
					case 4:
						ack.NodeID = "unconfigured"
					case 5:
						ack.DurableIndex++
					case 6:
						ack.AppliedIndex = ack.Index + 1
					}
					got, gotErr := tracker.Acknowledge(ack)
					want, wantErr := reference.acknowledge(ack)
					if got != want || (gotErr == nil) != (wantErr == nil) {
						t.Fatalf("baseline=%+v seed=%d step=%d ACK got=%d/%v want=%d/%v", baseline, seed, step, got, gotErr, want, wantErr)
					}
				}
				if tracker.Confirmed() != reference.confirmed || tracker.lastRegistered != reference.registered || tracker.anchorIndex != reference.anchor {
					t.Fatalf("state diverged at seed=%d step=%d", seed, step)
				}
			}
		}
	}
}

func TestConfirmationWatermarksLargeUnacknowledgedSuffixDoesNotAllocatePerIndex(t *testing.T) {
	tracker, err := NewConfirmationTracker("production-a", "inc_01", 7, 0, []string{"halro-1", "halro-2"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Register(testConfirmationFrame(1, KindLeadershipEstablished)); err != nil {
		t.Fatal(err)
	}
	frame := testConfirmationFrame(2, KindData)
	index := uint64(1)
	allocations := testing.AllocsPerRun(1, func() {
		for count := 0; count < 100000; count++ {
			index++
			frame.Index, frame.StoreSequenceFirst, frame.StoreSequenceLast = index, index, index
			if err := tracker.Register(frame); err != nil {
				t.Fatal(err)
			}
		}
	})
	if allocations != 0 {
		t.Fatalf("registration allocates historical state: %.0f allocations per 100000 frames", allocations)
	}
	if tracker.Confirmed() != 0 || tracker.lastRegistered != 200001 {
		t.Fatal("unconfirmed prefix changed")
	}
	if got, err := tracker.Acknowledge(testAcknowledgement("halro-2", index)); err != nil || got != index {
		t.Fatal(got, err)
	}
}

func TestConfirmationWatermarksRefuseIndexWrapAfterMaximum(t *testing.T) {
	// The old index-walking ACK loop stepped past MaxUint64 into zero and
	// rejected this valid final ACK as an unregistered index. Scalar progress
	// handles the final registered index while still refusing registration wrap.
	reference := newIndexedConfirmationReference(math.MaxUint64-1, true)
	if err := reference.register(testConfirmationFrame(math.MaxUint64, KindData)); err != nil {
		t.Fatal(err)
	}
	if got, err := reference.acknowledge(testAcknowledgement("halro-1", math.MaxUint64)); err == nil || got != math.MaxUint64-1 {
		t.Fatal("old reference did not reproduce maximum-index ACK loop wrap", got, err)
	}
	tracker, err := NewConfirmationTracker("production-a", "inc_01", 7, math.MaxUint64-1, []string{"halro-1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Register(testConfirmationFrame(math.MaxUint64, KindData)); err != nil {
		t.Fatal(err)
	}
	if got, err := tracker.Acknowledge(testAcknowledgement("halro-1", math.MaxUint64)); err != nil || got != math.MaxUint64 {
		t.Fatal(got, err)
	}
	for _, index := range []uint64{0, 1, math.MaxUint64} {
		if err := tracker.Register(testConfirmationFrame(index, KindData)); err == nil {
			t.Fatal("accepted wrapped or repeated registration", index)
		}
	}
	if tracker.lastRegistered != math.MaxUint64 || tracker.Confirmed() != math.MaxUint64 {
		t.Fatal("maximum prefix changed")
	}
}

func TestConfirmationWatermarksConcurrentRegistrationAndCumulativePeers(t *testing.T) {
	tracker, err := NewConfirmationTracker("production-a", "inc_01", 7, 0, []string{"halro-1", "halro-2"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.Register(testConfirmationFrame(1, KindLeadershipEstablished)); err != nil {
		t.Fatal(err)
	}
	var registered atomic.Uint64
	registered.Store(1)
	done := make(chan struct{})
	errors := make(chan error, 3)
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		defer close(done)
		frame := testConfirmationFrame(2, KindData)
		for index := uint64(2); index <= 10001; index++ {
			frame.Index, frame.StoreSequenceFirst, frame.StoreSequenceLast = index, index, index
			if err := tracker.Register(frame); err != nil {
				errors <- err
				return
			}
			registered.Store(index)
		}
	}()
	for _, peer := range []string{"halro-1", "halro-2"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				if _, err := tracker.Acknowledge(testAcknowledgement(peer, registered.Load())); err != nil {
					errors <- err
					return
				}
			}
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	if got, err := tracker.Acknowledge(testAcknowledgement("halro-2", 10001)); err != nil || got != 10001 {
		t.Fatal(got, err)
	}
}
