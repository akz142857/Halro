package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func verifiedFileBudget(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	l := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 64 << 20, Disk: 64 << 20}, MaxClaims: 16, MaxOwners: 4, MaxPressureAge: time.Hour}
	for i := range l.Classes {
		l.Classes[i] = l.Total
	}
	l.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	l.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 16 * resourcebudget.ClaimRAMBytes}
	m, err := resourcebudget.New(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return m
}

func verifiedFilePrivateDir(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func verifiedFileArchive(t *testing.T, paged bool) (*VerifiedArchive, CreateOptions) {
	t.Helper()
	o := manifestLimitOptions(t)
	// The selected metadata occurs before more than two encrypted chunks of
	// other payloads. Corruption after it must prevent any usable source owner.
	large := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(large, bytes.Repeat([]byte("a"), 2*chunkSize+17), 0600); err != nil {
		t.Fatal(err)
	}
	o.Files = append(o.Files, SourceFile{ArchivePath: "data/z-other", LocalPath: large})
	if paged {
		if _, err := CreatePaged(o); err != nil {
			t.Fatal(err)
		}
	} else if _, err := Create(o); err != nil {
		t.Fatal(err)
	}
	h, err := OpenVerified(context.Background(), o.OutputPath, o.BackupKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	return h, o
}

func TestVerifiedFileSelectsOnlyAuthenticatedPayloadAndOwnsPhysicalRetirement(t *testing.T) {
	for _, paged := range []bool{false, true} {
		name := "legacy"
		if paged {
			name = "paged"
		}
		t.Run(name, func(t *testing.T) {
			h, _ := verifiedFileArchive(t, paged)
			m := verifiedFileBudget(t)
			dir := verifiedFilePrivateDir(t)
			v, err := h.OpenVerifiedFile(context.Background(), "data/metadata.db", filepath.Join(dir, "metadata.db"), 1<<20, m)
			if err != nil || v == nil {
				t.Fatal(v, err)
			}
			t.Cleanup(func() {
				if err := v.Discard(); err != nil {
					t.Error(err)
				}
			})
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			if err := v.WithReadOnly(context.Background(), func(ctx context.Context, f *os.File, fact File) error {
				raw, err := io.ReadAll(f)
				if err != nil || string(raw) != "native fact" || fact.Path != "data/metadata.db" || fact.Size != 11 {
					t.Fatalf("%q %+v %v", raw, fact, err)
				}
				if m.Snapshot().Classes[resourcebudget.IO].RAM != VerifiedFileRAMBytes {
					t.Fatal("consumer has no IO fee")
				}
				if _, err := f.Write([]byte("untrusted")); err == nil {
					t.Fatal("borrowed source is writable")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "metadata.db" {
				t.Fatal(entries, err)
			}
			info, err := os.Stat(v.path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal(info, err)
			}
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			if m.Snapshot().Used.Disk != 11 || m.Snapshot().Used.RAM != resourcebudget.ClaimRAMBytes {
				t.Fatal("Close refunded physical file", m.Snapshot())
			}
			if err := v.WithReadOnly(context.Background(), func(context.Context, *os.File, File) error { t.Fatal("closed source consumer ran"); return nil }); err == nil {
				t.Fatal("closed source usable")
			}
			if err := v.Discard(); err != nil {
				t.Fatal(err)
			}
			if m.Snapshot().Used != (resourcebudget.Cost{}) || m.Snapshot().Owners != 0 {
				t.Fatal(m.Snapshot())
			}
			if _, err := os.Stat(v.path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}
}

func TestVerifiedFileActualCloseDebtRetainsSelectedInodeAndDisk(t *testing.T) {
	h, _ := verifiedFileArchive(t, true)
	m := verifiedFileBudget(t)
	v, err := h.OpenVerifiedFile(context.Background(), "data/metadata.db", filepath.Join(verifiedFilePrivateDir(t), "metadata.db"), 1<<20, m)
	if err != nil {
		t.Fatal(err)
	}
	before := m.Snapshot().Used
	if err := v.file.Close(); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		if err := v.Close(); !errors.Is(err, ownedfile.ErrRetirement) {
			t.Fatal("selected source close debt disappeared", err)
		}
		if err := v.Discard(); !errors.Is(err, ownedfile.ErrRetirement) {
			t.Fatal("discard erased selected source debt", err)
		}
		if m.Snapshot().Used != before {
			t.Fatal("failed actual close refunded selected source", before, m.Snapshot())
		}
	}
	if _, err := os.Stat(v.path); err != nil {
		t.Fatal("unknown selected inode removed", err)
	}
}

func TestVerifiedFileRejectsChangedFullArchiveBeforeSourceExposure(t *testing.T) {
	for _, damage := range []string{"source replacement", "authenticated rewrite", "unselected suffix", "ending"} {
		t.Run(damage, func(t *testing.T) {
			h, o := verifiedFileArchive(t, true)
			info, err := os.Stat(o.OutputPath)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(o.OutputPath)
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "source replacement":
				other := o.OutputPath + ".other"
				if err := os.WriteFile(other, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(other, o.OutputPath); err != nil {
					t.Fatal(err)
				}
			case "authenticated rewrite":
				other := o
				other.OutputPath += ".other"
				other.Build.Version = "xxxx" // same encoded length as "test"
				if _, err := CreatePaged(other); err != nil {
					t.Fatal(err)
				}
				raw, err = os.ReadFile(other.OutputPath)
				if err != nil || int64(len(raw)) != info.Size() {
					t.Fatal(len(raw), info.Size(), err)
				}
				if err := os.WriteFile(o.OutputPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
			default:
				position := len(raw) - 1
				if damage == "unselected suffix" {
					position = 2 * chunkSize
				}
				raw[position] ^= 1
				if err := os.WriteFile(o.OutputPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if damage != "source replacement" {
				if err := os.Chtimes(o.OutputPath, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			m := verifiedFileBudget(t)
			path := filepath.Join(verifiedFilePrivateDir(t), "metadata.db")
			v, err := h.OpenVerifiedFile(context.Background(), "data/metadata.db", path, 1<<20, m)
			if err == nil || v != nil {
				t.Fatal("damaged archive returned source", v, err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed derivative leaked", err)
			}
			if m.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal("failed owner fee leaked", m.Snapshot())
			}
		})
	}
}

func TestVerifiedFileAdmissionRefusesBeforeCreatingOrReplacingOutput(t *testing.T) {
	h, _ := verifiedFileArchive(t, false)
	for _, shape := range []string{"missing", "oversize", "unsafe", "relative", "nil budget", "pressure", "zero disk", "canceled", "existing", "public parent"} {
		t.Run(shape, func(t *testing.T) {
			m := verifiedFileBudget(t)
			path, name, limit := filepath.Join(verifiedFilePrivateDir(t), "metadata.db"), "data/metadata.db", int64(1<<20)
			ctx := context.Background()
			switch shape {
			case "missing":
				name = "data/missing"
			case "oversize":
				limit = 10
			case "unsafe":
				name = "../data/metadata.db"
			case "relative":
				path = "metadata.db"
			case "nil budget":
				m = nil
			case "pressure":
				if err := m.SetPressure(resourcebudget.Pressured); err != nil {
					t.Fatal(err)
				}
			case "zero disk":
				reservation, err := m.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 64 << 20})
				if err != nil {
					t.Fatal(err)
				}
				defer reservation.Release()
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "existing":
				if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			case "public parent":
				if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(filepath.Dir(path), 0700)
			}
			var before resourcebudget.Stats
			if m != nil {
				before = m.Snapshot()
			}
			v, err := h.OpenVerifiedFile(ctx, name, path, limit, m)
			if err == nil || v != nil {
				t.Fatal(v, err)
			}
			if shape == "existing" {
				raw, err := os.ReadFile(path)
				if err != nil || string(raw) != "foreign" {
					t.Fatal(string(raw), err)
				}
			} else if filepath.IsAbs(path) {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("refused operation created file", err)
				}
			}
			if m != nil && (m.Snapshot().Used != before.Used || m.Snapshot().Owners != before.Owners) {
				t.Fatal("refusal retained fee", before, m.Snapshot())
			}
		})
	}
}

func TestVerifiedFileRefusesTamperedOrReplacedDerivativeAndPreservesForeignPath(t *testing.T) {
	for _, damage := range []string{"inode", "content", "parent", "consumer mutation"} {
		t.Run(damage, func(t *testing.T) {
			h, _ := verifiedFileArchive(t, true)
			m := verifiedFileBudget(t)
			parent := filepath.Join(t.TempDir(), "private")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "metadata.db")
			v, err := h.OpenVerifiedFile(context.Background(), "data/metadata.db", path, 1<<20, m)
			if err != nil {
				t.Fatal(err)
			}
			original := path + ".original"
			if damage == "inode" {
				if err := os.Rename(path, original); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if damage == "parent" {
				if err := os.Rename(parent, parent+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(parent, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if damage == "content" {
				if err := os.WriteFile(path, []byte("other bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			err = v.WithReadOnly(context.Background(), func(context.Context, *os.File, File) error {
				called = true
				if damage != "consumer mutation" {
					t.Fatal("damaged source consumed")
				}
				return os.WriteFile(path, []byte("other bytes"), 0600)
			})
			if err == nil || called != (damage == "consumer mutation") {
				t.Fatal(called, err)
			}
			if damage == "inode" || damage == "parent" {
				if err := v.Discard(); !errors.Is(err, ownedfile.ErrReplaced) {
					t.Fatal("foreign deletion permitted", err)
				}
				raw, err := os.ReadFile(path)
				if err != nil || string(raw) != "foreign" || m.Snapshot().Used.Disk != 11 {
					t.Fatal(string(raw), m.Snapshot(), err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if damage == "inode" {
					err = os.Rename(original, path)
				} else {
					if err := os.Remove(parent); err != nil {
						t.Fatal(err)
					}
					err = os.Rename(parent+".original", parent)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := v.Discard(); err != nil {
				t.Fatal(err)
			}
			if m.Snapshot().Used != (resourcebudget.Cost{}) {
				t.Fatal(m.Snapshot())
			}
		})
	}
}

func TestVerifiedFileCloseCancelsAndJoinsConsumerBeforeRefund(t *testing.T) {
	h, _ := verifiedFileArchive(t, true)
	m := verifiedFileBudget(t)
	v, err := h.OpenVerifiedFile(context.Background(), "data/metadata.db", filepath.Join(verifiedFilePrivateDir(t), "metadata.db"), 1<<20, m)
	if err != nil {
		t.Fatal(err)
	}
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	consumerDone := make(chan error, 1)
	go func() {
		consumerDone <- v.WithReadOnly(context.Background(), func(ctx context.Context, _ *os.File, _ File) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return ctx.Err()
		})
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- v.Close() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not revoke consumer")
	}
	select {
	case err := <-closed:
		t.Fatal("Close did not join consumer", err)
	default:
	}
	if m.Snapshot().Used.Disk != 11 || m.Snapshot().Classes[resourcebudget.IO].RAM != VerifiedFileRAMBytes {
		t.Fatal("fee refunded while consumer active", m.Snapshot())
	}
	close(release)
	if err := <-consumerDone; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := v.Discard(); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal(m.Snapshot())
	}
}

func TestVerifiedFileConsumerErrorAndPanicRetireRAMWithoutGrantingPublish(t *testing.T) {
	h, _ := verifiedFileArchive(t, false)
	m := verifiedFileBudget(t)
	v, err := h.OpenVerifiedFile(context.Background(), "data/metadata.db", filepath.Join(verifiedFilePrivateDir(t), "metadata.db"), 1<<20, m)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Discard()
	want := errors.New("candidate refused")
	if err := v.WithReadOnly(context.Background(), func(context.Context, *os.File, File) error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() != want {
				t.Fatal("panic lost")
			}
		}()
		_ = v.WithReadOnly(context.Background(), func(context.Context, *os.File, File) error { panic(want) })
	}()
	if m.Snapshot().Classes[resourcebudget.IO] != (resourcebudget.Cost{}) || m.Snapshot().Used.Disk != 11 {
		t.Fatal(m.Snapshot())
	}
	// Neither an error nor a caller panic authorizes publication; a subsequent
	// independent consumer must still validate this original source in full.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := v.WithReadOnly(context.Background(), func(context.Context, *os.File, File) error { return nil }); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
}
