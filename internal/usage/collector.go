package usage

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

type Replayer interface {
	Replay(ledger.Watermark, func(ledger.Record) error) (ledger.Watermark, error)
}

type CollectorStats struct {
	QueueDepth    int
	QueueCapacity int
	Dropped       uint64
	Lagging       bool
	// FlushFailures counts history writes that failed and were retried.
	FlushFailures uint64
}

// Flush policy for the history index: a batch is written when it is this old
// or this large, whichever comes first. Both bound what Usage holds in memory
// between two writes, and the interval bounds how far the index trails the
// Ledger.
const (
	historyFlushInterval = 250 * time.Millisecond
	historyFlushRecords  = 2000
	// replayChunkRecords bounds how many records one Ledger walk collects
	// while it holds the Ledger's writer mutex.
	replayChunkRecords = 2000
)

var errReplayChunkFull = errors.New("usage replay chunk is full")

// Collector keeps derivative Usage aggregation off the durable append path.
// Queue overflow drops only derivative notifications and marks the collector
// lagging; CatchUp reconstructs the gap from the authoritative Ledger.
type Collector struct {
	aggregate *Aggregate
	replayer  Replayer
	queue     chan ledger.Record
	applyMu   sync.Mutex
	lagging   atomic.Bool
	dropped   atomic.Uint64
	flushFail atomic.Uint64
	building  atomic.Bool
	// replayChunk is replayChunkRecords outside tests.
	replayChunk int
}

// NewCollector starts lagging: the aggregate resumes from the history index's
// watermark, which is usually behind the Ledger head, and a live record
// applied before CatchUp has replayed the gap would advance the watermark past
// records nobody applied. Live records are therefore dropped until the first
// CatchUp, which reads them back from the Ledger in order.
func NewCollector(aggregate *Aggregate, replayer Replayer, capacity int) (*Collector, error) {
	if aggregate == nil || replayer == nil || capacity < 1 {
		return nil, errors.New("aggregate, replayer, and positive queue capacity are required")
	}
	collector := &Collector{aggregate: aggregate, replayer: replayer, queue: make(chan ledger.Record, capacity), replayChunk: replayChunkRecords}
	collector.lagging.Store(true)
	return collector, nil
}

func (c *Collector) Observe(record ledger.Record) {
	select {
	case c.queue <- record:
	default:
		c.dropped.Add(1)
		c.lagging.Store(true)
	}
}

func (c *Collector) Run(ctx context.Context) {
	ticker := time.NewTicker(historyFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case record := <-c.queue:
			c.applyMu.Lock()
			if !c.lagging.Load() {
				if err := c.aggregate.Apply(record); err != nil {
					c.lagging.Store(true)
				}
			}
			c.applyMu.Unlock()
			if c.aggregate.PendingRecords() >= historyFlushRecords {
				c.flush()
			}
		case <-ticker.C:
			c.flush()
		case <-ctx.Done():
			c.flush()
			return
		}
	}
}

// flush writes the pending batch. A failure leaves the batch in the aggregate
// for the next attempt, so it is counted rather than lost.
func (c *Collector) flush() {
	if err := c.aggregate.Flush(); err != nil {
		c.flushFail.Add(1)
	}
}

// Build is the catch-up that opens a runtime: it replays the Ledger from the
// index's watermark, which on the first start after an upgrade is the whole
// Ledger. While it runs, Building reports true so readers serve what the index
// already holds instead of waiting for the build.
func (c *Collector) Build(ctx context.Context) error {
	c.building.Store(true)
	defer c.building.Store(false)
	return c.CatchUp(ctx)
}

// Building reports whether the opening catch-up is still running.
func (c *Collector) Building() bool {
	return c.building.Load()
}

func (c *Collector) CatchUp(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	for {
		baseline := c.dropped.Load()
		c.drainLocked()
		// Always replay the suffix: a worker may already have dequeued a
		// durable record while waiting for applyMu, so queue depth alone is not
		// a sufficient read barrier.
		if err := c.replayToHead(ctx); err != nil {
			return err
		}
		c.drainLocked()
		c.lagging.Store(false)
		if c.dropped.Load() == baseline {
			return c.aggregate.Flush()
		}
		c.lagging.Store(true)
	}
}

// replayToHead applies the Ledger from the index watermark to its head, one
// bounded chunk per Replay. Replay holds the Ledger's writer mutex for its whole
// walk, so a walk that also applied and wrote history would stall every append
// for as long as the gap takes — on the first start after an upgrade, the whole
// Ledger. Each walk therefore only collects a chunk and stops; the chunk is
// applied and written after the lock is released, and the next walk resumes
// after its last record. Memory stays one chunk deep.
func (c *Collector) replayToHead(ctx context.Context) error {
	from := c.aggregate.Watermark()
	chunk := make([]ledger.Record, 0, c.replayChunk)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk = chunk[:0]
		_, err := c.replayer.Replay(from, func(record ledger.Record) error {
			if len(chunk) == cap(chunk) {
				return errReplayChunkFull
			}
			chunk = append(chunk, record)
			return nil
		})
		if err != nil && !errors.Is(err, errReplayChunkFull) {
			return err
		}
		for _, record := range chunk {
			if err := c.aggregate.Apply(record); err != nil {
				return err
			}
			if err := c.aggregate.FlushIfFull(); err != nil {
				return err
			}
		}
		if err == nil {
			return nil
		}
		last := chunk[len(chunk)-1]
		from = ledger.Watermark{Generation: last.Generation, Offset: last.Offset, Sequence: last.Sequence}
	}
}

func (c *Collector) drainLocked() {
	for {
		select {
		case record := <-c.queue:
			if !c.lagging.Load() {
				if err := c.aggregate.Apply(record); err != nil {
					c.lagging.Store(true)
				}
			}
		default:
			return
		}
	}
}

func (c *Collector) Stats() CollectorStats {
	return CollectorStats{QueueDepth: len(c.queue), QueueCapacity: cap(c.queue), Dropped: c.dropped.Load(), Lagging: c.lagging.Load(), FlushFailures: c.flushFail.Load()}
}
