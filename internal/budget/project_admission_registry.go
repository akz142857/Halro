package budget

import "sync"

// Registry ownership covers money/lifecycle lock holders AND their waiters.
// Pending reservations and Run-close markers retain identity between calls.
// Idle projects keep no entry; there is no TTL or eviction of live admission.
// Cardinality follows current users/obligations, not all historically seen IDs.
// Whole-process active admission and shared resource fees remain caller concerns.
type projectAdmissionRegistry struct {
	mu      sync.Mutex
	entries map[string]*projectAdmission
}

func (r *projectAdmissionRegistry) retain(projectID string) *projectAdmission {
	r.mu.Lock()
	defer r.mu.Unlock()
	admission := r.entries[projectID]
	if admission == nil {
		admission = newProjectAdmission()
		if r.entries == nil {
			r.entries = make(map[string]*projectAdmission)
		}
		r.entries[projectID] = admission
	}
	admission.users++
	return admission
}

// Release only AFTER dropping the owned money/lifecycle lock. No path acquires
// registry.mu while holding admission.mu; the registry never spans a reader,
// durable append or confirmation wait. With zero users nobody can hold or wait
// on admission.mu, so checking obligations cannot block a live money operation.
func (r *projectAdmissionRegistry) release(projectID string, admission *projectAdmission) {
	r.mu.Lock()
	defer r.mu.Unlock()
	admission.users--
	if admission.users != 0 {
		return
	}
	admission.mu.Lock()
	idle := len(admission.pending) == 0 && len(admission.runPending) == 0 && len(admission.runAdmissions) == 0 && len(admission.closingRuns) == 0
	admission.mu.Unlock()
	if idle && r.entries[projectID] == admission {
		delete(r.entries, projectID)
		if len(r.entries) == 0 {
			// Drop backing capacity once the registry is completely idle.
			r.entries = nil
		}
	}
}

func (r *projectAdmissionRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// Clear is called only after Manager operations and deferred admission cleanup
// have actually joined. It never permits live callers to replace their locks.
func (r *projectAdmissionRegistry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = nil
}

// Range is an internal diagnostic/test traversal, not a public query path.
// The callback must not reenter the registry or block on a project lock.
func (r *projectAdmissionRegistry) Range(visit func(any, any) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, admission := range r.entries {
		if !visit(id, admission) {
			return
		}
	}
}
