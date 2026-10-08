package app

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/statecut"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestHAMemorySelectedMetadataResumeActualTwoNodeWriteAndSevenRootPublication(t *testing.T) {
	f := prepareHAMetadataMaintenanceFixture(t)
	prepareHAMetadataMaintenanceArtifacts(t, f)
	completeHAMemoryActivationBoundary(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, cfg := range f.configs {
		guard, err := lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		activated, err := ActivateHAMemoryRuntime(ctx, cfg, f.keys, guard, functionalHAMetadataManager(t, cfg), logger)
		if err != nil {
			t.Fatal(err)
		}
		if err := activated.Close(); err != nil {
			t.Fatal(err)
		}
		if err := guard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var owners [2]*HAMemoryRuntimeActivation
	var selected [2]*statecut.ReopenedHA
	var guards [2]*lock.Lock
	var metadata [2]*boltstore.Store
	for i, cfg := range f.configs {
		var err error
		guards[i], err = lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		owners[i], selected[i], err = openHAMemoryActivationRead(ctx, cfg, f.keys, guards[i], functionalHAMetadataManager(t, cfg), logger, haMemoryStateBootstrapPath(cfg))
		if err != nil {
			t.Fatal(err)
		}
		if err := selected[i].ResumeSelectedMetadata(ctx); err != nil {
			t.Fatal("actual selected root handoff", i, err)
		}
		if ref, err := owners[i].maintenance.baseline.SealReference(ctx); err != nil || ref.Root != selected[i].Seal.Roots[1] {
			t.Fatal("physical latest root replaced actual selected root", ref, err)
		}
		metadata[i], err = selected[i].OpenHAMetadataStore(ctx)
		if err != nil {
			t.Fatal("actual selected HA Metadata DAO", i, err)
		}
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	complete := make(chan replication.MemberState, 2)
	for _, owner := range owners {
		go func(m *HAMetadataMaintenance) {
			done <- m.Run(runCtx, func(s replication.MemberState) error { complete <- s; return nil })
		}(owner.maintenance)
	}
	running := true
	t.Cleanup(func() {
		stop()
		if running {
			for range 2 {
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			}
		}
		for i, a := range owners {
			if err := a.Close(); err != nil {
				t.Error(err)
			}
			if err := guards[i].Close(); err != nil {
				t.Error(err)
			}
		}
	})
	for range 2 {
		select {
		case <-complete:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// An already-published migration boundary does not prove that this new
	// process has established its actual TLS peer window. Wait for the real
	// coordinator's availability before submitting a semantic write.
	for {
		_, available, _ := owners[0].maintenance.member.coordinator.Status()
		if available {
			break
		}
		select {
		case <-ctx.Done():
			_, _, cause := owners[0].maintenance.member.coordinator.Status()
			t.Fatal("actual restarted TLS peer never became available", cause, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	project, err := metadata[0].PutProject(ctx, domain.Project{ID: "seven-root-confirmed-write", Name: "Actual HA DAO", Enabled: true}, 0, nil)
	if err != nil || project.Revision != 1 {
		t.Fatal("actual DAO did not complete confirmed write", project, err)
	}
	beforeReplica := owners[1].maintenance.Snapshot()
	if _, err := metadata[1].PutProject(ctx, domain.Project{ID: "replica-write-refused", Name: "Forbidden"}, 0, nil); err == nil {
		t.Fatal("Replica DAO accepted semantic write")
	}
	if owners[1].maintenance.Snapshot().DurableIndex != beforeReplica.DurableIndex {
		t.Fatal("Replica DAO refusal changed durable native cut")
	}
	index := owners[0].maintenance.Snapshot().ConfirmedIndex
	for owners[1].maintenance.Snapshot().AppliedIndex != index {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	for i, store := range metadata {
		actual, err := store.GetProject(ctx, project.ID)
		if err != nil || actual.ID != project.ID || actual.Name != project.Name || actual.Revision != project.Revision {
			t.Fatal("actual permitted HA DAO read", i, actual, err)
		}
	}
	// Stop after native/ordering confirmation and Replica applied, deliberately
	// before any new common publication. The next process must select the old
	// seven roots rather than trust the maintenance replay's newer physical rows.
	stop()
	for range 2 {
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	running = false
	for i, a := range owners {
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		if err := guards[i].Close(); err != nil {
			t.Fatal(err)
		}
	}
	for i, cfg := range f.configs {
		guards[i], err = lock.Acquire(cfg.Storage.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		owners[i], selected[i], err = openHARuntimeSavedPrefix(ctx, cfg, f.keys, guards[i], functionalHAMetadataManager(t, cfg), logger, haMemoryStateBootstrapPath(cfg))
		if err != nil {
			t.Fatal("actual saved-prefix restart", i, err)
		}
		if selected[i].Seal.OrderingIndex != index-1 || selected[i].Seal.Positions[1].Sequence != 0 {
			t.Fatal("candidate native head granted newer common permission", selected[i].Seal)
		}
		if err := selected[i].Owners.Components.Metadata.View(ctx, func(tx *metadatastate.Transaction) error {
			_, found, err := tx.Get([]string{"projects"}, []byte("seven-root-confirmed-write"))
			if found {
				return errors.New("unpublished tail exposed by saved root")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i, r := range selected {
		if err := r.AdvanceFullHAPrefix(ctx); err != nil {
			t.Fatal("actual independent full-prefix reduction", i, err)
		}
		base := r.Seal
		base.Revision++
		published, err := statecut.PublishHA(ctx, r.Owners, base)
		if err != nil {
			t.Fatal("actual seven roots after selected Metadata write", i, err)
		}
		if published.Seal.OrderingIndex != index || published.Seal.Positions[1].Sequence != 1 || published.Seal.RootCount != 7 || published.Seal.Roots[1] == r.Seal.Roots[1] {
			t.Fatal("new common root did not bind actual ordered Metadata", published.Seal)
		}
		if err := r.Owners.Components.Metadata.View(ctx, func(tx *metadatastate.Transaction) error {
			value, found, err := tx.Get([]string{"projects"}, []byte("seven-root-confirmed-write"))
			var actual domain.Project
			decodeErr := json.Unmarshal(value, &actual)
			if err != nil || !found || decodeErr != nil || actual.Name != project.Name || actual.Revision != 1 {
				return errors.Join(err, errors.New("actual allowed native suffix was not reduced"))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, component := range []int{0, 2, 3, 4, 5, 6} {
			if published.Seal.Roots[component] != r.Seal.Roots[component] {
				t.Fatal("unrelated roots changed during actual Metadata-only delta", i, component)
			}
		}
	}
}
