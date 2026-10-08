//go:build linux

package authenticatedstate

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
	"golang.org/x/sys/unix"
)

func requireSealPhysicalExt4(t *testing.T, dir string) unix.Statfs_t {
	t.Helper()
	if os.Getenv("HALRO_EXT4_ROOT") == "" {
		t.Skip("isolated ext4 fixture required")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil || fs.Type != unix.EXT4_SUPER_MAGIC || fs.Blocks*uint64(fs.Bsize) > 512<<20 {
		t.Fatal("isolated bounded ext4 qualification", fs, err)
	}
	return fs
}

func exhaustSealPhysicalExt4(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "foreign-space-filler")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_SYNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	size := len(chunk)
	total := int64(0)
	for {
		n, err := file.Write(chunk[:size])
		total += int64(n)
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
		break
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	fs := requireSealPhysicalExt4(t, dir)
	if fs.Bavail != 0 {
		t.Fatal("fixture did not actually exhaust target filesystem", fs.Bavail)
	}
	t.Logf("actual foreign filler bytes=%d bavail=%d", total, fs.Bavail)
	return path
}

func TestSealPreparedPhysicalCandidatePublishesAtActualENOSPC(t *testing.T) {
	dir := t.TempDir()
	requireSealPhysicalExt4(t, dir)
	key := make([]byte, 32)
	key[0] = 9
	manager := testBudget(t)
	reservation := sealPublisherReservation(t, manager)
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	selected := sealCandidateFixture()
	hard := sealHardFor(selected)
	publisher, err := OpenSealCandidatePublisher(dir, guard, key, hard, manager, reservation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(context.Background(), selected); err != nil {
		t.Fatal(err)
	}
	currentReference, err := publisher.PrepareCurrentPhysicalRoot(context.Background(), selected)
	if err != nil {
		t.Fatal(err)
	}
	target, err := resourcebudget.PreparePhysicalCandidate(context.Background(), filepath.Join(dir, "active.seal.pending"), candidateSealMaxBytes, manager, reservation)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(dir, "active.seal.pending"))
	if err != nil {
		t.Fatal(err)
	}
	reference, err := target.Reference()
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ClosePreserving(); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Close(); err != nil {
		t.Fatal(err)
	}
	manager = testBudget(t)
	reservation = sealPublisherReservation(t, manager)
	// A fresh shared Manager reconstructs persistent target costs, as on a
	// separate-process restart; the previous owner's credit remains charged.
	// The independent caller authenticated the selected envelope and ownership
	// receipt before this restore API. Pending bytes supply no root authority.
	restored, err := resourcebudget.RestorePhysicalCandidate(context.Background(), filepath.Join(dir, "active.seal.pending"), reference, manager, reservation)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err = OpenSealCandidatePublisherPrepared(dir, guard, key, hard, manager, reservation, selected, restored)
	if err != nil {
		t.Fatal(err)
	}
	filler := exhaustSealPhysicalExt4(t, dir)
	competitor := exec.Command(os.Args[0], "-test.run", "^TestSealPhysicalForeignWriterHelper$", "-test.count=1", "-test.v")
	competitor.Env = append(os.Environ(), "HALRO_SEAL_FOREIGN_WRITER="+filler)
	output, err := competitor.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	competitor.Stderr = competitor.Stdout
	if err := competitor.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(output)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "FOREIGN_READY") {
			break
		}
	}
	time.Sleep(10 * time.Millisecond)
	next := selected
	next.Revision++
	receipt, err := publisher.PublishPreparedExchange(context.Background(), next, restored)
	if err != nil || receipt.Revision != next.Revision {
		t.Fatal(receipt, err)
	}
	after, err := os.Stat(filepath.Join(dir, "active.seal"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("publication allocated a different inode", err)
	}
	current, found, err := publisher.Select(sealEligibilityFor(next))
	if err != nil || !found || current != next {
		t.Fatal(current, found, err)
	}
	// The old active inode remains in the already-existing pending name. No
	// source block is freed between commit and rebuilding the next vector.
	oldPending, err := os.Stat(filepath.Join(dir, "active.seal.pending"))
	if err != nil {
		t.Fatal(err)
	}
	nextTarget, err := resourcebudget.RestorePhysicalCandidate(context.Background(), filepath.Join(dir, "active.seal.pending"), currentReference, manager, reservation)
	if err != nil {
		t.Fatal(err)
	}
	next.Revision++
	if _, err := publisher.PublishPreparedExchange(context.Background(), next, nextTarget); err != nil {
		t.Fatal("renewal released/reallocated old blocks", err)
	}
	actualSecond, err := os.Stat(filepath.Join(dir, "active.seal"))
	if err != nil || !os.SameFile(oldPending, actualSecond) {
		t.Fatal("second exchange did not reuse actual old root", err)
	}
	remaining, readErr := io.ReadAll(reader)
	if err := competitor.Wait(); err != nil || readErr != nil || !strings.Contains(string(remaining), "FOREIGN_NO_BLOCKS_RELEASED") {
		t.Fatal("actual concurrent foreign writer", string(remaining), err, readErr)
	}
	if fs := requireSealPhysicalExt4(t, dir); fs.Bavail != 0 {
		t.Fatal("publication freed actual target blocks", fs.Bavail)
	}
	if _, err := os.Stat(filler); err != nil {
		t.Fatal("foreign capacity was retired before actual publication", err)
	}
	if err := os.Remove(filler); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "active.seal.pending")); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Retire(); err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Release(); err != nil {
		t.Fatal(err)
	}
	// Original ClosePreserving retains its own persistent clone until its
	// exact receipt is retired; transferring candidate loans never refunds it.
}

func TestSealPhysicalForeignWriterHelper(t *testing.T) {
	path := os.Getenv("HALRO_SEAL_FOREIGN_WRITER")
	if path == "" {
		t.Skip("child only")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|syscall.O_SYNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fmt.Println("FOREIGN_READY")
	block := make([]byte, 4096)
	writes := 0
	for i := 0; i < 500; i++ {
		n, err := file.Write(block)
		if n != 0 {
			writes++
		}
		if err != nil && !errors.Is(err, syscall.ENOSPC) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if writes != 0 {
		t.Fatalf("foreign writer consumed freed publication blocks: %d", writes)
	}
	fmt.Println("FOREIGN_NO_BLOCKS_RELEASED")
}
