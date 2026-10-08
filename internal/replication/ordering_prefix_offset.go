package replication

import (
	"errors"
	"math"
)

// PrefixOffset names the end of an existing authenticated ordering record.
// Role/applied/confirmed eligibility is the actual Member authority's job.
// Callers freezing that authority retain this journal's WithFrozenHead scope.
func (j *OrderingJournal) PrefixOffset(index uint64) (int64, error) {
	if j == nil {
		return 0, ErrOrderingSource
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.closing || j.file == nil || j.poisoned != nil || index > j.lastIndex || index > uint64((math.MaxInt64-j.dataOffset)/orderingRecordBytes) {
		return 0, errors.Join(ErrOrderingSource, j.poisoned)
	}
	return j.dataOffset + int64(index)*orderingRecordBytes, nil
}
