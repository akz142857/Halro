package governance

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestGovernanceNativeSourceCeilingBeforeDecodeAndLinkRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	wrapped := false
	options := Options{MaxDiskBytes: 4096, WrapDurability: func(file *os.File) DurabilityWriter { wrapped = true; return file }}
	if log, err := OpenWithOptions(path, make([]byte, 32), options); log != nil || !errors.Is(err, ErrJournalDiskLimit) || wrapped {
		t.Fatal("decoded an oversized source or constructed writer", err, wrapped)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 1<<20 {
		t.Fatal("changed rejected native bytes", info, err)
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if log, err := OpenWithOptions(link, make([]byte, 32), Options{}); err == nil || log != nil {
		t.Fatal("opened symbolic native authority", err)
	}
}
