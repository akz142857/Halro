//go:build linux

package scratchstate

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

// This fixed-scale microbenchmark measures actual ext4 FD IO, including every
// existing page/tree MAC and namespace check. It is neither a Usage semantic
// fixture nor capacity acceptance; no production owner or RAM limit is changed.
func BenchmarkScratchFreshReducerIO(b *testing.B) {
	if os.Getenv("HALRO_SCRATCH_IO_BENCH") != "1" {
		b.Skip("opt-in isolated ext4 IO microbenchmark")
	}
	if os.Getenv("HALRO_EXT4_ROOT") != "/halro-fixture/data" {
		b.Fatal("requires isolated ext4")
	}
	for _, records := range []int{100, 1000, 5000, 10000} {
		b.Run(strconv.Itoa(records), func(b *testing.B) {
			if b.N != 1 {
				b.Fatal("fixed experiment requires -benchtime=1x")
			}
			limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 256 << 20, Disk: 2 << 30}, MaxClaims: 32, MaxOwners: 8, MaxPressureAge: time.Hour}
			for i := range limits.Classes {
				limits.Classes[i] = limits.Total
			}
			limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
			limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 32 * resourcebudget.ClaimRAMBytes}
			manager, err := resourcebudget.New(limits)
			if err != nil {
				b.Fatal(err)
			}
			// Library-only logical acquisition; the container's actual physical limit
			// applies independently. This is not Runtime sampler or capacity evidence.
			if err := manager.SetPressure(resourcebudget.Healthy); err != nil {
				b.Fatal(err)
			}
			store, err := Create(context.Background(), filepath.Join(b.TempDir(), "fresh"), Options{Budget: manager, MaxDiskBytes: 256 << 20})
			if err != nil {
				b.Fatal(err)
			}
			// Test-only known key, assigned before any authenticated row exists,
			// makes the before/after treap priorities and actual page IO identical.
			// Production Create still uses crypto/rand and exposes no key option.
			clear(store.key[:])
			for index := range store.key {
				store.key[index] = byte(index*7 + 3)
			}
			store.initializeMAC()
			defer func() {
				if err := store.Discard(); err != nil {
					b.Fatal(err)
				}
			}()
			before := scratchProcIO(b)
			b.ResetTimer()
			var value [512]byte
			for base := 0; base < records; base += 64 {
				err := store.Update(context.Background(), func(tx *Transaction) error {
					for record := base; record < min(base+64, records); record++ {
						binary.BigEndian.PutUint64(value[:8], uint64(record))
						clear(value[8:])
						for namespace := 0; namespace < 10; namespace++ {
							var key string
							if namespace < 6 {
								key = fmt.Sprintf("namespace-%02d/request-%064d", namespace, record)
							} else {
								key = fmt.Sprintf("namespace-%02d/balance", namespace)
							}
							if _, _, err := tx.Get([]byte(key)); err != nil {
								return err
							}
							if err := tx.Put([]byte(key), value[:]); err != nil {
								return err
							}
						}
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
			rows := 0
			err = store.View(context.Background(), func(tx *Transaction) error {
				iterator, err := tx.Iterator()
				if err != nil {
					return err
				}
				for {
					row, found, err := iterator.Next()
					if err != nil {
						return err
					}
					if !found {
						return nil
					}
					rows++
					expected := records - 1
					if slash := bytes.IndexByte(row.Key, '/'); slash >= 0 && bytes.HasPrefix(row.Key[slash+1:], []byte("request-")) {
						expected, err = strconv.Atoi(string(row.Key[slash+9:]))
						if err != nil {
							return err
						}
					}
					if len(row.Value) != len(value) || binary.BigEndian.Uint64(row.Value[:8]) != uint64(expected) {
						return ErrCorrupt
					}
				}
			})
			if err != nil || rows != records*6+4 {
				b.Fatal("actual iterator differs", rows, err)
			}
			b.StopTimer()
			after := scratchProcIO(b)
			b.ReportMetric(float64(after[0]-before[0])/float64(records), "read-syscalls/record")
			b.ReportMetric(float64(after[1]-before[1])/float64(records), "write-syscalls/record")
			b.ReportMetric(float64(after[2]-before[2])/float64(records), "rchar-bytes/record")
			b.ReportMetric(float64(after[3]-before[3])/float64(records), "disk-read-bytes/record")
			b.ReportMetric(float64(rows), "authenticated-rows")
			b.Logf("actual /proc/self/io delta syscr=%d syscw=%d rchar=%d read_bytes=%d; 64-record transactions; 10 Get+Put per record, 6 unique+4 shared rows; includes /proc sample read", after[0]-before[0], after[1]-before[1], after[2]-before[2], after[3]-before[3])
		})
	}
}
func scratchProcIO(b *testing.B) [4]uint64 {
	b.Helper()
	raw, err := os.ReadFile("/proc/self/io")
	if err != nil {
		b.Fatal(err)
	}
	var result [4]uint64
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		index := -1
		switch fields[0] {
		case "syscr:":
			index = 0
		case "syscw:":
			index = 1
		case "rchar:":
			index = 2
		case "read_bytes:":
			index = 3
		}
		if index >= 0 {
			result[index], err = strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				b.Fatal(err)
			}
		}
	}
	return result
}
