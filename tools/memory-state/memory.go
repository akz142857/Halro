package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type memoryPoint struct {
	HeapAlloc    uint64            `json:"heap_alloc_bytes"`
	HeapInuse    uint64            `json:"heap_inuse_bytes"`
	Sys          uint64            `json:"runtime_sys_bytes"`
	HeapReleased uint64            `json:"heap_released_bytes"`
	TotalAlloc   uint64            `json:"total_alloc_bytes"`
	RSS          *uint64           `json:"rss_bytes,omitempty"`
	Cgroup       *uint64           `json:"cgroup_current_bytes,omitempty"`
	CgroupPeak   *uint64           `json:"cgroup_peak_bytes,omitempty"`
	CgroupMax    string            `json:"cgroup_max,omitempty"`
	CgroupEvents map[string]uint64 `json:"cgroup_events,omitempty"`
}

func sampleMemory(cgroup string) memoryPoint {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	p := memoryPoint{HeapAlloc: m.HeapAlloc, HeapInuse: m.HeapInuse, Sys: m.Sys, HeapReleased: m.HeapReleased, TotalAlloc: m.TotalAlloc}
	if runtime.GOOS == "linux" {
		if raw, err := os.ReadFile("/proc/self/status"); err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 3 && fields[0] == "VmRSS:" && fields[2] == "kB" {
					if n, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
						n *= 1024
						p.RSS = &n
					}
				}
			}
		}
	}
	if cgroup != "" {
		if raw, err := os.ReadFile(filepath.Join(cgroup, "memory.current")); err == nil {
			if n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err == nil {
				p.Cgroup = &n
			}
		}
		if raw, err := os.ReadFile(filepath.Join(cgroup, "memory.peak")); err == nil {
			if n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err == nil {
				p.CgroupPeak = &n
			}
		}
		if raw, err := os.ReadFile(filepath.Join(cgroup, "memory.max")); err == nil {
			p.CgroupMax = strings.TrimSpace(string(raw))
		}
		if raw, err := os.ReadFile(filepath.Join(cgroup, "memory.events")); err == nil {
			p.CgroupEvents = make(map[string]uint64)
			for _, line := range strings.Split(string(raw), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 2 {
					if n, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
						p.CgroupEvents[fields[0]] = n
					}
				}
			}
		}
	}
	return p
}

type memoryMonitor struct {
	mu           sync.Mutex
	peak         memoryPoint
	done, exited chan struct{}
	once         sync.Once
}

func startMonitor(cgroup string) *memoryMonitor {
	m := &memoryMonitor{done: make(chan struct{}), exited: make(chan struct{})}
	m.observe(sampleMemory(cgroup))
	go func() {
		defer close(m.exited)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-m.done:
				return
			case <-ticker.C:
				m.observe(sampleMemory(cgroup))
			}
		}
	}()
	return m
}

// Peaks are per-field maxima, not one simultaneous sample. Sampling does not
// prove a hard maximum and must be paired with cgroup peak/events on Linux.
func (m *memoryMonitor) observe(p memoryPoint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peak.HeapAlloc = max(m.peak.HeapAlloc, p.HeapAlloc)
	m.peak.HeapInuse = max(m.peak.HeapInuse, p.HeapInuse)
	m.peak.Sys = max(m.peak.Sys, p.Sys)
	m.peak.HeapReleased = max(m.peak.HeapReleased, p.HeapReleased)
	m.peak.TotalAlloc = max(m.peak.TotalAlloc, p.TotalAlloc)
	if p.RSS != nil && (m.peak.RSS == nil || *p.RSS > *m.peak.RSS) {
		n := *p.RSS
		m.peak.RSS = &n
	}
	if p.Cgroup != nil && (m.peak.Cgroup == nil || *p.Cgroup > *m.peak.Cgroup) {
		n := *p.Cgroup
		m.peak.Cgroup = &n
	}
	if p.CgroupPeak != nil && (m.peak.CgroupPeak == nil || *p.CgroupPeak > *m.peak.CgroupPeak) {
		n := *p.CgroupPeak
		m.peak.CgroupPeak = &n
	}
	m.peak.CgroupMax = p.CgroupMax
	m.peak.CgroupEvents = p.CgroupEvents
}

func (m *memoryMonitor) stop() memoryPoint {
	m.once.Do(func() { close(m.done) })
	<-m.exited
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak
}
