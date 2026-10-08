package audit

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditBatchRejectsAllConflictsBeforeNativeWrite(t *testing.T) {
	path, key := filepath.Join(t.TempDir(), "audit.log"), randomKey(t)
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	old := sequencedEvent(1)
	if _, err := log.Append(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	head := log.Summary()
	changed := old
	changed.ActorID = "different"
	if _, err := log.AppendBatch(context.Background(), []Event{sequencedEvent(2), changed}); err == nil {
		t.Fatal("cached conflict accepted")
	}
	changed = sequencedEvent(2)
	changed.ActorID = "different"
	if _, err := log.AppendBatch(context.Background(), []Event{sequencedEvent(2), changed}); err == nil {
		t.Fatal("in-batch conflict accepted")
	}
	if actual, err := Verify(path, key); err != nil || actual != head || log.Summary() != head {
		t.Fatal("conflict left native bytes", actual, err)
	}
	got, err := log.AppendBatch(context.Background(), []Event{sequencedEvent(2), sequencedEvent(2)})
	if err != nil || got[0].Sequence != head.Records+1 || got[0].Sequence != got[1].Sequence || log.Summary().Records != head.Records+1 {
		t.Fatal("same batch retry changed facts", err)
	}
}

func TestAuditBatchBoundsRowsAndWidePayloadBeforeNativeWrite(t *testing.T) {
	path, key := filepath.Join(t.TempDir(), "audit.log"), randomKey(t)
	log, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	tooMany := make([]Event, MaxBatchEvents+1)
	if _, err := log.AppendBatch(context.Background(), tooMany); !errors.Is(err, ErrBatchLimit) {
		t.Fatal(err)
	}
	wide := make([]Event, MaxBatchEvents)
	for i := range wide {
		wide[i] = sequencedEvent(i)
		wide[i].ActorID = strings.Repeat("x", 32<<10)
	}
	if _, err := log.AppendBatch(context.Background(), wide); !errors.Is(err, ErrBatchLimit) {
		t.Fatal(err)
	}
	if actual, err := Verify(path, key); err != nil || actual.Records != 0 || actual.Bytes != 0 {
		t.Fatal("oversize batch wrote bytes", actual, err)
	}
}
