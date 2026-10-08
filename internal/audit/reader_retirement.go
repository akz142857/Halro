package audit

import (
	"errors"
	"os"

	"github.com/akz142857/Halro/internal/ownedfile"
)

// The caller keeps replayWG active until this actual Close finishes. A reader
// failure withdraws every admission and survives the writer's later Close.
func (l *Log) retireReader(file *os.File, closeFile func(*os.File) error) error {
	err := closeFile(file)
	if err == nil {
		return nil
	}
	err = ownedfile.CloseError(file, err)
	l.mu.Lock()
	l.replayRetirementErr = errors.Join(l.replayRetirementErr, err)
	l.closing = true
	l.mu.Unlock()
	return err
}
