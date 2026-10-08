package resourcebudget

import "context"

const MaxCompletionBatch = 32

// TryAcquireCompletionBatch atomically admits a fixed terminal vector and its
// control owner before external work. Each credit may later transfer to the
// persistent confirmation owner without acquiring another claim or capacity.
func (m *Manager) TryAcquireCompletionBatch(class Class, active Cost, count int, each Cost) (Permit, []Permit, error) {
	if m == nil || !normalClass(class) || !active.valid() || active == (Cost{}) || count < 1 || count > MaxCompletionBatch || !each.valid() || each == (Cost{}) {
		return Permit{}, nil, ErrInvalid
	}
	var total Cost
	for i := 0; i < count; i++ {
		var ok bool
		total, ok = total.add(each)
		if !ok {
			return Permit{}, nil, ErrInvalid
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(class, active, total, false, count+1); err != nil {
		return Permit{}, nil, err
	}
	credits := make([]Permit, count)
	for i := range credits {
		credits[i] = m.charge(Completion, each, false, true)
	}
	return m.charge(class, active, false, false), credits, nil
}

type completionContinuationKey struct{}

// InCompletionContinuation distinguishes an explicit terminal callback from
// ordinary work carrying a request's reserved credits. The actual domain owner
// must still authenticate its own private capability, generation and lifetime.
func InCompletionContinuation(ctx context.Context) bool {
	return ctx != nil && ctx.Value(completionContinuationKey{}) == true
}
