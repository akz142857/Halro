package gateway

import (
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

func queued(projectID string, ids ...string) []domain.ProviderResource {
	records := make([]domain.ProviderResource, 0, len(ids))
	for _, id := range ids {
		records = append(records, domain.ProviderResource{ID: id, ProjectID: projectID})
	}
	return records
}

// The queue ceiling is per Project and the workers draining it are process-wide.
// Serving the store's global submission order means one Project that fills its
// own queue is served in full before any other Project is served at all — and at
// the default ceiling that wait outlives the 24-hour TTL, so those submissions
// expire without ever having been tried.
func TestOneProjectsBacklogDoesNotHoldUpAnother(t *testing.T) {
	var pending []domain.ProviderResource
	pending = append(pending, queued("noisy", "n1", "n2", "n3", "n4", "n5")...)
	pending = append(pending, queued("quiet", "q1")...)

	order := interleaveByProject(pending)
	if len(order) != len(pending) {
		t.Fatalf("interleaved %d records, want %d", len(order), len(pending))
	}
	position := -1
	for index, record := range order {
		if record.ID == "q1" {
			position = index
		}
	}
	if position > 1 {
		t.Fatalf("the quiet Project's only submission is at position %d, behind a whole backlog", position)
	}
}

// Round robin must not reorder a Project's own submissions: within one Project
// the queue is still first in, first out.
func TestAProjectsOwnSubmissionsKeepTheirOrder(t *testing.T) {
	var pending []domain.ProviderResource
	pending = append(pending, queued("a", "a1", "a2", "a3")...)
	pending = append(pending, queued("b", "b1", "b2")...)

	var seen []string
	for _, record := range interleaveByProject(pending) {
		if record.ProjectID == "a" {
			seen = append(seen, record.ID)
		}
	}
	want := []string{"a1", "a2", "a3"}
	for index := range want {
		if seen[index] != want[index] {
			t.Fatalf("project a dequeued as %v, want %v", seen, want)
		}
	}
}

// A Project with nothing waiting takes no turn: round robin shares what is
// contended, it does not hand out reservations.
func TestASingleProjectQueueIsUnchanged(t *testing.T) {
	pending := queued("only", "x1", "x2", "x3")
	order := interleaveByProject(pending)
	for index, record := range order {
		if record.ID != pending[index].ID {
			t.Fatalf("a single-Project queue was reordered: %v", order)
		}
	}
}

// Turn order follows the global submission order the store returned, so who
// goes first is still decided by arrival time — it just no longer decides who
// goes at all.
func TestTurnOrderFollowsTheOldestWaitingSubmission(t *testing.T) {
	var pending []domain.ProviderResource
	pending = append(pending, queued("second", "s1")...)
	pending = append(pending, queued("first", "f1")...)
	order := interleaveByProject(append(queued("earliest", "e1"), pending...))
	if order[0].ID != "e1" || order[1].ID != "s1" || order[2].ID != "f1" {
		t.Fatalf("turn order = %v", order)
	}
}
