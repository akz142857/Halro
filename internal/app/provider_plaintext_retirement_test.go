package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/dirscan"
	"github.com/akz142857/Halro/internal/diskset"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestProviderPlaintextRetirementPreservesRefreshedRowAndSealedReplacement(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "sealed replacement", true: "new revision same path"}[refresh], func(t *testing.T) {
			r, bootstrap, _ := providerMaintenanceBudgetRuntime(t)
			directory := filepath.Join(r.config.Storage.DataDir, "provider-objects")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			name := "legacy.content"
			path := filepath.Join(directory, name)
			if err := os.WriteFile(path, []byte("legacy plaintext"), 0600); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			resource := domain.ProviderResource{ID: "legacy", Kind: domain.ResourceFile, ProjectID: bootstrap.ProjectID, ProviderID: bootstrap.ProviderID, DeploymentID: bootstrap.DeploymentID, ProfileID: domain.ProfileOpenAIMediaResources, PublicModel: "chat", ObjectPath: name, CreationStatus: "completed", Status: "uploaded", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
			if _, err := r.store.PutProviderResource(context.Background(), resource, 0); err != nil {
				t.Fatal(err)
			}
			expected, err := r.store.ProviderResource(context.Background(), bootstrap.ProjectID, resource.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, observed, err := inspectObjectHeader(path)
			if err != nil {
				t.Fatal(err)
			}
			if refresh {
				updated := expected
				updated.Status = "refreshed"
				if _, err := r.store.PutProviderResource(context.Background(), updated, expected.Revision); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append([]byte("HVLT"), 1, 0), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = r.store.DeleteProviderResourceIfUnchanged(context.Background(), expected, func() error { return r.retireUnsealedProviderObject(context.Background(), directory, name, observed) })
			if refresh && !errors.Is(err, boltstore.ErrRevisionConflict) || !refresh && !errors.Is(err, dirscan.ErrIdentity) {
				t.Fatal("stale retirement licensed deletion", err)
			}
			if _, err := r.store.ProviderResource(context.Background(), bootstrap.ProjectID, resource.ID); err != nil {
				t.Fatal("changed resource removed", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("current file removed", err)
			}
			if !refresh {
				if _, err := os.Stat(path + ".original"); err != nil {
					t.Fatal("renamed original removed", err)
				}
			}
		})
	}
}

func TestProviderPlaintextOrphanSweepRequiresCompleteCurrentReferences(t *testing.T) {
	r, bootstrap, manager := providerMaintenanceBudgetRuntime(t)
	directory := filepath.Join(r.config.Storage.DataDir, "provider-objects")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	resource := domain.ProviderResource{ID: "named", Kind: domain.ResourceFile, ProjectID: bootstrap.ProjectID, ProviderID: bootstrap.ProviderID, DeploymentID: bootstrap.DeploymentID, ProfileID: domain.ProfileOpenAIMediaResources, PublicModel: "chat", ObjectPath: "named.content", InputObjectPath: "named.input", CreationStatus: "completed", Status: "uploaded", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if _, err := r.store.PutProviderResource(context.Background(), resource, 0); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"named.content", "named.input", "orphan", ".pending"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("legacy plaintext"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "sealed"), append([]byte("HVLT"), 1, 0), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := r.store.Info()
	if err != nil {
		t.Fatal(err)
	}
	var count uint64
	err = r.withProviderMaintenance(context.Background(), true, func(ctx context.Context, refs *diskset.Set) error {
		var err error
		count, err = r.sweepProviderObjectsWithMode(ctx, refs, true)
		return err
	})
	if err != nil || count != 1 {
		t.Fatal(count, err)
	}
	for _, name := range []string{"named.content", "named.input", ".pending", "sealed"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Fatal("protected file lost", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(directory, "orphan")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("definite plaintext orphan retained", err)
	}
	after, err := r.store.Info()
	if err != nil || after.TxID != before.TxID || manager.Snapshot().Used != (resourcebudget.Cost{}) {
		t.Fatal("orphan sweep changed facts or retained closed fees", before, after, manager.Snapshot(), err)
	}
}
