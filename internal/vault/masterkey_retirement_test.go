package vault

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
	"golang.org/x/sys/unix"
)

func TestMasterKeyActualCloseAndLateIdentityRevokeRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, bytes.Repeat([]byte{7}, MasterKeySize), 0600); err != nil {
		t.Fatal(err)
	}
	key, info, err := loadMasterKeyWithOpen(context.Background(), path, func(name string) (*os.File, error) {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		return f, nil
	})
	if key != nil || info != nil || !errors.Is(err, ErrMasterKeyRetirement) {
		t.Fatal("unknown Close returned key", err)
	}
	key, info, err = loadMasterKeyWithOpen(context.Background(), path, func(name string) (*os.File, error) {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(name, name+".original"); err != nil {
			f.Close()
			return nil, err
		}
		if err := os.WriteFile(name, bytes.Repeat([]byte{8}, MasterKeySize), 0600); err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	})
	if key != nil || info != nil || !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal("late named replacement returned original key", err)
	}
	if raw, err := os.ReadFile(path); err != nil || raw[0] != 8 {
		t.Fatal("foreign key removed", err)
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if key, _, err := LoadMasterKeyWithIdentity(context.Background(), fifo); err == nil || key != nil {
		t.Fatal("FIFO was read as key")
	}
}
func TestMasterKeyReplacementKeepsUnknownAndForeignFiles(t *testing.T) {
	for _, mode := range []string{"close", "target", "temporary"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "master.key")
			old := bytes.Repeat([]byte{1}, MasterKeySize)
			next := bytes.Repeat([]byte{2}, MasterKeySize)
			if err := os.WriteFile(path, old, 0600); err != nil {
				t.Fatal(err)
			}
			_, expected, err := LoadMasterKeyWithIdentity(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			var temporary string
			boom := errors.New("injected replacement stop")
			err = replaceMasterKeyCapturedContext(context.Background(), path, next, expected, func(f *os.File) error {
				temporary = f.Name()
				switch mode {
				case "close":
					if err := f.Close(); err != nil {
						return err
					}
					return boom
				case "target":
					if err := os.Rename(path, path+".original"); err != nil {
						return err
					}
					return os.WriteFile(path, bytes.Repeat([]byte{3}, MasterKeySize), 0600)
				case "temporary":
					if err := os.Rename(temporary, temporary+".escaped"); err != nil {
						return err
					}
					return os.WriteFile(temporary, []byte("foreign temporary"), 0600)
				}
				return nil
			})
			if err == nil {
				t.Fatal("uncertain/foreign replacement published")
			}
			if mode == "close" {
				if !errors.Is(err, ErrMasterKeyRetirement) || !errors.Is(err, boom) {
					t.Fatal(err)
				}
				if _, err := os.Stat(temporary); err != nil {
					t.Fatal("unknown actual temporary removed", err)
				}
			}
			if mode == "temporary" {
				if !errors.Is(err, ErrMasterKeyRetirement) {
					t.Fatal(err)
				}
				if raw, err := os.ReadFile(temporary); err != nil || string(raw) != "foreign temporary" {
					t.Fatal("foreign temporary removed", err)
				}
				if _, err := os.Stat(temporary + ".escaped"); err != nil {
					t.Fatal("renamed created inode removed", err)
				}
			}
			expectedByte := byte(1)
			if mode == "target" {
				expectedByte = 3
			}
			if raw, err := os.ReadFile(path); err != nil || raw[0] != expectedByte {
				t.Fatal("original/foreign live key changed", err)
			}
		})
	}
}

func TestMasterKeyInitializationPreservesUnknownAndForeignInodes(t *testing.T) {
	for _, mode := range []string{"close", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "master.key")
			boom := errors.New("initialization interrupted")
			err := initMasterKeyWithPhase(context.Background(), path, func(f *os.File) error {
				if mode == "close" {
					if err := f.Close(); err != nil {
						return err
					}
					return boom
				}
				if err := os.Rename(path, path+".escaped"); err != nil {
					return err
				}
				return os.WriteFile(path, []byte("foreign key"), 0600)
			})
			if !errors.Is(err, ErrMasterKeyRetirement) {
				t.Fatal("uncertain key cleanup did not retain debt", err)
			}
			if mode == "close" {
				if !errors.Is(err, boom) {
					t.Fatal("original failure lost", err)
				}
				if info, err := os.Stat(path); err != nil || info.Size() != MasterKeySize {
					t.Fatal("unknown actual key deleted", err)
				}
			} else {
				if raw, err := os.ReadFile(path); err != nil || string(raw) != "foreign key" {
					t.Fatal("foreign key deleted", err)
				}
				if _, err := os.Stat(path + ".escaped"); err != nil {
					t.Fatal("renamed created key removed", err)
				}
			}
		})
	}
}
