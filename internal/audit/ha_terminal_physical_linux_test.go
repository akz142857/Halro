//go:build linux

package audit

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/nativephysical"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// This is a native component qualification/IO test. Its synthetic independent
// scope is not evidence for actual Member/TLS, seven-root publication or Ready.
func TestHASelectedPhysicalNativeActualFullFilesystem(t *testing.T) {
	if os.Getenv("HALRO_EXT4_ROOT") == "" {
		t.Skip("isolated actual ext4 fixture required")
	}
	for _, replica := range []bool{false, true} {
		t.Run(map[bool]string{false: "primary", true: "replica"}[replica], func(t *testing.T) {
			ctx := context.Background()
			limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 512 << 20, Disk: 128 << 20}, RecoveryReserve: resourcebudget.Cost{RAM: 8 << 20}, MaxClaims: 64, RecoveryClaims: 4, MaxOwners: 16, MaxPressureAge: time.Hour}
			for i := range limits.Classes {
				limits.Classes[i] = limits.Total
			}
			limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 64 * resourcebudget.ClaimRAMBytes}
			limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{RAM: (8 << 20) - 4*resourcebudget.ClaimRAMBytes}
			budget, err := resourcebudget.New(limits)
			if err != nil {
				t.Fatal(err)
			}
			if err = budget.SetPressure(resourcebudget.Healthy); err != nil {
				t.Fatal(err)
			}
			f := newStartupCacheFixtureWithBudget(t, 1, budget)
			if err = f.native.PrepareHAPhysicalNative(ctx, budget, HAPhysicalOptions{}); err == nil {
				t.Fatal("Standalone native granted HA physical rights")
			}
			if err = f.native.Close(); err != nil {
				t.Fatal(err)
			}
			seal, found, err := f.selected.Publisher.Current()
			if err != nil || !found {
				t.Fatal(err)
			}
			seal.Generation++
			seal.Role = authenticatedstate.SealPrimary
			if replica {
				seal.Role = authenticatedstate.SealReplica
			}
			seal.Cluster = [32]byte{11}
			seal.Member = [32]byte{12}
			seal.Term = 1
			seal.OrderingTerm = 1
			seal.OrderingIndex = 9
			seal.OrderingOffset = 256
			seal.OrderingMAC = [32]byte{13}
			if _, err = f.selected.Publisher.Publish(ctx, seal); err != nil {
				t.Fatal(err)
			}
			a := f.selected.Authority
			a.Role = seal.Role
			a.Cluster = seal.Cluster
			a.Member = seal.Member
			a.Term = seal.Term
			a.OrderingIndex = seal.OrderingIndex
			a.OrderingOffset = seal.OrderingOffset
			a.OrderingMAC = seal.OrderingMAC
			options := f.options
			options.Replica = replica
			options.MaxDiskBytes = 64 << 20
			calls := 0
			if !replica {
				options.BeforeDurable = func(DurableBatch) (AppendAdmission, error) { calls++; return nil, nil }
				options.AfterDurable = func(DurableBatch) error { return nil }
			}
			head := f.cache.Head()
			n, _, err := OpenWithHASelectedSeal(ctx, f.path, f.key, options, HASelectedSealOpenOptions{SelectedSealOpenOptions: f.selected, WithAuthority: func(ctx context.Context, visit func(context.Context, authenticatedstate.SealEligibility, HASelectedNativePosition) error) error {
				return visit(ctx, a, HASelectedNativePosition{Sequence: head.Records, Hash: head.LastHash})
			}})
			if err != nil {
				t.Fatal(err)
			}
			f.native = n
			if err = n.PrepareHAPhysicalNative(ctx, budget, HAPhysicalOptions{}); !errors.Is(err, ErrStartupAuditPending) {
				t.Fatal("pending historical audit accepted allocation", err)
			}
			job, err := n.StartSelectedStartupAudit(ctx, budget, StartupAuditOptions{Deadline: 2 * time.Second, ProgressTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if err = job.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			nativeDisk, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			defer nativeDisk.Release()
			controls, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: nativephysical.ControlDiskBytes})
			if err != nil {
				t.Fatal(err)
			}
			defer controls.Release()
			if err = n.PrepareHAPhysicalNative(ctx, budget, HAPhysicalOptions{NativeDisk: nativeDisk, ControlDisk: controls, WindowBytes: 16 << 20}); err != nil {
				t.Fatal(err)
			}
			before := budget.Snapshot()
			filler, err := os.OpenFile(filepath.Join(filepath.Dir(f.path), "foreign-full"), os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_SYNC, 0600)
			if err != nil {
				t.Fatal(err)
			}
			chunk := make([]byte, 1<<20)
			size := len(chunk)
			fullRetries := 0
			for {
				_, err = filler.Write(chunk[:size])
				if err == nil {
					continue
				}
				if !errors.Is(err, syscall.ENOSPC) {
					t.Fatal(err)
				}
				if size > 4096 {
					size = 4096
					continue
				}
				if err := unix.Syncfs(int(filler.Fd())); err != nil {
					t.Fatal(err)
				}
				var now unix.Statfs_t
				if err := unix.Statfs(f.path, &now); err != nil {
					t.Fatal(err)
				}
				if now.Bavail > 0 && fullRetries < 32 {
					fullRetries++
					continue
				}
				break
			}
			var fs unix.Statfs_t
			if err = unix.Statfs(f.path, &fs); err != nil || fs.Bavail != 0 {
				t.Fatal("not actual full filesystem", fs.Bavail, err)
			}
			if err = budget.SetPressure(resourcebudget.Pressured); err != nil {
				t.Fatal(err)
			}
			event := validEvent(2, "key.create")
			if replica {
				payload, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				frames, hash := encodeFrame(f.key, 2, head.LastHash, payload)
				if err = n.AppendReplicated(2, 2, frames); err != nil {
					t.Fatal(err)
				}
				if err = n.AppendReplicated(2, 2, frames); err != nil {
					t.Fatal("durable duplicate charged a new physical slot", err)
				}
				if got := n.Summary(); got.Records != 2 || got.LastHash != hash {
					t.Fatal(got)
				}
			} else {
				record, err := n.Append(ctx, event)
				if err != nil || record.Sequence != 2 || calls != 1 {
					t.Fatal(record, calls, err)
				}
			}
			if after := budget.Snapshot(); after.Used != before.Used || after.NormalClaims != before.NormalClaims {
				t.Fatal("native fullFS write reacquired resources", before, after)
			}
			entered := make(chan struct{})
			exited := make(chan error, 1)
			go func() {
				exited <- n.withPhysicalEncoder(ctx, func(job context.Context) error { close(entered); <-job.Done(); return job.Err() })
			}()
			<-entered
			waiting := make(chan error, 1)
			go func() {
				waiting <- n.withPhysicalEncoder(ctx, func(context.Context) error { t.Error("withdrawn encoder entered native IO"); return nil })
			}()
			if err = n.Close(); err != nil {
				t.Fatal(err)
			}
			if err := <-exited; !errors.Is(err, context.Canceled) {
				t.Fatal("Close did not cancel actual active encoder", err)
			}
			if err := <-waiting; err == nil {
				t.Fatal("Close did not withdraw waiting encoder")
			}
			if err = filler.Close(); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(filler.Name()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
