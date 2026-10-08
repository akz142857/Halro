//go:build race

package scratchstate

// The race-instrumented os.File descriptor IO retains one instrumented byte
// range on the heap. Production builds must still pass the zero-allocation gate.
const maximumPageIOAllocations = 1
