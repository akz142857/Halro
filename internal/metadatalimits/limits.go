// Package metadatalimits holds shared Metadata file/mapping admission estimates.
// These estimates reserve logical capacity; they do not cap physical RSS.
package metadatalimits

import (
	"errors"
	"math"
)

// MappingOverheadRAMBytes conservatively covers mapping/freelist bookkeeping
// alongside a charge for every byte of the bounded data file. It excludes
// application caches, decoded rows, journal buffers and temporary copies.
const MappingOverheadRAMBytes int64 = 32 << 20

func MappingRAMBytes(maxFileBytes int64) (int64, error) {
	if maxFileBytes < 0 || maxFileBytes > math.MaxInt64-MappingOverheadRAMBytes {
		return 0, errors.New("Metadata mapping estimate overflows")
	}
	if maxFileBytes == 0 {
		return 0, nil
	}
	return maxFileBytes + MappingOverheadRAMBytes, nil
}
