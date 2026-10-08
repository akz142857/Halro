package metadatajournal

import (
	"errors"
	"os"

	"github.com/akz142857/Halro/internal/authenticatedstate"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// A failed actual Close is permanent uncertainty, not permission to retry IO
// or refund physical ownership. Callers hold the native mutex for Log helpers.
func closeDescriptor(file *os.File) error { return ownedfile.CloseFile(file) }

func (l *Log) availabilityError() error {
	if err := l.descriptorAvailabilityError(); err != nil {
		return err
	}
	if l.startupGuard != nil && !l.startupGuard.CoversNativePath(l.path) {
		return authenticatedstate.ErrSealIneligible
	}
	if l.startupAuditPending {
		if l.startupAudit == nil {
			return ErrStartupAuditPending
		}
		progress := l.startupAudit.Snapshot()
		if !progress.Ready {
			return errors.Join(ErrStartupAuditPending, progress.Err)
		}
	}
	return nil
}

func (l *Log) descriptorAvailabilityError() error {
	if l.retirementErr != nil {
		return l.retirementErr
	}
	if l.closed || l.closing || l.file == nil {
		return errors.New("metadata journal is closed")
	}
	return nil
}

func (l *Log) closeCurrent() error {
	if l.file == nil || l.retirementErr != nil {
		return l.retirementErr
	}
	err := l.file.Close()
	if err != nil {
		l.retirementErr = ownedfile.CloseError(l.file, err)
		l.closed = true
		return l.retirementErr
	}
	l.file, l.durability = nil, nil
	return nil
}
