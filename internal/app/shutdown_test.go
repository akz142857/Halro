package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/replication"
)

type fakeShutdownServer struct {
	shutdownError error
	shutdownCalls int
	closeCalls    int
}

func (server *fakeShutdownServer) Shutdown(context.Context) error {
	server.shutdownCalls++
	return server.shutdownError
}

func (server *fakeShutdownServer) Close() error {
	server.closeCalls++
	return nil
}

func TestGracefulShutdownRecordsAndClosesAttemptsAtDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	server := &fakeShutdownServer{shutdownError: context.DeadlineExceeded}
	var recorded uint64
	errs := gracefullyShutdownHTTPServers(
		ctx,
		[]shutdownHTTPServer{server},
		func() uint64 { return 3 },
		func(delta uint64) error { recorded = delta; return nil },
	)
	if recorded != 3 || server.shutdownCalls != 1 || server.closeCalls != 1 {
		t.Fatalf("recorded=%d shutdown=%d close=%d", recorded, server.shutdownCalls, server.closeCalls)
	}
	if !errors.Is(errors.Join(errs...), context.DeadlineExceeded) {
		t.Fatalf("deadline error missing: %v", errs)
	}
}

func TestGracefulShutdownDoesNotRecordOrForceCloseWithinBudget(t *testing.T) {
	server := &fakeShutdownServer{}
	recorded := false
	errs := gracefullyShutdownHTTPServers(
		context.Background(),
		[]shutdownHTTPServer{server},
		func() uint64 { return 3 },
		func(uint64) error { recorded = true; return nil },
	)
	if len(errs) != 0 || recorded || server.shutdownCalls != 1 || server.closeCalls != 0 {
		t.Fatalf("errors=%v recorded=%v shutdown=%d close=%d", errs, recorded, server.shutdownCalls, server.closeCalls)
	}
}

func TestPlannedStepdownDrainWithdrawsReadinessAndWaitsForHTTPServers(t *testing.T) {
	server := &fakeShutdownServer{}
	replicationRuntime := &replicationRuntime{role: replication.RolePrimary}
	replicationRuntime.startupReady.Store(true)
	runtime := &Runtime{config: testConfig(t), replication: replicationRuntime}
	runtime.installPlannedStepdownDrain([]shutdownHTTPServer{server})
	if replicationRuntime.plannedStepdownDrain == nil {
		t.Fatal("Primary did not install its planned-stepdown drain")
	}
	if err := replicationRuntime.plannedStepdownDrain(); err != nil {
		t.Fatal(err)
	}
	if !runtime.draining.Load() || replicationRuntime.startupReady.Load() {
		t.Fatalf("draining=%t startup_ready=%t", runtime.draining.Load(), replicationRuntime.startupReady.Load())
	}
	if server.shutdownCalls != 1 || server.closeCalls != 0 {
		t.Fatalf("shutdown calls=%d close calls=%d", server.shutdownCalls, server.closeCalls)
	}
}
