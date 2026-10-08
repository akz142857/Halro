package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/id"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// WriteSealLedger preserves the seal command's complete JSON report without
// retaining the historical descriptor array. It prepares the whole report in
// an owned private spool after full verification; failed preparation/retirement
// emits no bytes. Destination IO can fail after partial output, so consumers
// must require both a complete JSON document and a successful command exit.
func WriteSealLedger(ctx context.Context, cfg config.Config, compress bool, destination io.Writer) (err error) {
	if destination == nil {
		return errors.New("seal report destination is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, limit, retireWorkspace, err := ledgerMaintenanceContext(ctx, cfg, true)
	if err != nil {
		return err
	}
	var spool *os.File
	var owner *ownedfile.Owner
	defer func() {
		var cleanupErr error
		if spool != nil {
			cleanupErr = spool.Close()
		}
		if cleanupErr == nil {
			cleanupErr = owner.Retire()
		}
		if cleanupErr == nil && owner != nil {
			cleanupErr = durable.SyncDirectory(filepath.Dir(spool.Name()))
		}
		err = errors.Join(err, cleanupErr, retireWorkspace(cleanupErr == nil && !errors.Is(err, errLedgerMaintenanceRetirement) && !errors.Is(err, ledger.ErrSegmentRetirement) && !errors.Is(err, ownedfile.ErrRetirement)))
	}()
	_, err = sealLedger(ctx, cfg, compress, func(report *SealReport, log *ledger.Log) error {
		name, createErr := id.New("halro-seal-report")
		if createErr != nil {
			return createErr
		}
		spool, owner, createErr = ownedfile.Create(filepath.Join(os.TempDir(), name+".json"), os.O_RDWR, 0600)
		if createErr != nil {
			return createErr
		}
		return writeSealReportJSON(ctx, *report, log, &sealReportBoundedWriter{writer: spool, remaining: limit})
	})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
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
	// Explicit fixed chunks, including destinations that implement ReaderFrom.
	// No encoder or io.Copy fast path may assemble the complete report in RAM.
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

// The output is an unpublished private file. Walk failures can leave a prefix
// here; the caller must discard it rather than expose it as a successful report.
func writeSealReportJSON(ctx context.Context, report SealReport, log *ledger.Log, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	header, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if report.SealedGenerations == 0 {
		if err := log.WalkSegments(ctx, func(ledger.Segment) error { return errors.New("seal report generation count changed") }); err != nil {
			return err
		}
		_, err := fmt.Fprintf(output, "%s\n", header)
		return err
	}
	if _, err := fmt.Fprintf(output, "%s,\"verified\":[", header[:len(header)-1]); err != nil {
		return err
	}
	count := 0
	encoder := json.NewEncoder(output)
	if err := log.WalkSegments(ctx, func(segment ledger.Segment) error {
		if count >= report.SealedGenerations {
			return errors.New("seal report generation count changed")
		}
		if count > 0 {
			if _, err := io.WriteString(output, ","); err != nil {
				return err
			}
		}
		if err := encoder.Encode(segment); err != nil {
			return err
		}
		count++
		return nil
	}); err != nil {
		return err
	}
	if count != report.SealedGenerations {
		return errors.New("seal report generation count changed")
	}
	_, err = io.WriteString(output, "]}\n")
	return err
}
