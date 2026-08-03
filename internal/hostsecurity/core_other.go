//go:build !linux && !darwin

package hostsecurity

import "errors"

func disableCoreDumps() (bool, error) {
	return false, errors.New("RLIMIT_CORE hardening is unsupported on this platform")
}
