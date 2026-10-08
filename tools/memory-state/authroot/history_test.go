package authroot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This opted-in experiment uses generated KV rows, not Halro priced lifecycle
// records. It cannot establish domain accounting/HA correctness or acceptance.
type historyMemory struct {
	HeapAlloc, HeapInuse, Sys, RSS, CgroupCurrent uint64
	CgroupStat                                    cgroupBreakdown
}
type historyMaxima struct {
	Samples                                                                             int
	HeapAlloc, HeapInuse, Sys, RSS, CgroupCurrent                                       uint64
	CgroupFile, CgroupAnon, CgroupKernel, CgroupDirty, CgroupWriteback, PgScan, PgSteal uint64
}
type cgroupBreakdown struct {
	Available                                                     bool
	File, Anon, Kernel, FileDirty, FileWriteback, PgScan, PgSteal uint64
}

func parseCgroupMemoryStat(text string) cgroupBreakdown {
	var result cgroupBreakdown
	var required uint8
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "file":
			result.File = n
			required |= 1
		case "anon":
			result.Anon = n
			required |= 2
		case "kernel":
			result.Kernel = n
			required |= 4
		case "file_dirty":
			result.FileDirty = n
		case "file_writeback":
			result.FileWriteback = n
		case "pgscan":
			result.PgScan = n
		case "pgsteal":
			result.PgSteal = n
		}
	}
	result.Available = required == 7
	return result
}
func (max *historyMaxima) capture(m historyMemory) {
	max.Samples++
	pairs := []struct {
		source      uint64
		destination *uint64
	}{
		{m.HeapAlloc, &max.HeapAlloc}, {m.HeapInuse, &max.HeapInuse}, {m.Sys, &max.Sys}, {m.RSS, &max.RSS}, {m.CgroupCurrent, &max.CgroupCurrent},
		{m.CgroupStat.File, &max.CgroupFile}, {m.CgroupStat.Anon, &max.CgroupAnon}, {m.CgroupStat.Kernel, &max.CgroupKernel}, {m.CgroupStat.FileDirty, &max.CgroupDirty}, {m.CgroupStat.FileWriteback, &max.CgroupWriteback}, {m.CgroupStat.PgScan, &max.PgScan}, {m.CgroupStat.PgSteal, &max.PgSteal},
	}
	for _, p := range pairs {
		if p.source > *p.destination {
			*p.destination = p.source
		}
	}
}

type historyPhase struct {
	Name    string
	Seconds float64
	Memory  historyMemory
}
type historyResult struct {
	CachePolicy   string         `json:"cache_policy"`
	CacheQuantum  int64          `json:"cache_quantum_bytes"`
	CacheStats    packCacheStats `json:"cache_stats"`
	Rows          int            `json:"rows"`
	ValueBytes    int            `json:"value_bytes"`
	PackLimit     int64          `json:"pack_limit_bytes"`
	PackBytes     int64          `json:"pack_bytes"`
	Height        uint16         `json:"height"`
	Sequence      uint64         `json:"synthetic_sequence"`
	BatchRows     int            `json:"atomic_batch_rows"`
	QueryCount    int            `json:"point_query_count"`
	QueryReads    int            `json:"point_query_page_reads"`
	Phases        []historyPhase `json:"phases"`
	SampledMax    historyMaxima  `json:"sampled_maxima_20ms"`
	ObservedMax   historyMaxima  `json:"observed_maxima"`
	CgroupPeak    uint64         `json:"cgroup_memory_peak_bytes"`
	CgroupEvents  string         `json:"cgroup_memory_events"`
	VerifiedRows  int            `json:"verified_scan_rows"`
	VerifiedChain bool           `json:"verified_synthetic_chain"`
}

func smallSystemFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65536))
	if err != nil {
		return ""
	}
	return string(b)
}
func systemUint(path string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimSpace(smallSystemFile(path)), 10, 64)
	return n
}
func sampleHistoryMemory() historyMemory {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	sample := historyMemory{HeapAlloc: m.HeapAlloc, HeapInuse: m.HeapInuse, Sys: m.Sys, CgroupCurrent: systemUint("/sys/fs/cgroup/memory.current"), CgroupStat: parseCgroupMemoryStat(smallSystemFile("/sys/fs/cgroup/memory.stat"))}
	for _, line := range strings.Split(smallSystemFile("/proc/self/status"), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			parts := strings.Fields(line)
			if len(parts) > 1 {
				n, _ := strconv.ParseUint(parts[1], 10, 64)
				sample.RSS = n * 1024
			}
		}
	}
	return sample
}
func sampleHistoryMaxima() func() historyMaxima {
	stop := make(chan struct{})
	done := make(chan historyMaxima, 1)
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var max historyMaxima
		capture := func() { max.capture(sampleHistoryMemory()) }
		capture()
		for {
			select {
			case <-ticker.C:
				capture()
			case <-stop:
				capture()
				done <- max
				return
			}
		}
	}()
	return func() historyMaxima { close(stop); return <-done }
}

type countingPageSource struct {
	source pageSource
	reads  int
}

func (s *countingPageSource) readPage(ref pageRef, b []byte) (int, error) {
	s.reads++
	return s.source.readPage(ref, b)
}

