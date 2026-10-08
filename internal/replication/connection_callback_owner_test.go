package replication

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replicationlimits"
)

func TestConnectionManagerPreparationFailureRetainsActualSessionThroughCleanupCallback(t *testing.T) {
	manager, _ := queueBudgetFixture(t, 1, 4096+replicationlimits.MinReadRAMBytes)
	budget, err := NewPeerSessionBudget(manager, 4096, replicationlimits.MinReadRAMBytes, 1)
	if err != nil {
		t.Fatal(err)
	}
	session := queuedTestSession(t, budget)
	registry, err := NewSessionRegistry("halro-0", []string{"halro-1"})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); _ = registry.Close() })
	prepareErr := errors.New("actual prepare refused")
	connection := &ConnectionManager{registry: registry, options: ConnectionManagerOptions{
		OnDataSessionReady: func(string, Hello, func([]byte) error) error { return prepareErr },
		OnDataSessionClosed: func(string) {
			close(entered)
			<-release
		},
	}}
	done := make(chan error, 1)
	go func() {
		_, err := connection.runRegistered(context.Background(), "halro-1", true, session)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("actual cleanup callback did not start")
	}
	session.runMu.Lock()
	socketClosed, held := session.socketClosed, session.callbackOwners
	session.runMu.Unlock()
	if !socketClosed || held != 1 {
		t.Fatal("fixture did not close the socket while lending cleanup", socketClosed, held)
	}
	assertQueueFee(t, manager, budget, 1)
	if _, err := budget.acquire(); !errors.Is(err, ErrPeerQueueBudgetFull) {
		t.Fatal("cleanup callback admitted a replacement beyond actual capacity", err)
	}
	select {
	case err := <-done:
		t.Fatal("callback owner returned before cleanup joined", err)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-done; !errors.Is(err, prepareErr) {
		t.Fatal(err)
	}
	assertQueueFee(t, manager, budget, 0)
	// The real permit and replacement slot become available only after join.
	replacement := queuedTestSession(t, budget)
	if _, err := connection.runRegistered(context.Background(), "halro-1", false, replacement); !errors.Is(err, ErrNonPreferredPeerConnection) {
		t.Fatal("register refusal lost its exact cause", err)
	}
	assertQueueFee(t, manager, budget, 0)
}
