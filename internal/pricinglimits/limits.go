// Package pricinglimits supplies structural lower bounds, not capacity advice.
package pricinglimits

import "github.com/akz142857/Halro/internal/metadatajournal"

const MinLifetimeRAMBytes int64 = 1 << 20

// A native wide row may coexist with its owned page and JSON decoder/string
// temporaries. Deployment-specific allocator/codec peaks still need calibration.
const MinIORAMBytes int64 = 4 * metadatajournal.MaxPayloadSize
