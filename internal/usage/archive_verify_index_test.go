package usage

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveComparisonIndexConstantNodesAndAuthenticatedAbsence(t *testing.T) {
	x, err := newArchiveVerifyIndex(filepath.Join(t.TempDir(), "index"), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer x.close()
	ctx := context.Background()
	work := &archiveVerifyReadBudget{remaining: 1 << 30}
	for i := 0; i < 10000; i++ {
		key := sha256.Sum256([]byte(fmt.Sprint(i)))
		var value [40]byte
		binary.BigEndian.PutUint64(value[:8], uint64(i))
		if err := x.insert(ctx, 0, key, value, work); err != nil {
			t.Fatal(i, err)
		}
	}
	if x.size != (2*10000)*archiveVerifyNodeBytes {
		t.Fatal("unexpected disk extent", x.size)
	}
	for i := 0; i < 10000; i++ {
		v, found, err := x.lookup(ctx, 0, sha256.Sum256([]byte(fmt.Sprint(i))), work)
		if err != nil || !found || binary.BigEndian.Uint64(v[:8]) != uint64(i) {
			t.Fatal(i, found, err)
		}
	}
	if _, found, err := x.lookup(ctx, 0, sha256.Sum256([]byte("absent")), work); err != nil || found {
		t.Fatal(found, err)
	}
	if err := x.insert(ctx, 0, sha256.Sum256([]byte("5")), [40]byte{}, work); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal(err)
	}
	// The same key belongs independently to each tree; none share membership.
	for tree := 1; tree < 3; tree++ {
		if err := x.insert(ctx, tree, sha256.Sum256([]byte("5")), [40]byte{}, work); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.check(); err != nil {
		t.Fatal(err)
	}
	root := x.roots[0]
	if _, err := x.file.WriteAt([]byte{0xff}, int64(root)+64); err != nil {
		t.Fatal(err)
	}
	if _, _, err := x.lookup(ctx, 0, sha256.Sum256([]byte("absent")), work); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal("tampered node certified absence", err)
	}
}

func TestArchiveComparisonIndexWorstDepthAndPhysicalBounds(t *testing.T) {
	x, err := newArchiveVerifyIndex(filepath.Join(t.TempDir(), "index"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer x.close()
	ctx := context.Background()
	work := &archiveVerifyReadBudget{remaining: 1 << 30}
	if err := x.insert(ctx, 0, [32]byte{}, [40]byte{}, work); err != nil {
		t.Fatal(err)
	}
	for bit := 0; bit < 256; bit++ {
		var key [32]byte
		key[bit/8] = 1 << uint(7-bit%8)
		if err := x.insert(ctx, 0, key, [40]byte{}, work); err != nil {
			t.Fatal(bit, err)
		}
	}
	bounded := &archiveVerifyReadBudget{remaining: 257 * archiveVerifyNodeBytes}
	if _, found, err := x.lookup(ctx, 0, [32]byte{}, bounded); err != nil || !found || bounded.remaining != 0 {
		t.Fatal(found, bounded.remaining, err)
	}
	if _, _, err := x.lookup(ctx, 0, [32]byte{}, &archiveVerifyReadBudget{remaining: 256 * archiveVerifyNodeBytes}); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal(err)
	}
	x.max = x.size
	if err := x.insert(ctx, 0, sha256.Sum256([]byte("overflow")), [40]byte{}, work); !errors.Is(err, ErrDiskUsageLimit) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := x.lookup(ctx, 0, [32]byte{}, work); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := os.Rename(x.file.Name(), x.file.Name()+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(x.file.Name(), make([]byte, x.size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := x.check(); !errors.Is(err, ErrDiskUsageCorrupt) {
		t.Fatal(err)
	}
}
