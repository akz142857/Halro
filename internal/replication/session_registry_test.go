package replication

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type registryTestSession struct{ closes atomic.Uint64 }

func (s *registryTestSession) Close() error {
	s.closes.Add(1)
	return nil
}

func TestSessionRegistryReplacementWaitsForAdmittedDelivery(t *testing.T) {
	registry, err := NewSessionRegistry("halro-0", []string{"halro-1"})
	if err != nil {
		t.Fatal(err)
	}
	first := &registryTestSession{}
	firstGeneration, err := registry.Register("halro-1", true, first)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	delivered := make(chan error, 1)
	go func() {
		delivered <- registry.Deliver("halro-1", firstGeneration, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	second := &registryTestSession{}
	registered := make(chan struct{})
	var secondGeneration uint64
	var registerErr error
	var resultMu sync.Mutex
	go func() {
		generation, err := registry.Register("halro-1", true, second)
		resultMu.Lock()
		secondGeneration, registerErr = generation, err
		resultMu.Unlock()
		close(registered)
	}()
	select {
	case <-registered:
		t.Fatal("replacement published before admitted delivery drained")
	case <-time.After(20 * time.Millisecond):
	}
	if first.closes.Load() != 1 {
		t.Fatalf("old session closes=%d want=1", first.closes.Load())
	}
	close(release)
	if err := <-delivered; err != nil {
		t.Fatal(err)
	}
	<-registered
	resultMu.Lock()
	defer resultMu.Unlock()
	if registerErr != nil || !registry.IsCurrent("halro-1", secondGeneration) {
		t.Fatalf("replacement generation=%d err=%v current=%t", secondGeneration, registerErr, registry.IsCurrent("halro-1", secondGeneration))
	}
}

func TestSessionRegistrySelectsOneConnectionAndRejectsStaleGeneration(t *testing.T) {
	registry, err := NewSessionRegistry("halro-0", []string{"halro-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !registry.PreferOutbound("halro-1") {
		t.Fatal("lower node did not own deterministic dial")
	}
	wrong := &registryTestSession{}
	if _, err := registry.Register("halro-1", false, wrong); !errors.Is(err, ErrNonPreferredPeerConnection) || wrong.closes.Load() != 1 {
		t.Fatalf("non-preferred session err=%v closes=%d", err, wrong.closes.Load())
	}
	first := &registryTestSession{}
	firstGeneration, err := registry.Register("halro-1", true, first)
	if err != nil {
		t.Fatal(err)
	}
	second := &registryTestSession{}
	secondGeneration, err := registry.Register("halro-1", true, second)
	if err != nil {
		t.Fatal(err)
	}
	if first.closes.Load() != 1 || registry.IsCurrent("halro-1", firstGeneration) || !registry.IsCurrent("halro-1", secondGeneration) {
		t.Fatalf("replacement first_closes=%d first_current=%t second_current=%t", first.closes.Load(), registry.IsCurrent("halro-1", firstGeneration), registry.IsCurrent("halro-1", secondGeneration))
	}
	registry.Remove("halro-1", firstGeneration)
	if !registry.IsCurrent("halro-1", secondGeneration) {
		t.Fatal("stale remove deleted the current session")
	}
	registry.Remove("halro-1", secondGeneration)
	if registry.IsCurrent("halro-1", secondGeneration) {
		t.Fatal("current session survived removal")
	}
}

func TestSessionRegistryCloseStopsCurrentSessionsAndFutureRegistration(t *testing.T) {
	registry, err := NewSessionRegistry("halro-2", []string{"halro-0", "halro-1"})
	if err != nil {
		t.Fatal(err)
	}
	first := &registryTestSession{}
	if _, err := registry.Register("halro-0", false, first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil || first.closes.Load() != 1 {
		t.Fatalf("close err=%v closes=%d", err, first.closes.Load())
	}
	late := &registryTestSession{}
	if _, err := registry.Register("halro-1", false, late); err == nil || late.closes.Load() != 1 {
		t.Fatalf("late registration err=%v closes=%d", err, late.closes.Load())
	}
}
