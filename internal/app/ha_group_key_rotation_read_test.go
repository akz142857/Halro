package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

func TestHAGroupRotationReadActualBothCloseDebtNoBytesEscape(t *testing.T) {
	for _, failed := range []int{0, 1} {
		t.Run([]string{"parent", "file"}[failed], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "intent")
			if err := os.WriteFile(path, bytes.Repeat([]byte{31}, 4096), 0600); err != nil {
				t.Fatal(err)
			}
			unknown := errors.New("actual descriptor close completion unknown")
			var calls [2]int
			raw, info, err := readHAGroupRotationBytesWithClose(path, func(f *os.File) error {
				n := 1
				if f.Name() == filepath.Dir(path) {
					n = 0
				}
				calls[n]++
				actual := ownedfile.CloseFile(f)
				if n == failed {
					return errors.Join(actual, unknown)
				}
				return actual
			})
			var debt *haGroupRotationReadFailure
			if raw != nil || info != nil || !errors.As(err, &debt) || !errors.Is(err, ownedfile.ErrRetirement) || !errors.Is(err, unknown) || debt.files[failed] == nil || len(debt.raw) != 4096 || calls != [2]int{1, 1} {
				t.Fatalf("owner not retained: bytes=%d info=%v err=%v calls=%v", len(raw), info, err, calls)
			}
		})
	}
}
func TestHAGroupRotationReadRejectsForeignNamespaceAndExactLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "intent")
	for _, n := range []int{1, haGroupRotationIntentLimit} {
		want := bytes.Repeat([]byte{24}, n)
		if err := os.WriteFile(path, want, 0600); err != nil {
			t.Fatal(err)
		}
		got, info, err := readHAGroupRotationBytes(path)
		if err != nil || info.Size() != int64(n) || !bytes.Equal(got, want) {
			t.Fatal(n, err)
		}
		clear(got)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if raw, _, err := readHAGroupRotationBytes(path); err == nil || raw != nil {
		t.Fatal("public file escaped", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "foreign")
	if err := os.WriteFile(foreign, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, path); err != nil {
		t.Fatal(err)
	}
	if raw, _, err := readHAGroupRotationBytes(path); err == nil || raw != nil {
		t.Fatal("symlink escaped", err)
	}
	if actual, err := os.ReadFile(foreign); err != nil || string(actual) != "private" {
		t.Fatal("foreign altered", err)
	}
}
