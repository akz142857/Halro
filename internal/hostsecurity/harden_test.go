//go:build linux || darwin

package hostsecurity

import (
	"syscall"
	"testing"
)

func TestHardenSetsAndVerifiesZeroCoreLimit(t *testing.T) {
	report, err := Harden()
	if err != nil {
		t.Fatal(err)
	}
	if !report.CoreDumpsDisabled || report.ManagedHeapDontDump {
		t.Fatalf("report=%#v", report)
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_CORE, &limit); err != nil {
		t.Fatal(err)
	}
	if limit.Cur != 0 {
		t.Fatalf("RLIMIT_CORE=%d", limit.Cur)
	}
}
