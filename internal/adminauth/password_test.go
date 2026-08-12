package adminauth

import (
	"os"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func TestArgon2WorkIsBoundedProcessWide(t *testing.T) {
	if cap(argonHashSlots) != argonHashConcurrency || argonHashConcurrency != 2 {
		t.Fatalf("argon2 slots capacity=%d concurrency=%d", cap(argonHashSlots), argonHashConcurrency)
	}
	for range argonHashConcurrency {
		argonHashSlots <- struct{}{}
	}
	acquired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		argonHashSlots <- struct{}{}
		close(acquired)
		<-argonHashSlots
		close(done)
	}()

	select {
	case <-acquired:
		t.Fatal("argon2 work acquired a slot above the process-wide bound")
	case <-time.After(25 * time.Millisecond):
	}

	<-argonHashSlots
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("queued argon2 work did not proceed after a slot was released")
	}
	<-done
	for len(argonHashSlots) > 0 {
		<-argonHashSlots
	}
}

func TestArgon2IDPasswordVerification(t *testing.T) {
	password := []byte("correct horse battery staple")
	user, err := NewUser("admin", password, domain.AdminRoleAdministrator, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(user, password) {
		t.Fatal("correct password was rejected")
	}
	if VerifyPassword(user, []byte("wrong password value")) {
		t.Fatal("wrong password was accepted")
	}
	if PasswordNeedsUpgrade(user) {
		t.Fatal("new password unexpectedly needs upgrade")
	}
}

func TestPasswordMinimumUsesUnicodeCharacters(t *testing.T) {
	if _, err := NewUser("admin", []byte(strings.Repeat("密", 7)), domain.AdminRoleAdministrator, time.Now().UTC()); err == nil {
		t.Fatal("seven-character password was accepted")
	}
	password := []byte(strings.Repeat("密", 8))
	user, err := NewUser("admin", password, domain.AdminRoleAdministrator, time.Now().UTC())
	if err != nil {
		t.Fatalf("eight-character Unicode password was rejected: %v", err)
	}
	if !VerifyPassword(user, password) {
		t.Fatal("accepted Unicode password could not be verified")
	}
}

// The structural test above proves the slot count. This one produces the
// number the release criterion actually asks for: what a concurrent login
// storm costs in memory. It is opt-in because it allocates hundreds of MiB and
// samples the heap, which makes it a poor neighbour in the ordinary suite —
// run it with HALRO_MEASURE_ARGON2=1 and record the result in
// docs/verification/performance-baseline.md.
func TestArgon2MemoryUnderAConcurrentLoginStorm(t *testing.T) {
	if os.Getenv("HALRO_MEASURE_ARGON2") != "1" {
		t.Skip("set HALRO_MEASURE_ARGON2=1 to measure; this allocates hundreds of MiB")
	}
	user, err := NewUser("measured", []byte("correct horse battery staple"), domain.AdminRoleAdministrator, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	goruntime.GC()
	var base goruntime.MemStats
	goruntime.ReadMemStats(&base)

	var peak atomic.Uint64
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		var stats goruntime.MemStats
		for {
			select {
			case <-stop:
				return
			default:
			}
			goruntime.ReadMemStats(&stats)
			for {
				current := peak.Load()
				if stats.HeapInuse <= current || peak.CompareAndSwap(current, stats.HeapInuse) {
					break
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	const concurrency = 64
	var wait sync.WaitGroup
	started := time.Now()
	for range concurrency {
		wait.Add(1)
		go func() {
			defer wait.Done()
			VerifyPassword(user, []byte("correct horse battery staple"))
		}()
	}
	wait.Wait()
	close(stop)
	sampler.Wait()

	growth := float64(peak.Load()-base.HeapInuse) / (1 << 20)
	perLogin := growth / concurrency
	t.Logf("concurrency=%d peak heap growth=%.1f MiB (%.2f MiB per concurrent login) in %s; unbounded would be ~%d MiB",
		concurrency, growth, perLogin, time.Since(started), concurrency*argonMemoryKiB/1024)
	// Two slots of 64 MiB plus allocator slack. The point of the assertion is
	// that growth does not scale with concurrency: unbounded, 64 logins cost
	// about 4 GiB.
	if growth > 512 {
		t.Fatalf("peak heap growth %.1f MiB exceeds the bounded expectation for %d slots", growth, argonHashConcurrency)
	}
}
