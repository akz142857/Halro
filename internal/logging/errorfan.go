package logging

import (
	"context"
	"log/slog"
)

// fanout writes every record to both the ordinary handler and the errors-only
// one, and lets each decide for itself whether to keep it.
//
// It sits *inside* safelog: the logger is safelog.New(fanout{...}), never two
// already-redacted loggers side by side with call sites choosing between them.
// Redaction is a property of the log, not a setting, and the moment a caller
// picks a destination it has also picked whether its record is redacted — which
// is a decision no call site should be able to make.
type fanout struct {
	handlers []slog.Handler
}

func newFanout(handlers ...slog.Handler) slog.Handler {
	if len(handlers) == 1 {
		return handlers[0]
	}
	return &fanout{handlers: handlers}
}

// Enabled is true when any destination wants the record. It has to be, or the
// stricter of the two would suppress records the other was configured to keep —
// and safelog delegates Enabled straight through, so this is what decides
// whether a record is built at all.
func (f *fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range f.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle offers the record to every destination that wants it, and keeps going
// after one fails. A file that cannot be written must not take the other copy
// with it; the sinks fall back to stderr on their own, and this returns the
// first error for the caller that ignores it anyway.
func (f *fanout) Handle(ctx context.Context, record slog.Record) error {
	var firstErr error
	for _, handler := range f.handlers {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}
		// Each handler may retain what it is given, so each gets its own copy.
		if err := handler.Handle(ctx, record.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (f *fanout) WithAttrs(attributes []slog.Attr) slog.Handler {
	next := make([]slog.Handler, 0, len(f.handlers))
	for _, handler := range f.handlers {
		next = append(next, handler.WithAttrs(attributes))
	}
	return &fanout{handlers: next}
}

func (f *fanout) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, 0, len(f.handlers))
	for _, handler := range f.handlers {
		next = append(next, handler.WithGroup(name))
	}
	return &fanout{handlers: next}
}
