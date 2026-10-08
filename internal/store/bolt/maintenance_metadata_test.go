package bolt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
)

func TestMetadataMaintenanceFiniteWalkAndPointReads(t *testing.T) {
	s := vaultStagedFixture(t, filepath.Join(t.TempDir(), "metadata.db"), 130)
	defer s.Close()
	seen := 0
	err := s.WalkAdminUsersMaintenance(context.Background(), func(item domain.AdminUser) error {
		if item.Username != fmt.Sprintf("row-%04d", seen) {
			return errors.New("unexpected identity or order")
		}
		seen++
		// A write in the callback must complete, proving the actual reader is closed,
		// but even a last-page write revokes success for the fixed initial source.
		if seen == 130 {
			return s.migrate(func(tx *Tx) error {
				return tx.Bucket(bucketAdminUsers).Put([]byte("later"), []byte(`{"username":"later"}`))
			})
		}
		return nil
	})
	if !errors.Is(err, ErrPricingRecoveryChanged) || seen != 130 {
		t.Fatal(seen, err)
	}
	credential, err := s.GetCredentialMaintenance(context.Background(), "row-0000")
	if err != nil || credential.ID != "row-0000" {
		t.Fatal(credential, err)
	}
	for name, err := range map[string]error{
		"providers":   s.WalkProvidersMaintenance(context.Background(), nil),
		"deployments": s.WalkDeploymentsMaintenance(context.Background(), nil),
		"routes":      s.WalkRoutesMaintenance(context.Background(), nil),
		"nil-context": s.WalkAdminUsersMaintenance(nil, func(domain.AdminUser) error { return nil }),
	} {
		if err == nil {
			t.Fatal(name, "accepted invalid consumer")
		}
	}
}

func TestMetadataMaintenanceRejectsStructureBeforeDomainAllocation(t *testing.T) {
	ctx := context.Background()
	for name, raw := range map[string][]byte{
		"huge":  bytes.Repeat([]byte(" "), pricingRecoveryWideRowBytes+1),
		"array": []byte(`[]`),
		"multi": []byte(`{} {}`),
		"depth": []byte(`{"unknown":` + strings.Repeat("[", 33) + `0` + strings.Repeat("]", 33) + `}`),
		"nodes": []byte(`{"unknown":[` + strings.Repeat(`0,`, 4096) + `0]}`),
	} {
		t.Run(name, func(t *testing.T) {
			result := domain.ProviderInstance{ID: "unchanged"}
			if err := decodeMetadataMaintenanceJSON(ctx, raw, &result); err == nil || result.ID != "unchanged" {
				t.Fatal(result.ID, err)
			}
		})
	}
	s := vaultStagedFixture(t, filepath.Join(t.TempDir(), "metadata.db"), 0)
	defer s.Close()
	raw, _ := json.Marshal(domain.ProviderInstance{ID: "other"})
	if err := s.migrate(func(tx *Tx) error { return tx.Bucket(bucketProviders).Put([]byte("expected"), raw) }); err != nil {
		t.Fatal(err)
	}
	if result, err := s.GetProviderMaintenance(ctx, "expected"); !errors.Is(err, ErrVaultRewriteIncomplete) || result.ID != "" {
		t.Fatal(result, err)
	}
	called := false
	if err := s.WalkProvidersMaintenance(ctx, func(domain.ProviderInstance) error { called = true; return nil }); !errors.Is(err, ErrVaultRewriteIncomplete) || called {
		t.Fatal(called, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.GetProviderMaintenance(cancelled, "expected"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
