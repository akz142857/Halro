package metadatastate

import (
	"context"
	"errors"
	"testing"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/metadatajournal"
)

// Component guard test uses the actual authenticated root/native owner. The
// complete seven-root peer/Member/Ordering/TLS constructor is verified by App.
func TestHAPeerTransitionColdOwnerRejectsOrdinaryMetadataEntries(t *testing.T) {
	b, o, _ := linkedDeltaCandidate(t)
	ctx := context.Background()
	ref, err := b.SealReference(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head := b.journal.Head()
	budget := o.Budget.Snapshot().Used
	b.haPeerTransition = &haPeerTransitionSource{}
	if _, err := b.Snapshot(ctx); err == nil {
		t.Fatal("cold owner exposed an ordinary snapshot")
	}
	if err := b.View(ctx, func(*Transaction) error { t.Fatal("cold read callback entered"); return nil }); !errors.Is(err, ErrSource) {
		t.Fatal(err)
	}
	if _, err := b.Update(ctx, func(*Transaction) error { t.Fatal("cold write callback entered"); return nil }); !errors.Is(err, ErrSource) {
		t.Fatal(err)
	}
	if _, err := b.AppendFrame(ctx, []metadatajournal.Op{{Kind: metadatajournal.OpPut, Path: []string{"projects"}, Key: []byte("cold"), Value: []byte("forbidden")}}); !errors.Is(err, ErrSource) {
		t.Fatal(err)
	}
	if _, err := b.ReplayTo(ctx, head, head.Sequence); !errors.Is(err, ErrSource) {
		t.Fatal(err)
	}
	if _, err := b.ReplayPeerTransitionTo(ctx, head, head.Sequence); !errors.Is(err, ErrSource) {
		t.Fatal("cold owner applied without a real promise/confirmed source", err)
	}
	if _, err := b.StandaloneFacade(ctx, o.Budget, b.evidence.Identity, b.evidence.InstanceID, b.evidence.KeyGeneration, ref); !errors.Is(err, ErrSource) {
		t.Fatal(err)
	}
	p := metadatajournal.BoundReplicationAuthority{Baseline: head.Baseline, FileRevision: head.FileRevision, MaxFileBytes: 16 << 20}
	if err := b.ConfigureHAPrimaryJournal(ctx, p, func(metadatajournal.DurableBatch) (metadatajournal.AppendAdmission, error) { return nil, nil }, func(metadatajournal.DurableBatch) (uint64, error) { return 1, nil }, func(context.Context, uint64) error { return nil }); !errors.Is(err, ErrSource) {
		t.Fatal(err)
	}
	if _, err := b.ConfigureHAReplicaJournal(ctx, metadataTestKey(), p); !errors.Is(err, ErrSource) {
		t.Fatal(err)
	}
	if _, err := b.HAFacade(ctx, o.Budget, b.evidence.Identity, b.evidence.InstanceID, b.evidence.KeyGeneration, ref, authenticatedstate.SealReplica, func(ctx context.Context, visit func(context.Context, authenticatedstate.SealPosition) error) error {
		return visit(ctx, ref.Position)
	}); !errors.Is(err, ErrSource) {
		t.Fatal(err)
	}
	if got, err := b.SealReference(ctx); err != nil || got != ref {
		t.Fatal("cold reference lost the original selected position", got, err)
	}
	if b.journal.Head() != head || o.Budget.Snapshot().Used != budget {
		t.Fatal("refused cold operations changed facts or fees")
	}
}
