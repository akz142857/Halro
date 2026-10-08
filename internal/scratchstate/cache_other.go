//go:build !linux

package scratchstate

import "os"

// No Linux cgroup/cache-bound claim is made by the portable functional path.
func retireCacheWindow(_ *os.File, _ int64) error { return nil }
