package gateway

import "context"

// The admission callback lends its actual finite owners before acceptance.
// Attempt callers retain their own cancellation/deadline and values; missing
// owner values come only from that already-captured request context.
type terminalAdmissionContext struct {
	context.Context
	admitted context.Context
}

func (c terminalAdmissionContext) Value(key any) any {
	if value := c.Context.Value(key); value != nil {
		return value
	}
	return c.admitted.Value(key)
}

func (run *requestRun) attemptContext(ctx context.Context) (context.Context, func()) {
	if run.closeTerminalAdmission == nil || run.completionContext == nil {
		return ctx, func() {}
	}
	// The callback may itself have a shorter lifetime. Join that cancellation
	// with the current attempt; neither canceled context can become usable by
	// copying the other one's owner values.
	bound, cancel := context.WithCancelCause(ctx)
	if deadline, ok := run.completionContext.Deadline(); ok {
		if callerDeadline, callerHasDeadline := ctx.Deadline(); !callerHasDeadline || deadline.Before(callerDeadline) {
			var stopDeadline context.CancelFunc
			bound, stopDeadline = context.WithDeadlineCause(bound, deadline, context.DeadlineExceeded)
			priorCancel := cancel
			cancel = func(cause error) { stopDeadline(); priorCancel(cause) }
		}
	}
	stop := context.AfterFunc(run.completionContext, func() { cancel(context.Cause(run.completionContext)) })
	if err := run.completionContext.Err(); err != nil {
		cancel(context.Cause(run.completionContext))
	}
	return terminalAdmissionContext{Context: bound, admitted: run.completionContext}, func() { stop(); cancel(nil) }
}
