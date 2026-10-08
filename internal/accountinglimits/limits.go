// Package accountinglimits supplies structural lower bounds, not capacity advice.
package accountinglimits

import (
	"errors"
	"math"
)

// Two independent 8 MiB pages, wide parent/lease/settlement records, JSON/native
// encoding and projection reduction may coexist during exclusive recovery.
// This conservative workspace floor still requires target peak calibration;
// Startup uses the same envelope for bounded Ledger/Usage/Governance native
// passes/batches, archive window validation and candidate cleanup. Calibrate the
// largest phase peak; these jobs are sequential, not simultaneous.
// It does not cover native Open, mmap, live DB caches, disk or HA persistent costs.
const MinRecoveryRAMBytes int64 = 128 << 20

// ProjectionMappingRAMBytes charges the complete bounded data files and a
// separate mapping/freelist envelope per live bbolt DB. Reducer/codec workspace
// is additional. Zero is the legacy uncapped path, not a physical RSS promise.
func ProjectionMappingRAMBytes(fileCeilings ...int64) (int64, error) {
	var total int64
	for _, size := range fileCeilings {
		if size < 0 || size > math.MaxInt64-(32<<20) {
			return 0, errors.New("projection mapping estimate overflows")
		}
		if size == 0 {
			continue
		}
		fee := size + (32 << 20)
		if fee > math.MaxInt64-total {
			return 0, errors.New("projection mapping aggregate overflows")
		}
		total += fee
	}
	return total, nil
}
