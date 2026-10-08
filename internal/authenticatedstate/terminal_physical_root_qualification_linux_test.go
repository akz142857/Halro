//go:build linux

package authenticatedstate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestPhysicalRootQualificationSIGKILLHelper(t *testing.T) {
	path := os.Getenv("HALRO_PHYSICAL_ROOT_QUALIFICATION")
	if path == "" {
		t.Skip("subprocess helper")
	}
	var f physicalReceiptCrashFixture
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	o := storeOptions(t)
	o.ExpectedRoot = &f.Root
	o.DiskReservation, err = o.Budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := OpenStore(ctx, f.Directory, o)
	if err != nil {
		t.Fatal(err)
	}
	native, verify := receiptNativeFixture(t, f.Native)
	if _, err = s.EnablePhysicalTerminalQualified(ctx, f.Selection, native, verify); err != nil {
		t.Fatal(err)
	}
	w, err := s.ReservePhysicalTerminals(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.PreparePhysicalUpdate(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UpdateNativeFrame(u.Bind(ctx), nextCut(77), func(tx *Transaction) error {
		return tx.Put([]byte("qualified-prefix"), []byte("bounded independent component fixture"))
	}, func() error { return nil })
	if err = errors.Join(err, u.Finish(true, err)); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	t.Fatal("SIGKILL returned")
}

func TestPhysicalRootQualificationReadsBoundedPrefixWithoutPromotion(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	for _, name := range []string{"qualified", "wrong-cut", "source-rejected"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			creator, o := testStoreWithoutCleanup(t, 3)
			selected, err := creator.RootIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			schema := SealRootSchema{Kind: SealRootState, Component: SealComponentUsage, IndexID: 1, Codec: 1}
			savedSeal, err := creator.SealRootReference(ctx, schema)
			if err != nil {
				t.Fatal(err)
			}
			if err = creator.Close(); err != nil {
				t.Fatal(err)
			}
			nativePath := filepath.Join(filepath.Dir(creator.dir), "actual-native")
			if err = os.WriteFile(nativePath, nil, 0600); err != nil {
				t.Fatal(err)
			}
			f := physicalReceiptCrashFixture{Directory: creator.dir, Native: nativePath, Root: selected, Selection: nextCut(99)}
			raw, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(filepath.Dir(creator.dir), "fixture.json")
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPhysicalRootQualificationSIGKILLHelper$", "-test.count=1")
			child.Env = append(os.Environ(), "HALRO_PHYSICAL_ROOT_QUALIFICATION="+path)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("not actual saved root crash: %v %s", err, output)
			}
			o.ExpectedRoot = &selected
			o.ExpectedSealRoot = &savedSeal
			o.ExpectedSealSchema = &schema
			s, err := OpenStore(ctx, creator.dir, o)
			if err != nil {
				t.Fatal(err)
			}
			native, verify := receiptNativeFixture(t, nativePath)
			calls := 0
			cut, found, qualificationErr := s.WithPhysicalRecoveryRoot(ctx, f.Selection, native, func(pin *Snapshot, comparison [32]byte, prior PhysicalNativeReference) ([32]byte, error) {
				calls++
				value, exists, err := pin.Get(ctx, []byte("qualified-prefix"))
				if err != nil || !exists || string(value) != "bounded independent component fixture" || comparison != nextCut(77) {
					return [32]byte{}, errors.Join(ErrRecovery, err)
				}
				if err := verify(prior, 0); err != nil {
					return [32]byte{}, err
				}
				if name == "source-rejected" {
					return [32]byte{}, ErrSealIneligible
				}
				if name == "wrong-cut" {
					return nextCut(78), nil
				}
				// Fixture's expected scalar/cut is fixed outside candidate bytes.
				// Actual domain callers separately verify real native anchors.
				return nextCut(77), nil
			})
			if calls != 1 {
				t.Fatal("actual bounded callback absent", calls)
			}
			identity, err := s.RootIdentity(ctx)
			if err != nil || identity != selected || s.physical != nil {
				t.Fatal("qualification promoted/borrowed root or leaked pin", identity, err)
			}
			for i := range s.pins {
				if s.pins[i].Load() != nil {
					t.Fatal("qualification leaked actual pin")
				}
			}
			if name != "qualified" {
				if qualificationErr == nil || found {
					t.Fatal("candidate comparison substituted for independent cut", found, qualificationErr)
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				return
			}
			if qualificationErr != nil || !found || cut != nextCut(77) {
				t.Fatal(found, qualificationErr)
			}
			window, _, restored, err := s.RestorePhysicalTerminalForRecovery(ctx, f.Selection, native, cut, verify)
			if err != nil || !restored || window.Count() != 1 {
				t.Fatal("qualified target restore", restored, err)
			}
			if err = window.Close(); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
