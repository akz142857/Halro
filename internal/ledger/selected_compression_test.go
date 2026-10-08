//go:build linux || darwin

package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSelectedCompressionActualSIGKILLReopensSignedPlaintext(t *testing.T) {
	for _, phase := range []string{"compact-artifact-published", "compact-manifest-published", "compact-index-published", "compact-plain-retired"} {
		t.Run(phase, func(t *testing.T) {
			f := newSelectedStartupFixture(t, true)
			want, hash, _ := f.native.ChainHead()
			selectedMaintenanceCrash(t, f, "compact", phase)
			r := f.reopen(t)
			if r.DecodedRecords != 0 || r.Head.Head != want || r.Head.Hash != hash {
				t.Fatal("physical transition changed selected facts", r)
			}
			if err := f.native.AuditSelectedStartup(context.Background(), f.o.DirectoryOptions.Budget); err != nil {
				t.Fatal(err)
			}
			if _, err := f.native.Compact(1); err != nil {
				t.Fatal("actual qualified compaction cannot complete", err)
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(f.path), "ledger-1.wal")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("actual joined duplicate not retired", err)
			}
		})
	}
}

func TestSelectedCompressionRejectsCorruptGzipBeforeActiveRepair(t *testing.T) {
	f := newSelectedStartupFixture(t, true)
	if _, err := f.native.Compact(1); err != nil {
		t.Fatal(err)
	}
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(f.path)
	m, err := loadSegmentManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	segment := &m.Segments[0]
	path := filepath.Join(dir, segment.File)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	segment.StoredChecksum = hex.EncodeToString(sum[:])
	if err := saveSegmentManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	before = append(before, 1, 2, 3)
	if err := os.WriteFile(f.path, before, 0600); err != nil {
		t.Fatal(err)
	}
	n, _, err := OpenWithSelectedSeal(context.Background(), f.path, NewStatus(), Options{ChainKey: testChainKey}, f.o)
	if n != nil || err == nil {
		t.Fatal("raw checksum credited corrupt gzip", n, err)
	}
	actual, err := os.ReadFile(f.path)
	if err != nil || !bytes.Equal(before, actual) {
		t.Fatal("corrupt compacted history authorized partial-tail repair", err)
	}
}

func TestSelectedCompressionRefreshesSeventyPhysicalDescriptorsInFinitePages(t *testing.T) {
	ctx := context.Background()
	f := newSelectedStartupGenerationsFixture(t, true, false, 70)
	seal, found, err := f.o.Publisher.Select(f.o.Authority)
	if err != nil || !found {
		t.Fatal(err)
	}
	ref := AuthenticatedDirectoryReference{Root: seal.Roots[1], Position: seal.Positions[0], Schema: authenticatedDirectorySchema}
	for generation := uint64(1); generation < ref.Position.Generation; generation++ {
		if _, err := f.native.Compact(generation); err != nil {
			t.Fatal(generation, err)
		}
	}
	f.reopen(t)
	if err := f.native.AuditSelectedStartup(ctx, f.o.DirectoryOptions.Budget); err != nil {
		t.Fatal(err)
	}
	selected := SelectedLedgerProjectionOpenOptions{Publisher: f.o.Publisher, Guard: f.o.Guard, Authority: f.o.Authority}
	d, err := OpenSelectedAuthenticatedDirectory(ctx, f.native, ref, f.o.Directory, f.o.DirectoryOptions, selected)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
		if err := f.creator.AdoptClosedSuccessor(d); err != nil {
			t.Error(err)
		}
	}()
	current, err := d.SealReference(ctx)
	if err != nil || current.Root.State.Revision != ref.Root.State.Revision+2 || current.Position != ref.Position {
		t.Fatal("physical refresh did not use two finite pages at unchanged native cut", current, err)
	}
	for generation := uint64(1); generation < ref.Position.Generation; generation++ {
		segment, found, err := d.ReadSegment(ctx, generation)
		if err != nil || !found || !segment.Compressed {
			t.Fatal("unrefreshed physical locator", generation, segment, err)
		}
	}
	actual, found, err := f.o.Publisher.Select(f.o.Authority)
	if err != nil || !found || actual != seal {
		t.Fatal("physical refresh silently changed common selection", err)
	}
}
