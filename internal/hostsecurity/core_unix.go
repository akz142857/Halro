//go:build linux || darwin

package hostsecurity

import (
	"errors"
	"syscall"
)

func disableCoreDumps() (bool, error) {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &limit); err != nil {
		return false, err
	}
	limit.Cur = 0
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &limit); err != nil {
		return false, err
	}
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &limit); err != nil {
		return false, err
	}
	if limit.Cur != 0 {
		return false, errors.New("RLIMIT_CORE remained nonzero")
	}
	return true, nil
}
