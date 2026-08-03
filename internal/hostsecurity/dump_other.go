//go:build !linux

package hostsecurity

func disableProcessDumping() (bool, error) { return false, nil }
