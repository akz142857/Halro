package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
)

type outputDiskTestToken struct{ retired int }

func (t *outputDiskTestToken) RetireKnown() error { t.retired++; return nil }

func TestEncryptedOutputAdmissionPrecedesInodeAndBoundedWrites(t *testing.T) {
	for _, outcome := range []string{"reject", "write-limit", "success"} {
		t.Run(outcome, func(t *testing.T) {
			options := manifestLimitOptions(t)
			options.OutputDiskBytes = 64 << 10
			if outcome == "write-limit" {
				options.OutputDiskBytes = headerSize + recordHeader
			}
			reject := errors.New("shared encrypted-output credit refused")
			token := &outputDiskTestToken{}
			options.ReserveOutputDisk = func(bytes int64) (ledger.NativeFileDiskReservation, error) {
				if bytes != options.OutputDiskBytes {
					t.Fatal("output did not prepay entire ceiling", bytes)
				}
				if outcome == "reject" {
					return nil, reject
				}
				return token, nil
			}
			_, err := Create(options)
			if outcome == "success" {
				if err != nil || token.retired != 0 {
					t.Fatal("published encrypted output refunded fee", err, token.retired)
				}
				if _, err := Verify(options.OutputPath, options.BackupKey); err != nil {
					t.Fatal("bounded ciphertext failed actual verification", err)
				}
			} else {
				want := reject
				if outcome == "write-limit" {
					want = ErrOutputDiskLimit
				}
				if !errors.Is(err, want) {
					t.Fatal("unfunded encrypted output accepted", err)
				}
				if _, err := os.Stat(options.OutputPath); !os.IsNotExist(err) {
					t.Fatal("failed output published", err)
				}
				if outcome == "write-limit" && token.retired != 1 {
					t.Fatal("known-created ciphertext did not physically retire before reuse", token.retired)
				}
			}
			partials, err := filepath.Glob(filepath.Join(filepath.Dir(options.OutputPath), ".halro-backup-*.tmp"))
			if err != nil || len(partials) != 0 {
				t.Fatal("definite refusal/known retirement left private output", partials, err)
			}
		})
	}
}

func TestEncryptedOutputUnknownActualSourceCloseRetainsBothIOOwners(t *testing.T) {
	options := manifestLimitOptions(t)
	token := &outputDiskTestToken{}
	options.OutputDiskBytes = 64 << 10
	options.ReserveOutputDisk = func(int64) (ledger.NativeFileDiskReservation, error) { return token, nil }
	output, err := newArchiveOutput(options)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := newEncryptWriter(output, options.BackupKey)
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewWriter(encrypted)
	var actual *os.File
	injected := errors.New("actual source Close outcome unknown")
	row, err := writeSourceWithClose(context.Background(), archive, options.Files[0], func(file *os.File) error {
		actual = file
		return ownedfile.CloseError(file, injected)
	})
	if actual == nil || !errors.Is(err, injected) || !errors.Is(err, ownedfile.ErrRetirement) || row != (File{}) {
		t.Fatal("uncertain source published authenticated archive row", row, err)
	}
	defer actual.Close()
	var raw [11]byte
	if _, err := actual.ReadAt(raw[:], 0); err != nil || string(raw[:]) != "native fact" {
		t.Fatal("actual uncertain source IO no longer owned", err)
	}
	err = output.finish(errors.Join(err, archive.Close(), encrypted.Close()))
	var debt *archiveOutputDebt
	if !errors.As(err, &debt) || debt.output != output || token.retired != 0 {
		t.Fatal("unknown source exit refunded independent output", err)
	}
	if _, err := os.Stat(output.file.Name()); err != nil {
		t.Fatal("uncertain output removed before all source exits", err)
	}
}

func TestEncryptedOutputUnknownCloseNameAndPublishedBarrierRetainOwner(t *testing.T) {
	for _, fault := range []string{"actual-close", "temp-replaced", "published-sync"} {
		t.Run(fault, func(t *testing.T) {
			options := manifestLimitOptions(t)
			token := &outputDiskTestToken{}
			options.OutputDiskBytes = 64 << 10
			options.ReserveOutputDisk = func(int64) (ledger.NativeFileDiskReservation, error) { return token, nil }
			output, err := newArchiveOutput(options)
			if err != nil {
				t.Fatal(err)
			}
			name := output.file.Name()
			if _, err := output.Write([]byte("actual staged ciphertext")); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("actual producer failed")
			cause := injected
			switch fault {
			case "actual-close":
				if err := output.file.Close(); err != nil {
					t.Fatal(err)
				}
			case "temp-replaced":
				if err := os.Rename(name, name+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			case "published-sync":
				if err := output.closeFile(); err != nil {
					t.Fatal(err)
				}
				cause = output.publishWithSync(options.OutputPath, func(string) error { return injected })
			}
			err = output.finish(cause)
			var debt *archiveOutputDebt
			if !errors.Is(err, ownedfile.ErrRetirement) || !errors.As(err, &debt) || debt.output != output || debt.output.token != token || token.retired != 0 {
				t.Fatal("uncertain ciphertext exit lost actual owner/token", err, token.retired)
			}
			if fault == "actual-close" {
				if !errors.Is(err, os.ErrClosed) || debt.output.file != output.file {
					t.Fatal("actual failed Close descriptor lost", err)
				}
			}
			if fault == "temp-replaced" {
				if actual, err := os.ReadFile(name); err != nil || !bytes.Equal(actual, []byte("foreign")) {
					t.Fatal("foreign output removed", err)
				}
				if _, err := os.Stat(name + ".retained"); err != nil {
					t.Fatal("original renamed ciphertext removed", err)
				}
			}
			if fault == "published-sync" {
				if _, err := os.Stat(options.OutputPath); err != nil {
					t.Fatal("uncertain published authority removed", err)
				}
			}
		})
	}
}

func TestEncryptedOutputLateForeignPublicationNeverOverwritesOrRefundsUnknown(t *testing.T) {
	options := manifestLimitOptions(t)
	token := &outputDiskTestToken{}
	options.OutputDiskBytes = 64 << 10
	options.ReserveOutputDisk = func(int64) (ledger.NativeFileDiskReservation, error) { return token, nil }
	output, err := newArchiveOutput(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := output.closeFile(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(options.OutputPath, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	err = output.finish(output.publish(options.OutputPath))
	if err == nil || token.retired != 1 {
		t.Fatal("failed exclusive publication did not retire only its own temp", err, token.retired)
	}
	if actual, err := os.ReadFile(options.OutputPath); err != nil || string(actual) != "foreign" {
		t.Fatal("racing publisher overwritten/removed", err)
	}
}
