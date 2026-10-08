package resourcebudget

import "os"

// PhysicalIdentity includes immutable filesystem birth identity. It is only a
// physical comparison; source/selection authority must come from its caller.
type PhysicalIdentity struct {
	DeviceMajor, DeviceMinor uint32
	Inode                    uint64
	BornSeconds              int64
	BornNanos                uint32
}

func CapturePhysicalIdentity(file *os.File) (PhysicalIdentity, error) {
	return capturePhysicalIdentity(file)
}

func PhysicalAllocatedBytes(file *os.File) (int64, error) {
	return physicalAllocatedBytes(file)
}
