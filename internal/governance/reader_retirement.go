package governance

import (
	"errors"
	"os"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// The caller keeps readerWG active until this actual Close finishes. A reader
// failure withdraws every admission and survives the writer's later Close.
func (l *Log) retireReader(file *os.File, closeFile func(*os.File) error) error {
	err := closeFile(file)
	if err == nil {
		return nil
	}
	err = ownedfile.CloseError(file, err)
	l.mu.Lock()
	l.readerRetirementErr = errors.Join(l.readerRetirementErr, err)
	l.closing = true
	l.startupFault.Store(true)
	l.mu.Unlock()
	return err
}
