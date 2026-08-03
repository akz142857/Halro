package hostsecurity

import "errors"

// Report is safe to log. It contains only process-control outcomes and no
// memory addresses, credentials, or key material.
type Report struct {
	CoreDumpsDisabled   bool
	ProcessDumpDisabled bool
	ManagedHeapDontDump bool
}

// Harden applies irreversible process controls before the Master Key is
// unlocked. ManagedHeapDontDump remains false: applying MADV_DONTDUMP to a Go
// heap page can affect unrelated objects and cannot be safely tied to a slice's
// lifetime. Dedicated non-Go secret allocation is required before enabling it.
func Harden() (Report, error) {
	coreDisabled, coreErr := disableCoreDumps()
	processDisabled, processErr := disableProcessDumping()
	return Report{
		CoreDumpsDisabled: coreDisabled, ProcessDumpDisabled: processDisabled,
		ManagedHeapDontDump: false,
	}, errors.Join(coreErr, processErr)
}
