package usage

import "time"

// The console's window over the aggregate.
//
// The attempt list and the failed-request list are served from memory, and
// until this window existed that memory only ever grew: every attempt this data
// directory had ever seen stayed resident, so what the console cost rose with
// the instance's uptime rather than with its load, and never came back down.
//
// A window fixes the shape of that curve — resident cost becomes throughput
// times window length, about a kilobyte per attempt, rather than a function of
// how long the process has been up. What it must not do is lose anything that
// has not already been archived, which is the whole reason PruneBefore takes a
// watermark as well as a cutoff.

// PruneResult is what one sweep removed, and the floor it left behind.
type PruneResult struct {
	Attempts  int
	Summaries int
	// Floor is the lowest ledger sequence the aggregate still claims to hold.
	// Reconciliation needs it: comparing a windowed aggregate against a longer
	// Parquet history would otherwise report every archived-and-trimmed record
	// as missing, and refuse.
	Floor uint64
}

// PruneBefore drops attempts and request summaries that are both older than
// the cutoff and already exported.
//
// Two conditions, and the second is the one that matters. Export selects
// attempts whose sequence is above the Parquet manifest's watermark, so an
// attempt trimmed below that watermark is already archived, and an attempt
// trimmed above it has been destroyed — the aggregate was the only place it
// still existed. `exportedThrough` is that watermark, and passing zero
// therefore prunes nothing, which is the correct behaviour when export is off
// or has failed: an aggregate that grows is a problem, an aggregate that
// silently discards unarchived history is a defect.
//
// Records are removed as a prefix. The slices are appended in ledger order, so
// the scan stops at the first record that has to be kept and leaves everything
// after it — including anything out of time order, which is kept rather than
// hunted for. Keeping too much is safe; keeping too little is not.
func (a *Aggregate) PruneBefore(cutoff time.Time, exportedThrough uint64) PruneResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	if exportedThrough == 0 {
		return PruneResult{Floor: a.floor}
	}
	retain := func(sequence uint64, completedAt time.Time) bool {
		return sequence > exportedThrough || !completedAt.Before(cutoff)
	}

	attempts := 0
	for attempts < len(a.attempts) && !retain(a.attempts[attempts].Sequence, a.attempts[attempts].CompletedAt) {
		attempts++
	}
	summaries := 0
	for summaries < len(a.summaries) && !retain(a.summaries[summaries].Sequence, a.summaries[summaries].CompletedAt) {
		summaries++
	}
	if attempts == 0 && summaries == 0 {
		return PruneResult{Floor: a.floor}
	}

	// The floor is the highest sequence that is no longer held, plus one. Taken
	// from both slices, because a request summary and its attempts are separate
	// records and either may be the last one dropped.
	for _, dropped := range []uint64{
		lastSequence(a.attempts[:attempts]),
		lastSummarySequence(a.summaries[:summaries]),
	} {
		if dropped >= a.floor {
			a.floor = dropped + 1
		}
	}

	// Dropping the prefix costs what was dropped, not what is kept.
	//
	// It used to copy both slices onto fresh arrays every sweep, because a
	// prefix re-slice leaves the whole backing array alive and the memory this
	// exists to release would not be. That is true of the array — but the array
	// is the small part. What a record actually costs is the strings and the
	// price snapshot hanging off it, and clearing the dropped entries releases
	// all of that immediately, in time proportional to how many were dropped.
	// The array itself is compacted only once the dead prefix outgrows what is
	// live, which bounds the waste at a factor of two and makes the copy
	// amortized rather than hourly.
	//
	// The difference is not academic: at ten requests a second and a thirty-day
	// window the old sweep copied twenty-six million records — seconds of the
	// aggregate's write lock, blocking the collector and every console read,
	// once an hour, to drop one hour's worth.
	clear(a.attempts[:attempts])
	clear(a.summaries[:summaries])
	a.attempts = a.attempts[attempts:]
	a.summaries = a.summaries[summaries:]
	a.attemptsDropped += attempts
	a.summariesDropped += summaries
	if a.attemptsDropped > len(a.attempts) {
		a.attempts = append([]AttemptEvent(nil), a.attempts...)
		a.attemptsDropped = 0
	}
	if a.summariesDropped > len(a.summaries) {
		a.summaries = append([]RequestSummary(nil), a.summaries...)
		a.summariesDropped = 0
	}
	a.trimmed += uint64(attempts)
	return PruneResult{Attempts: attempts, Summaries: summaries, Floor: a.floor}
}

// Floor is the lowest ledger sequence this aggregate still claims to hold.
// Zero means it has never been pruned and therefore claims everything.
func (a *Aggregate) Floor() uint64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.floor
}

// WindowState is what the console window looks like from the outside.
type WindowState struct {
	Attempts  int
	Summaries int
	Floor     uint64
	// TrimmedAttempts counts everything this process has removed. Read beside
	// the resident count it answers the only question that matters here: a
	// resident count climbing while this stays put means the trim is not
	// running, which is what a stalled export looks like from outside — the
	// trim is bounded by the export and stops when it does.
	TrimmedAttempts uint64
}

// Windowed reports the aggregate's resident size and window edge.
func (a *Aggregate) Windowed() WindowState {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return WindowState{
		Attempts: len(a.attempts), Summaries: len(a.summaries),
		Floor: a.floor, TrimmedAttempts: a.trimmed,
	}
}

func lastSequence(attempts []AttemptEvent) uint64 {
	if len(attempts) == 0 {
		return 0
	}
	return attempts[len(attempts)-1].Sequence
}

func lastSummarySequence(summaries []RequestSummary) uint64 {
	if len(summaries) == 0 {
		return 0
	}
	return summaries[len(summaries)-1].Sequence
}
