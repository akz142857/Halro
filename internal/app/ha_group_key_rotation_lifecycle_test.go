package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func groupRotationFileManager(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	cfg := config.Default()
	b := selectedRuntimeBudgetFixture()
	cfg.ResourceBudget = &b
	return functionalHAMetadataManager(t, cfg)
}

func TestHAGroupRotationActualCreatorCloseUnknownRetainsAllFees(t *testing.T) {
	for _, which := range []string{"file", "parent"} {
		t.Run(which, func(t *testing.T) {
			m := groupRotationFileManager(t)
			path := filepath.Join(t.TempDir(), "candidate")
			unknown := errors.New("actual creator close completion unknown")
			calls := 0
			x, err := createHAGroupRotationFileWithClose(context.Background(), path, []byte("durable owned content"), m, func(f *os.File) error {
				isParent := f.Name() == filepath.Dir(path)
				calls++
				actual := ownedfile.CloseFile(f)
				if isParent == (which == "parent") {
					return errors.Join(actual, unknown)
				}
				return actual
			})
			var owner *haGroupRotationFileFailure
			if x == nil || !errors.As(err, &owner) || owner.Owner != x || !errors.Is(err, unknown) || !errors.Is(err, ownedfile.ErrRetirement) || calls != 2 {
				t.Fatal("lost actual unknown owner", err, calls)
			}
			before := m.Snapshot()
			if before.Classes[resourcebudget.IO].RAM != 4*haGroupRotationIntentLimit || before.Classes[resourcebudget.Recovery].Disk != haGroupRotationIntentLimit {
				t.Fatal("refunded unknown bytes/FD work", before)
			}
			for range 2 {
				if !errors.Is(x.Close(), unknown) {
					t.Fatal("sticky close debt lost")
				}
			}
			if m.Snapshot() != before || calls != 2 {
				t.Fatal("unknown actual Close retried/refunded", calls, m.Snapshot())
			}
			if raw, err := os.ReadFile(path); err != nil || string(raw) != "durable owned content" {
				t.Fatal("owned bytes lost", err)
			}
		})
	}
}
func TestHAGroupRotationCreatorCancellationAndForeignTargetPreserve(t *testing.T) {
	m := groupRotationFileManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(t.TempDir(), "cancelled")
	if _, err := createHAGroupRotationFile(ctx, path, []byte("unused"), m); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled before create wrote bytes", err)
	}
	if stats := m.Snapshot(); stats.Classes[resourcebudget.IO].RAM != 0 || stats.Classes[resourcebudget.Recovery].Disk != 0 {
		t.Fatal("known noncreate debt", stats)
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("original destination"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	x, err := createHAGroupRotationFile(context.Background(), source, []byte("actual candidate"), m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, target+".preserved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("foreign target"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := x.publishCaptured(context.Background(), target, before); !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal("foreign target overwritten", err)
	}
	for path, want := range map[string]string{source: "actual candidate", target: "foreign target", target + ".preserved": "original destination"} {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != want {
			t.Fatal("foreign/source bytes changed", path, err)
		}
	}
	if stats := m.Snapshot(); stats.Classes[resourcebudget.IO].RAM != 0 || stats.Classes[resourcebudget.Recovery].Disk != haGroupRotationIntentLimit {
		t.Fatal("persistent candidate refunded", stats)
	}
}
func TestHAGroupRotationIntentReadUnknownKeepsKeyAndOriginalWorkspace(t *testing.T) {
	m := groupRotationFileManager(t)
	dir := t.TempDir()
	path := haGroupRotationIntentPath(dir, 0)
	raw := bytes.Repeat([]byte{21}, 128)
	x, err := createHAGroupRotationFile(context.Background(), path, raw, m)
	if err != nil {
		t.Fatal(err)
	}
	work, err := haGroupRotationIntentWorkspace(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	i := &haGroupRotationIntent{Count: 2, OldKey: bytes.Repeat([]byte{13}, 32), NewKey: bytes.Repeat([]byte{14}, 32)}
	i.Members[0].DataDir = dir
	r := &haGroupFileRotation{intent: i, intentDigest: sha256.Sum256(raw), slot: 0, work: work}
	r.intentNames[0] = info
	r.files[0][0] = x
	unknown := errors.New("actual intent FD completion unknown")
	calls := 0
	r.readIntent = func(path string) ([]byte, os.FileInfo, error) {
		return readHAGroupRotationBytesWithClose(path, func(f *os.File) error {
			calls++
			actual := ownedfile.CloseFile(f)
			if f.Name() == path {
				return errors.Join(actual, unknown)
			}
			return actual
		})
	}
	if err := r.checkIntent(context.Background(), 0); !errors.Is(err, unknown) || !errors.Is(err, ownedfile.ErrRetirement) {
		t.Fatal(err)
	}
	before := m.Snapshot()
	for range 2 {
		if !errors.Is(r.Close(), unknown) {
			t.Fatal("unknown original work cleared")
		}
	}
	if calls != 2 || m.Snapshot() != before || r.work == (resourcebudget.Permit{}) || i.OldKey[0] != 13 || i.NewKey[0] != 14 {
		t.Fatal("reader/key/fees retired on unknown", calls, m.Snapshot())
	}
}
