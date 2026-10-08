//go:build linux

package authenticatedstate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/store/lock"
)

func TestPhysicalRepackActualSIGKILLTransfersOnlyIndependentSelectedCopy(t *testing.T) {
	requirePhysicalReceiptExt4(t)
	for _, name := range []string{"before-common", "common-selected", "transfer-durable", "foreign-source", "foreign-target", "foreign-control"} {
		t.Run(name, func(t *testing.T) {
			f := newRepackFixture(t)
			f.grow(t)
			want := storeRows(t, f.store)
			dir := filepath.Dir(f.store.dir)
			nativePath := filepath.Join(dir, "native")
			if err := os.WriteFile(nativePath, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(f.store.Close(), f.publisher.Close(), f.guard.Close()); err != nil {
				t.Fatal(err)
			}
			phase := "repack-common-selected"
			if name == "before-common" {
				phase = "repack-physical-copy-qualified"
			}
			if name == "transfer-durable" {
				phase = "repack-physical-transfer-durable"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStoreRepackCrashHelper$", "-test.count=1")
			child.Env = append(os.Environ(), "HALRO_REPACK_CRASH_DIRECTORY="+dir, "HALRO_REPACK_CRASH_PHASE="+phase, "HALRO_REPACK_PHYSICAL_NATIVE="+nativePath)
			output, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || ctx.Err() != nil || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("actual SIGKILL absent: %v %s", err, output)
			}
			guard, err := lock.Acquire(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			pub, err := OpenSealCandidatePublisher(dir, guard, f.options.Key, sealHardFor(f.seal), f.options.Budget, sealPublisherReservation(t, f.options.Budget))
			if err != nil {
				t.Fatal(err)
			}
			defer pub.Close()
			selected, found, err := pub.Current()
			if err != nil || !found {
				t.Fatal(err)
			}
			var controls [2]os.FileInfo
			for i := range controls {
				controls[i], err = os.Stat(filepath.Join(f.store.dir, "terminal.physical."+string(rune('0'+i))))
				if err != nil {
					t.Fatal(err)
				}
			}
			foreignPath := ""
			if name == "foreign-source" {
				foreignPath = filepath.Join(f.store.dir, "pages")
			}
			if name == "foreign-target" {
				foreignPath = filepath.Join(f.store.dir, "pages.next")
			}
			if name == "foreign-control" {
				foreignPath = filepath.Join(f.store.dir, "terminal.physical.0")
			}
			var foreign []byte
			if foreignPath != "" {
				foreign, err = os.ReadFile(foreignPath)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(foreignPath, foreignPath+".original"); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(foreignPath, foreign, 0600); err != nil {
					t.Fatal(err)
				}
			}
			o := f.options
			o.PhysicalRecovery, o.ExpectedRoot = true, nil
			o.ExpectedCut, o.ExpectedSealRoot, o.ExpectedSealSchema = selected.Roots[0].State.Cut, &selected.Roots[0], &f.schema
			s, err := OpenStore(ctx, f.store.dir, o)
			if err != nil {
				t.Fatal(err)
			}
			f.store = s
			native, verify := repackPhysicalNativeFixture(t, nativePath)
			encoded, err := json.Marshal(selected)
			if err != nil {
				t.Fatal(err)
			}
			selection := sha256.Sum256(encoded)
			_, restoreErr := s.EnablePhysicalTerminalAfterQualifiedNoDebt(ctx, selection, native, o.ExpectedCut, verify)
			if foreignPath != "" {
				if restoreErr == nil {
					t.Fatal("foreign actual inode acquired old receipt authority")
				}
				raw, err := os.ReadFile(foreignPath)
				if err != nil || !reflect.DeepEqual(raw, foreign) {
					t.Fatal("foreign bytes changed", err)
				}
				if errors.Is(restoreErr, ownedfile.ErrReplaced) && s.physical != nil {
					f.expectedCloseErr = ownedfile.ErrReplaced
				}
				return
			}
			if restoreErr != nil {
				t.Fatal("qualified actual copied root recovery", restoreErr)
			}
			for i, info := range controls {
				now, err := os.Stat(s.receiptPath(i))
				if err != nil || !os.SameFile(info, now) {
					t.Fatal("original controls replaced", err)
				}
			}
			if got := storeRows(t, s); !reflect.DeepEqual(got, want) {
				t.Fatal("repack changed authenticated rows")
			}
			if name == "before-common" {
				c, err := s.PrepareRepack(ctx, pub, guard, selected, f.schema, f.reservation)
				if err != nil || c == nil {
					t.Fatal("old selection could not safely retry", err)
				}
				if err := c.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
