package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// These callers use actual Replica native fsync, rather than semantic Append,
// so the read lease must drain/exclude them through the same mutation registry.
func TestReplicaReadMaintenanceJoinsActualReplicatedAppendAndRoll(t *testing.T) {
	for _, operation := range []string{"append", "roll"} {
		for _, join := range []string{"lease", "close"} {
			t.Run(operation+"/"+join, func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				primaryPath := filepath.Join(dir, "primary", "ledger.wal")
				primary, err := OpenWithOptions(primaryPath, NewStatus(), Options{ChainKey: testChainKey, MaxBatch: 1})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = primary.Append(ctx, validReservation("evt_replica_join", "att_replica_join")); err != nil {
					t.Fatal(err)
				}
				frames, err := os.ReadFile(primaryPath)
				if err != nil {
					t.Fatal(err)
				}
				segment, err := primary.Roll()
				if err != nil {
					t.Fatal(err)
				}
				if err = primary.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, "replica"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "replica", "ledger.wal"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				var d *snapshotBlockingDurability
				native, err := OpenWithOptions(filepath.Join(dir, "replica", "ledger.wal"), NewStatus(), Options{ChainKey: testChainKey, Replica: true, WrapDurability: func(f *os.File) DurabilityWriter {
					d = &snapshotBlockingDurability{file: f, entered: make(chan struct{}), release: make(chan struct{})}
					return d
				}})
				if err != nil {
					t.Fatal(err)
				}
				var once sync.Once
				release := func() { once.Do(func() { close(d.release) }) }
				t.Cleanup(func() {
					release()
					if err := native.Close(); err != nil {
						t.Error(err)
					}
				})
				if operation == "roll" {
					if err = native.AppendReplicated(1, 1, 1, frames); err != nil {
						t.Fatal(err)
					}
				}
				d.block.Store(true)
				written := make(chan error, 1)
				go func() {
					if operation == "append" {
						written <- native.AppendReplicated(1, 1, 1, frames)
					} else {
						_, err := native.ApplyReplicatedRoll(segment.Sealed)
						written <- err
					}
				}()
				select {
				case <-d.entered:
				case err := <-written:
					t.Fatal("did not reach actual fsync", err)
				case <-time.After(5 * time.Second):
					t.Fatal("actual fsync not entered")
				}
				type outcome struct {
					lease *MaintenanceLease
					err   error
				}
				joined := make(chan outcome, 1)
				if join == "lease" {
					go func() { lease, err := native.BeginReplicaReadMaintenance(ctx); joined <- outcome{lease, err} }()
					waitMaintenanceGate(t, native)
					if err := native.AppendReplicated(1, 1, 1, frames); !errors.Is(err, ErrLedgerMaintenance) {
						t.Fatal("in-flight read lease allowed replicated append", err)
					}
					if _, err := native.ApplyReplicatedRoll(segment.Sealed); !errors.Is(err, ErrLedgerMaintenance) {
						t.Fatal("in-flight read lease allowed replicated roll", err)
					}
				} else {
					go func() { joined <- outcome{err: native.Close()} }()
				}
				select {
				case result := <-joined:
					t.Fatal("actual fsync was not joined", result.err)
				case <-time.After(25 * time.Millisecond):
				}
				release()
				if err := <-written; err != nil {
					t.Fatal("accepted actual native fact lost", err)
				}
				result := <-joined
				if result.err != nil {
					t.Fatal(result.err)
				}
				if join == "lease" {
					if result.lease == nil {
						t.Fatal("no ready read lease")
					}
					if err := native.AppendReplicated(1, 1, 1, frames); !errors.Is(err, ErrLedgerMaintenance) {
						t.Fatal("ready read lease allowed replicated append", err)
					}
					if _, err := native.ApplyReplicatedRoll(segment.Sealed); !errors.Is(err, ErrLedgerMaintenance) {
						t.Fatal("ready read lease allowed replicated roll", err)
					}
					if err := result.lease.Close(); err != nil {
						t.Fatal(err)
					}
					// Closing an old lease cannot revoke the successor's exclusion.
					successor, err := native.BeginReplicaReadMaintenance(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if err := result.lease.Close(); err != nil {
						t.Fatal(err)
					}
					if err := native.AppendReplicated(1, 1, 1, frames); !errors.Is(err, ErrLedgerMaintenance) {
						t.Fatal("old Close released successor lease", err)
					}
					if err := successor.Close(); err != nil {
						t.Fatal(err)
					}
					if operation == "append" {
						if err := native.AppendReplicated(1, 1, 1, frames); err != nil {
							t.Fatal("durable duplicate after joined read lease refused", err)
						}
					}
				} else {
					if err := native.AppendReplicated(1, 1, 1, frames); err == nil {
						t.Fatal("closed native owner accepted append")
					}
					if _, err := native.ApplyReplicatedRoll(segment.Sealed); err == nil {
						t.Fatal("closed native owner accepted roll")
					}
				}
			})
		}
	}
}
