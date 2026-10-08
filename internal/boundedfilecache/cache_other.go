//go:build !linux

package boundedfilecache

import "os"

// Non-Linux builds preserve IO correctness, without claiming Linux cache policy.
func advise(file *os.File, offset, length int64) error { return nil }
