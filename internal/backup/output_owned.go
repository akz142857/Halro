package backup

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/ownedfile"
)

// Includes the fixed 1MiB encryption buffer, its AEAD ciphertext and bounded
// source/catalog/tar work. This is independent of disposable index mapping RAM.
const OutputWorkspaceRAMBytes int64 = 3 << 20

var ErrOutputDiskLimit = errors.New("encrypted backup output exceeds admitted capacity")

type archiveOutput struct {
	file      *os.File
	owner     *ownedfile.Owner
	alias     *ownedfile.Owner
	token     ledger.NativeFileDiskReservation
	limit     int64
	written   int64
	closed    bool
	closeErr  error
	published bool
}

type archiveOutputDebt struct {
	output *archiveOutput
	cause  error
}

type archiveCatalogDebt struct {
	catalog *SourceCatalog
	cause   error
}

func (e *archiveCatalogDebt) Error() string { return e.cause.Error() }
func (e *archiveCatalogDebt) Unwrap() error { return e.cause }

func (e *archiveOutputDebt) Error() string { return e.cause.Error() }
func (e *archiveOutputDebt) Unwrap() error { return e.cause }

func newArchiveOutput(options CreateOptions) (*archiveOutput, error) {
	output := &archiveOutput{limit: options.OutputDiskBytes}
	if output.limit < 0 || output.limit > 1<<50 || (output.limit > 0) != (options.ReserveOutputDisk != nil) {
		return nil, ErrOutputDiskLimit
	}
	var err error
	if options.ReserveOutputDisk != nil {
		output.token, err = options.ReserveOutputDisk(output.limit)
		if err != nil {
			return nil, err // No output inode exists on definite admission refusal.
		}
	}
	output.file, output.owner, err = ownedfile.CreateTemporary(filepath.Dir(options.OutputPath), ".halro-backup-*.tmp")
	if err != nil {
		return nil, &archiveOutputDebt{output: output, cause: err}
	}
	return output, nil
}

func (o *archiveOutput) Write(raw []byte) (int, error) {
	if o.limit > 0 && int64(len(raw)) > o.limit-o.written {
		return 0, ErrOutputDiskLimit
	}
	n, err := o.file.Write(raw)
	o.written += int64(n)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (o *archiveOutput) verify() error {
	if err := o.owner.VerifyFile(o.file); err != nil {
		return err
	}
	if o.limit == 0 {
		return nil
	}
	info, err := o.file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Blocks < 0 || stat.Blocks > math.MaxInt64/512 || info.Size() > o.limit || stat.Blocks*512 > o.limit {
		return ErrOutputDiskLimit
	}
	return nil
}

func (o *archiveOutput) closeFile() error {
	if !o.closed {
		o.closed = true
		o.closeErr = ownedfile.CloseFile(o.file)
	}
	return o.closeErr
}

func (o *archiveOutput) publish(destination string) error {
	return o.publishWithSync(destination, durable.SyncDirectory)
}

func (o *archiveOutput) publishWithSync(destination string, syncDirectory func(string) error) error {
	var err error
	o.alias, err = o.owner.LinkCaptured(destination)
	if o.alias != nil {
		o.published = true // Captured link happened; later error cannot erase it.
	}
	if err != nil {
		return err
	}
	if err := o.owner.Retire(); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return err
	}
	return o.alias.VerifyName()
}

func (o *archiveOutput) finish(cause error) error {
	cause = errors.Join(cause, o.closeFile())
	if cause == nil {
		return nil
	}
	// A published output is persistent authority. Unknown Close or namespace
	// exit keeps every captured name and actual descriptor reachable.
	if o.published || errors.Is(cause, ownedfile.ErrRetirement) || errors.Is(cause, ownedfile.ErrReplaced) {
		return &archiveOutputDebt{output: o, cause: errors.Join(cause, ownedfile.ErrRetirement)}
	}
	if o.owner != nil {
		retireErr := o.owner.Retire()
		if retireErr == nil {
			retireErr = durable.SyncDirectory(filepath.Dir(o.file.Name()))
		}
		if retireErr == nil && o.token != nil {
			retireErr = o.token.RetireKnown()
			if retireErr == nil {
				o.token = nil
			}
		}
		cause = errors.Join(cause, retireErr)
		if retireErr != nil {
			cause = errors.Join(cause, ownedfile.ErrRetirement)
		}
	}
	return &archiveOutputDebt{output: o, cause: cause}
}
