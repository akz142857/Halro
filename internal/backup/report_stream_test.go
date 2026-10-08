package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/ownedfile"
)

type backupReportSink struct {
	bytes.Buffer
	max           int
	verifyRetired func() error
}

func (s *backupReportSink) Write(p []byte) (int, error) {
	if len(p) > s.max {
		s.max = len(p)
	}
	if s.verifyRetired != nil {
		if err := s.verifyRetired(); err != nil {
			return 0, err
		}
	}
	return s.Buffer.Write(p)
}

type shortBackupReportSink struct{}

func (shortBackupReportSink) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestBackupReportStreamsCompleteInventoryAfterRetirement(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	for _, paged := range []bool{false, true} {
		options := manifestLimitOptions(t)
		for n := 0; n < 800; n++ {
			options.Files = append(options.Files, SourceFile{ArchivePath: fmt.Sprintf("data/extra/%04d", n), LocalPath: options.Files[0].LocalPath})
		}
		if paged {
			if _, err := CreatePaged(options); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := Create(options); err != nil {
				t.Fatal(err)
			}
		}
		sink := &backupReportSink{verifyRetired: func() error {
			files, err := filepath.Glob(filepath.Join(scratch, "halro-backup-observation_*.db"))
			if err != nil {
				return err
			}
			if len(files) != 0 {
				return errors.New("native observation owner survived output")
			}
			return nil
		}}
		if err := WriteVerifiedReport(context.Background(), options.OutputPath, options.BackupKey, sink); err != nil {
			t.Fatal(err)
		}
		if sink.max > 32<<10 || sink.Len() <= 32<<10 {
			t.Fatal(sink.max, sink.Len())
		}
		var report struct {
			ArchiveSummary
			Files []File `json:"files"`
		}
		if err := json.Unmarshal(sink.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Files) != 804 || report.Catalog.Files != 804 {
			t.Fatal(len(report.Files), report.Catalog)
		}
		for _, file := range report.Files {
			if file.Size != int64(len("native fact")) {
				t.Fatal(file)
			}
		}
		files, err := os.ReadDir(scratch)
		if err != nil || len(files) != 0 {
			t.Fatal(files, err)
		}
	}
}
func TestBackupReportFailuresAndExpectedRoot(t *testing.T) {
	options := manifestLimitOptions(t)
	expected, err := CreatePaged(options)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	wrong := expected
	wrong.Catalog.Files++
	if err := WriteVerifiedReportForSummary(context.Background(), options.OutputPath, options.BackupKey, wrong, &output); err == nil || output.Len() != 0 {
		t.Fatal("changed root was reported", err)
	}
	if err := WriteVerifiedReport(context.Background(), options.OutputPath, options.BackupKey, shortBackupReportSink{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WriteVerifiedReport(ctx, options.OutputPath, options.BackupKey, &output); !errors.Is(err, context.Canceled) || output.Len() != 0 {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(options.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(options.OutputPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteVerifiedReport(context.Background(), options.OutputPath, options.BackupKey, &output); err == nil || output.Len() != 0 {
		t.Fatal("damaged archive emitted report", err)
	}
}
func TestVerifiedManifestDigestPreservesLegacyAndBindsPagedRoot(t *testing.T) {
	for _, paged := range []bool{false, true} {
		options := manifestLimitOptions(t)
		var expected []byte
		if paged {
			root, err := CreatePaged(options)
			if err != nil {
				t.Fatal(err)
			}
			expected, err = json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
		} else {
			m, err := Create(options)
			if err != nil {
				t.Fatal(err)
			}
			expected, err = json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
		}
		h, err := OpenVerified(context.Background(), options.OutputPath, options.BackupKey)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := h.ManifestDigest()
		if err != nil {
			h.Close()
			t.Fatal(err)
		}
		sum := sha256.Sum256(expected)
		if digest != hex.EncodeToString(sum[:]) {
			t.Fatal("canonical manifest digest changed")
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

type replacingBackupReportSink struct {
	directory string
	cancel    context.CancelFunc
	path      string
}

func (s *replacingBackupReportSink) Write(p []byte) (int, error) {
	paths, err := filepath.Glob(filepath.Join(s.directory, "halro-backup-report_*.json"))
	if err != nil || len(paths) != 1 {
		return 0, errors.Join(errors.New("owned report spool missing"), err)
	}
	s.path = paths[0]
	if err := os.Rename(s.path, s.path+".old"); err != nil {
		return 0, err
	}
	if err := os.WriteFile(s.path, []byte("foreign report"), 0600); err != nil {
		return 0, err
	}
	s.cancel()
	return len(p), nil
}
func TestBackupReportCanceledOutputPreservesReplacement(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	options := manifestLimitOptions(t)
	if _, err := CreatePaged(options); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &replacingBackupReportSink{directory: scratch, cancel: cancel}
	err := WriteVerifiedReport(ctx, options.OutputPath, options.BackupKey, sink)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ownedfile.ErrReplaced) {
		t.Fatal(err)
	}
	raw, readErr := os.ReadFile(sink.path)
	if readErr != nil || string(raw) != "foreign report" {
		t.Fatal(string(raw), readErr)
	}
	if err := os.Remove(sink.path + ".old"); err != nil {
		t.Fatal(err)
	}
}
