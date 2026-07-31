package usage

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/akz142857/Heimdall/internal/ledger"
)

type Replayer interface {
	Replay(ledger.Watermark, func(ledger.Record) error) (ledger.Watermark, error)
}

type CollectorStats struct {
	QueueDepth    int
	QueueCapacity int
	Dropped       uint64
	Lagging       bool
}

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
}

func NewCollector(aggregate *Aggregate, replayer Replayer, capacity int) (*Collector, error) {
	if aggregate == nil || replayer == nil || capacity < 1 {
		return nil, errors.New("aggregate, replayer, and positive queue capacity are required")
	}
	return &Collector{aggregate: aggregate, replayer: replayer, queue: make(chan ledger.Record, capacity)}, nil
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
		case <-ctx.Done():
			return
		}
	}
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
		if _, err := c.replayer.Replay(c.aggregate.Watermark(), func(record ledger.Record) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return c.aggregate.Apply(record)
		}); err != nil {
			return err
		}
		c.drainLocked()
		c.lagging.Store(false)
		if c.dropped.Load() == baseline {
			return nil
		}
		c.lagging.Store(true)
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
	return CollectorStats{QueueDepth: len(c.queue), QueueCapacity: cap(c.queue), Dropped: c.dropped.Load(), Lagging: c.lagging.Load()}
}
