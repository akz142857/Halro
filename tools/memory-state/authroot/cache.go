package authroot

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
)

var errCacheUnsupported = errors.New("experiment pack cache advice is unsupported on this platform")

// Advisory, scoped to one experiment-owned pack. This is not a memory budget:
// the kernel may retain pages, and syscall/writeback latency is unbounded.
type packCache struct {
	quantum                                                 int64
	drop                                                    func(*os.File, int64, int64) error
	lastWriteDrop                                           int64 // protected by the owning diskTree writer mutex
	readBytes                                               atomic.Uint64
	writeSyncs, writeDrops, readDrops, advisedBytes, errors atomic.Uint64
	readMu                                                  sync.Mutex
	readWindows                                             [16]int64 // fixed working set; never a historical range list
	readWindowCount                                         int
}
type packCacheStats struct {
	WriteSyncs, WriteDrops, ReadDrops, AdvisedBytes, Errors, ReadBytes uint64
}

func (c *packCache) stats() packCacheStats {
	return packCacheStats{c.writeSyncs.Load(), c.writeDrops.Load(), c.readDrops.Load(), c.advisedBytes.Load(), c.errors.Load(), c.readBytes.Load()}
}
func (s *packCacheStats) add(v packCacheStats) {
	s.WriteSyncs += v.WriteSyncs
	s.WriteDrops += v.WriteDrops
	s.ReadDrops += v.ReadDrops
	s.AdvisedBytes += v.AdvisedBytes
	s.Errors += v.Errors
	s.ReadBytes += v.ReadBytes
}
func (c *packCache) discard(f *os.File, offset, length int64) (bool, error) {
	// DONTNEED ignores partial pages. Never pass size=0 (which means whole file).
	size := int64(os.Getpagesize())
	if offset < 0 || offset%size != 0 || length < 0 {
		c.errors.Add(1)
		return false, errCorrupt
	}
	length -= length % size
	if length == 0 {
		return false, nil
	}
	if err := c.drop(f, offset, length); err != nil {
		c.errors.Add(1)
		return false, err
	}
	c.advisedBytes.Add(uint64(length))
	return true, nil
}
func (c *packCache) afterWrite(f *os.File, offset int64, force bool) error {
	if !force && offset-c.lastWriteDrop < c.quantum {
		return nil
	}
	if !force {
		if err := f.Sync(); err != nil {
			c.errors.Add(1)
			return err
		}
		c.writeSyncs.Add(1)
	}
	dropped, err := c.discard(f, c.lastWriteDrop, offset-c.lastWriteDrop)
	if err != nil {
		return err
	}
	// Preserve the final partial page for the next synced interval. Old aligned
	// prefixes are never swept again, including after opening an existing pack.
	c.lastWriteDrop = offset - offset%int64(os.Getpagesize())
	if dropped {
		c.writeDrops.Add(1)
	}
	return nil
}
func (c *packCache) afterRead(f *os.File, offset int64, n int) error {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	total := c.readBytes.Add(uint64(n))
	quantum := uint64(c.quantum)
	// Track both ends (a bounded page can cross one window boundary). Upper tree
	// pages are reread frequently, so advising only the window of the read that
	// crosses the counter threshold can miss all preceding leaf windows.
	for _, pos := range [2]int64{offset, offset + int64(n) - 1} {
		start := pos / c.quantum * c.quantum
		found := false
		for _, existing := range c.readWindows[:c.readWindowCount] {
			if existing == start {
				found = true
				break
			}
		}
		if found {
			continue
		}
		if c.readWindowCount == len(c.readWindows) {
			if err := c.flushReadWindows(f); err != nil {
				return err
			}
		}
		c.readWindows[c.readWindowCount] = start
		c.readWindowCount++
	}
	if total/quantum == (total-uint64(n))/quantum {
		return nil
	}
	return c.flushReadWindows(f)
}

// Caller holds readMu. Each syscall handles at most quantum bytes; a batch has
// at most 16 windows. Advice is best effort, not a cache ownership guarantee.
func (c *packCache) flushReadWindows(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		c.errors.Add(1)
		return err
	}
	for _, start := range c.readWindows[:c.readWindowCount] {
		length := min(c.quantum, info.Size()-start)
		dropped, err := c.discard(f, start, length)
		if err != nil {
			return err
		}
		if dropped {
			c.readDrops.Add(1)
		}
	}
	c.readWindowCount = 0
	return nil
}
func (t *diskTree) configureCache(quantum int64) error {
	pageSize := int64(os.Getpagesize())
	if quantum < pageSize || quantum%pageSize != 0 {
		return errLimit
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.poisoned || t.cache.Load() != nil {
		return errCorrupt
	}
	t.ioMu.RLock()
	defer t.ioMu.RUnlock()
	if err := configurePackAccess(t.pack); err != nil {
		return err
	}
	t.cache.Store(&packCache{quantum: quantum, drop: discardPackCache, lastWriteDrop: t.nextOffset - t.nextOffset%pageSize})
	return nil
}