func TestAuthrootHistoryExperiment(t *testing.T) {
	text := os.Getenv("HALRO_AUTHROOT_HISTORY_ROWS")
	if text == "" {
		t.Skip("opt-in synthetic KV history experiment")
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 10000 || n > 10000000 {
		t.Fatal("history rows must be 10000..10000000")
	}
	ctx := context.Background()
	key := bytes.Repeat([]byte{0x37}, 32)
	const packLimit int64 = 4 << 30
	result := historyResult{Rows: n, ValueBytes: 64, PackLimit: packLimit, BatchRows: 32, QueryCount: 2000}
	result.CachePolicy = "disabled"
	var caches []*packCache
	quantumText := os.Getenv("HALRO_AUTHROOT_CACHE_QUANTUM_BYTES")
	var quantum int64
	if quantumText != "" {
		quantum, err = strconv.ParseInt(quantumText, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		result.CachePolicy = "linux_random_sync_incremental_write_tracked_read_windows"
		result.CacheQuantum = quantum
	}
	configure := func(tree *diskTree) {
		if quantumText != "" {
			if err := tree.configureCache(quantum); err != nil {
				t.Fatal(err)
			}
			caches = append(caches, tree.cache.Load())
		}
	}

	stop := sampleHistoryMaxima()
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()
	runtime.GC()
	result.Phases = append(result.Phases, historyPhase{Name: "before_create_after_gc", Memory: sampleHistoryMemory()})
	dir := filepath.Join(t.TempDir(), "state")
	tree, err := createDiskTree(dir, key, 7, packLimit)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { tree.close() }()
	configure(tree)
	started := time.Now()
	if err := tree.bulkBuild(ctx, sortedFixture(n)); err != nil {
		t.Fatal(err)
	}
	result.Phases = append(result.Phases, historyPhase{Name: "bulk_build", Seconds: time.Since(started).Seconds(), Memory: sampleHistoryMemory()})
	fmt.Fprintf(os.Stdout, "AUTHROOT_PROGRESS built=%d\n", n)
	runtime.GC()
	result.Phases = append(result.Phases, historyPhase{Name: "open_after_build_gc", Memory: sampleHistoryMemory()})
	runtime.KeepAlive(tree)
	before := tree.current
	if before.count != uint64(n) || before.sequence != uint64(n) {
		t.Fatal("wrong bulk watermark/count")
	}
	if err := tree.close(); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	tree, err = openDiskTree(dir, key, 7, packLimit)
	if err != nil {
		t.Fatal(err)
	}
	configure(tree)
	result.Phases = append(result.Phases, historyPhase{Name: "reopen", Seconds: time.Since(started).Seconds(), Memory: sampleHistoryMemory()})
	r, err := tree.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	counter := &countingPageSource{source: r.source}
	r.source = counter
	started = time.Now()
	for i := 0; i < 1000; i++ {
		index := i * 7919 % n
		k := []byte(fmt.Sprintf("k%012d", index))
		v, found, err := r.get(ctx, k)
		if err != nil || !found || !bytes.Equal(v, bytes.Repeat([]byte{byte(index % 251)}, 64)) {
			t.Fatalf("point query %d failed", index)
		}
		if _, found, err := r.get(ctx, append(k, 'x')); err != nil || found {
			t.Fatal("false absence result")
		}
	}
	result.QueryReads = counter.reads
	result.Phases = append(result.Phases, historyPhase{Name: "point_queries", Seconds: time.Since(started).Seconds(), Memory: sampleHistoryMemory()})
	started = time.Now()
	var after []byte
	var digest [32]byte
	var expectedValue [64]byte
	for {
		page, err := r.scan(ctx, []byte("a"), []byte("z"), after, 32, 8192)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.rows {
			index := result.VerifiedRows
			for j := range expectedValue {
				expectedValue[j] = byte(index % 251)
			}
			if string(e.key) != fmt.Sprintf("k%012d", index) || !bytes.Equal(e.value, expectedValue[:]) {
				t.Fatalf("scan omitted/replaced row %d", index)
			}
			result.VerifiedRows++
			digest = mutationHash(digest, uint64(result.VerifiedRows), mutation{key: e.key, value: e.value})
		}
		if page.done {
			break
		}
		if len(page.rows) == 0 {
			t.Fatal("scan made no progress")
		}
		after = page.after
	}
	result.VerifiedChain = result.VerifiedRows == n && digest == before.factHash
	if !result.VerifiedChain {
		t.Fatal("full scan/input synthetic chain differs")
	}
	result.Phases = append(result.Phases, historyPhase{Name: "full_verified_scan", Seconds: time.Since(started).Seconds(), Memory: sampleHistoryMemory()})
	fmt.Fprintf(os.Stdout, "AUTHROOT_PROGRESS scanned=%d\n", n)
	input := make([]mutation, 32)
	wantHash := before.factHash
	for i := range input {
		input[i] = mutation{key: []byte(fmt.Sprintf("k%012d", i*(n/32))), value: bytes.Repeat([]byte{0xa5}, 64)}
		wantHash = mutationHash(wantHash, before.sequence+uint64(i)+1, input[i])
	}
	started = time.Now()
	if _, err := tree.applyBatch(ctx, input); err != nil {
		t.Fatal(err)
	}
	result.Phases = append(result.Phases, historyPhase{Name: "atomic_batch", Seconds: time.Since(started).Seconds(), Memory: sampleHistoryMemory()})
	if err := tree.close(); err != nil {
		t.Fatal(err)
	}
	tree, err = openDiskTree(dir, key, 7, packLimit)
	if err != nil {
		t.Fatal(err)
	}
	configure(tree)
	if tree.current.count != before.count || tree.current.sequence != before.sequence+32 || tree.current.factHash != wantHash {
		t.Fatal("batch not atomic after reopen")
	}
	check, err := tree.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range input {
		v, found, err := check.get(ctx, m.key)
		if err != nil || !found || !bytes.Equal(v, m.value) {
			t.Fatal("batch value not recovered")
		}
	}
	runtime.GC()
	result.Phases = append(result.Phases, historyPhase{Name: "open_after_final_gc", Memory: sampleHistoryMemory()})
	runtime.KeepAlive(tree)
	result.PackBytes = tree.nextOffset
	result.Height = tree.current.height
	result.Sequence = tree.current.sequence
	for _, cache := range caches {
		result.CacheStats.add(cache.stats())
	}
	result.SampledMax = stop()
	result.ObservedMax = result.SampledMax
	for _, phase := range result.Phases {
		result.ObservedMax.capture(phase.Memory)
	}
	stopped = true
	result.CgroupPeak = systemUint("/sys/fs/cgroup/memory.peak")
	result.CgroupEvents = smallSystemFile("/sys/fs/cgroup/memory.events")
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(os.Stdout, "AUTHROOT_RESULT %s\n", b)
}

func TestHistoryMemoryStatParser(t *testing.T) {
	got := parseCgroupMemoryStat("pgsteal 5\nfile_dirty 10\nfile 123\nslab 777\nfile_writeback 11\nunknown_key 555\nkernel 789\npgscan 6\nanon 456\n")
	want := cgroupBreakdown{Available: true, File: 123, Anon: 456, Kernel: 789, FileDirty: 10, FileWriteback: 11, PgScan: 6, PgSteal: 5}
	if got != want {
		t.Fatalf("unordered counters: %+v", got)
	}
	for _, text := range []string{"", "file 1\nanon 2", "file -1\nanon 2\nkernel 3"} {
		if parseCgroupMemoryStat(text).Available {
			t.Fatal("missing/invalid metric reported available")
		}
	}
}
