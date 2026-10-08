package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// WriteVerifiedReport prepares complete JSON inventory in an owned private
// spool, retires the archive/key/index, then writes fixed chunks. Preparation
// failure emits no bytes; output IO failure can leave a prefix and returns an
// error. The report's files array is inventory, not a stored format5 root.
func WriteVerifiedReport(ctx context.Context, path string, key []byte, destination io.Writer) error {
	return writeVerifiedReport(ctx, path, key, nil, destination)
}
func WriteVerifiedReportForSummary(ctx context.Context, path string, key []byte, expected ArchiveSummary, destination io.Writer) error {
	return writeVerifiedReport(ctx, path, key, &expected, destination)
}
func writeVerifiedReport(ctx context.Context, path string, key []byte, expected *ArchiveSummary, destination io.Writer) (err error) {
	if destination == nil {
		return errors.New("backup report destination is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h, err := OpenVerified(ctx, path, key)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, h.Close()) }()
	if expected != nil {
		wanted, err := json.Marshal(expected)
		if err != nil {
			return err
		}
		actual, err := json.Marshal(h.Summary())
		if err != nil {
			return err
		}
		if !bytes.Equal(wanted, actual) {
			return errors.New("backup changed after creation")
		}
	}
	name, err := id.New("halro-backup-report")
	if err != nil {
		return err
	}
	budget := backupWorkspace(ctx)
	release, err := budget.acquire(ctx)
	if err != nil {
		return err
	}
	var cleanupErr error
	defer func() {
		if cleanupErr == nil {
			err = errors.Join(err, release())
		}
	}()
	spool, owner, err := ownedfile.Create(filepath.Join(os.TempDir(), name+".json"), os.O_RDWR, 0600)
	if err != nil {
		if errors.Is(err, ownedfile.ErrRetirement) {
			cleanupErr = err
		}
		return err
	}
	identity, statErr := spool.Stat()
	defer func() {
		cleanupErr = errors.Join(statErr, spool.Close())
		if cleanupErr == nil {
			named, nameErr := os.Lstat(spool.Name())
			if nameErr != nil || !os.SameFile(identity, named) {
				cleanupErr = errors.Join(ownedfile.ErrReplaced, nameErr)
			} else {
				cleanupErr = owner.Remove()
			}
		}
		err = errors.Join(err, cleanupErr)
	}()
	if statErr != nil {
		return statErr
	}
	limit := int64(math.MaxInt64)
	if budget != nil {
		limit = budget.bytes
	}
	output := &backupWorkspaceWriter{writer: spool, remaining: limit}
	summary := h.Summary()
	// A shallower omitted field suppresses Manifest.Files without editing JSON.
	header, err := json.Marshal(struct {
		ArchiveSummary
		Files *json.RawMessage `json:"files,omitempty"`
	}{ArchiveSummary: summary})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "%s,\"files\":[", header[:len(header)-1]); err != nil {
		return err
	}
	var count uint64
	encoder := json.NewEncoder(output)
	if err := h.FilesEach(ctx, func(file File) error {
		if count >= summary.Catalog.Files {
			return errors.New("backup report file count changed")
		}
		if count > 0 {
			if _, err := io.WriteString(output, ","); err != nil {
				return err
			}
		}
		if err := encoder.Encode(file); err != nil {
			return err
		}
		count++
		return nil
	}); err != nil {
		return err
	}
	if count != summary.Catalog.Files {
		return errors.New("backup report file count changed")
	}
	if _, err := io.WriteString(output, "]}\n"); err != nil {
		return err
	}
	if err := h.Close(); err != nil {
		return err
	}
	info, err := spool.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(spool.Name())
	if err != nil || !os.SameFile(info, named) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var buffer [32 << 10]byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := spool.Read(buffer[:])
		if n > 0 {
			written, writeErr := destination.Write(buffer[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return ctx.Err()
		}
		if readErr != nil {
			return readErr
		}
	}
}

type backupWorkspaceWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *backupWorkspaceWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("backup report exceeds admitted workspace file ceiling")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}
