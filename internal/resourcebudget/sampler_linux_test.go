//go:build linux

package resourcebudget

import (
	"context"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"math"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestLinuxFilesystemSpaceRejectsMissingInodesAndOverflow(t *testing.T) {
	st := unix.Statfs_t{Frsize: 4096, Bsize: 8192, Bavail: 3, Bfree: 99, Files: 20, Ffree: 5}
	d, err := filesystemSpace(st)
	if err != nil || d.available != 12288 || d.inodes != 5 {
		t.Fatal(d, err)
	}
	st.Frsize = 0
	d, err = filesystemSpace(st)
	if err != nil || d.available != 24576 {
		t.Fatal(d, err)
	}
	st.Ffree = 0
	if d, err = filesystemSpace(st); err != nil || d.inodes != 0 {
		t.Fatal("known exhaustion", d, err)
	}
	for _, change := range []func(*unix.Statfs_t){func(s *unix.Statfs_t) { s.Files = 0 }, func(s *unix.Statfs_t) { s.Ffree = s.Files + 1 }, func(s *unix.Statfs_t) { s.Bavail = math.MaxUint64 }, func(s *unix.Statfs_t) { s.Files = math.MaxUint64; s.Ffree = math.MaxUint64 }, func(s *unix.Statfs_t) { s.Bsize = 0 }} {
		bad := st
		change(&bad)
		if _, err := filesystemSpace(bad); !errors.Is(err, ErrInvalid) {
			t.Fatal(bad, err)
		}
	}
}

// Explicit opt-in: the test touches 64MiB and needs a writable scratch filesystem
// and readable cgroup v2. Thresholds qualify this fixture, not production capacity.
func TestLinuxPressureSignalsAndAdmissionSmoke(t *testing.T) {
	if os.Getenv("HALRO_PRESSURE_LINUX_SMOKE") != "1" {
		t.Skip("opt-in Linux cgroup fixture")
	}
	s, err := NewSampler(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := PressurePolicy{RuntimeEnter: 48 << 20, RuntimeExit: 32 << 20, CgroupEnter: 192 << 20, CgroupExit: 160 << 20, HeadroomEnter: 16 << 20, HeadroomExit: 32 << 20, DiskEnter: 64 << 10, DiskExit: 128 << 10, InodesEnter: 16, InodesExit: 32, MaxAge: 10 * time.Second, Cooldown: time.Millisecond}
	c, err := NewController(p)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(fixtureLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err = m.BindController(c); err != nil {
		t.Fatal(err)
	}
	before := s.Sample(context.Background())
	if before.Issues != 0 {
		t.Fatal(before)
	}
	if err = m.ApplyPressure(c.Observe(before, time.Now())); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	ready := s.Sample(context.Background())
	d := c.Observe(ready, time.Now())
	if d.State() != Healthy {
		t.Fatal("fixture did not begin below explicit thresholds", d, ready)
	}
	if err = m.ApplyPressure(d); err != nil {
		t.Fatal(err)
	}
	work, err := m.TryAcquireWork(IO, Cost{RAM: 100}, Cost{RAM: 50})
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 64<<20)
	for i := 0; i < len(buffer); i += 4096 {
		buffer[i] = 1
	}
	after := s.Sample(context.Background())
	d = c.Observe(after, time.Now())
	if after.Issues != 0 || d.State() != Pressured || d.Issues()&RuntimeHigh == 0 {
		t.Fatal("actual touched heap did not close admission", d, after)
	}
	if err = m.ApplyPressure(d); err != nil {
		t.Fatal(err)
	}
	if _, err = m.TryAcquire(IO, Cost{RAM: 1}); err == nil {
		t.Fatal("ordinary admission stayed open")
	}
	done, err := work.Completion.ActivateCompletion()
	if err != nil {
		t.Fatal("owned completion stranded", err)
	}
	recovery, err := m.TryAcquireRecovery(Cost{RAM: 1})
	if err != nil {
		t.Fatal("recovery stranded", err)
	}
	if err = done.Release(); err != nil {
		t.Fatal(err)
	}
	if err = work.Active.Release(); err != nil {
		t.Fatal(err)
	}
	if err = recovery.Release(); err != nil {
		t.Fatal(err)
	}
	zeroUsage(t, m)
	for _, entry := range []struct {
		Name   string
		Sample Sample
	}{{"before", before}, {"ready", ready}, {"after_64MiB_touch", after}} {
		raw, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(string(raw))
	}
	runtime.KeepAlive(buffer)
}
