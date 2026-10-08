package governance

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/resourcebudget"
)

// ResolveProjectionPrefix derives the exact byte position for an independent
// sequence/hash permission. It authenticates the entire native source, including
// excluded received facts. It grants no role, applied progress or stored root.
func (l *Log) ResolveProjectionPrefix(ctx context.Context, budget *resourcebudget.Manager, sequence uint64, hash [32]byte) (prefix Summary, resultErr error) {
	resultErr = l.WithFrozenHead(ctx, budget, func(ctx context.Context, full Summary) error {
		seen := sequence == 0 && hash == ([32]byte{})
		actual, err := l.Replay(func(record Record) error {
			if record.Sequence == sequence && record.Hash == hash {
				prefix, seen = Summary{Records: record.Sequence, Bytes: record.Offset, LastHash: record.Hash}, true
			}
			return ctx.Err()
		})
		if err != nil || actual != full || !seen {
			return errors.Join(ErrGovernanceSealSource, err)
		}
		return ctx.Err()
	})
	if resultErr != nil {
		prefix = Summary{}
	}
	return
}
