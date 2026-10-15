package bolt

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

// Migration 30 gives every stored price a cache-read rate. The reconstruction
// has to be the input rate: until this migration cached prompt tokens were
// billed at it, and decoding the missing term as zero would retroactively make
// every cached token free on prices nobody re-entered.
func TestMigration30ReconstructsTheCacheReadRateFromTheInputRate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = store.db.Update(func(tx *bbolt.Tx) error {
		price := map[string]any{
			"id": "price_legacy", "deployment_id": "dep_legacy", "version": 1, "revision": 1,
			"billing_mode": "metered", "currency": "USD", "formula_version": "usd_token_v1",
			"input_micros_per_million": 5_000_000, "output_micros_per_million": 30_000_000,
			"fixed_request_micros_usd": 0, "effective_from": "2026-01-01T00:00:00Z",
			"created_by": "admin", "created_at": "2026-01-01T00:00:00Z",
			"source": map[string]any{
				"type": "manual", "assurance": "asserted", "received_at": "2026-01-01T00:00:00Z",
				"reference": "official_public_price", "asserted_without_archive": true,
			},
		}
		encoded, err := json.Marshal(price)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketDeploymentPriceVersions).Put([]byte("price_legacy"), encoded); err != nil {
			return err
		}
		proposal := map[string]any{
			"id": "proposal_legacy", "deployment_id": "dep_legacy", "provider_id": "provider_legacy",
			"provider_model": "gpt-legacy", "billing_mode": "metered", "currency": "USD",
			"formula_version": "usd_token_v1", "input_micros_per_million": 5_000_000,
			"output_micros_per_million": 30_000_000, "fixed_request_micros_usd": 0,
			"match": "exact", "status": "pending", "created_by": "admin",
			"fetched_at": "2026-01-01T00:00:00Z", "expires_at": "2026-01-08T00:00:00Z",
			"created_at": "2026-01-01T00:00:00Z", "revision": 1,
			// The digest this build would compute covers the cache-read term, so a
			// record written before it cannot be re-derived here. What matters is
			// that the migration leaves a self-consistent record behind, which a
			// stale digest proves and a coincidentally-correct one would not.
			"digest": "sha256:" + "00000000000000000000000000000000000000000000000000000000000000ff",
			"source": map[string]any{
				"type": "official_url", "assurance": "asserted", "received_at": "2026-01-01T00:00:00Z",
				"uri": "https://example.test/pricing", "retrieved_at": "2026-01-01T00:00:00Z",
				"reference":      "public standard tier",
				"content_sha256": "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		}
		encodedProposal, err := json.Marshal(proposal)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketDeploymentPriceProposals).Put([]byte("proposal_legacy"), encodedProposal); err != nil {
			return err
		}
		var version [8]byte
		binary.BigEndian.PutUint64(version[:], 29)
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, version[:])
	})
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()

	var price domain.DeploymentPriceVersion
	err = migrated.db.View(func(tx *bbolt.Tx) error {
		return json.Unmarshal(tx.Bucket(bucketDeploymentPriceVersions).Get([]byte("price_legacy")), &price)
	})
	if err != nil {
		t.Fatal(err)
	}
	if price.CachedInputMicrosPerMillion != price.InputMicrosPerMillion {
		t.Fatalf("cached rate=%d input rate=%d", price.CachedInputMicrosPerMillion, price.InputMicrosPerMillion)
	}
	if err := price.Validate(); err != nil {
		t.Fatalf("migrated price does not validate: %v", err)
	}
	// The rate a migrated price charges is the rate it charged yesterday: a
	// prompt with cached tokens costs exactly what it did before the term existed.
	cost, err := domain.CalculateUSDTokensV1(1_000_000, 400_000, 0, price, time.Now().UTC())
	if err != nil || cost.InputCostMicrosUSD != 5_000_000 {
		t.Fatalf("migrated price re-rates existing traffic: cost=%#v err=%v", cost, err)
	}

	proposals, err := migrated.ListDeploymentPriceProposals(context.Background(), "dep_legacy")
	if err != nil || len(proposals) != 1 {
		t.Fatalf("proposals=%#v err=%v", proposals, err)
	}
	if proposals[0].CachedInputMicrosPerMillion != proposals[0].InputMicrosPerMillion {
		t.Fatalf("proposal cached rate=%d input rate=%d", proposals[0].CachedInputMicrosPerMillion, proposals[0].InputMicrosPerMillion)
	}
	if err := proposals[0].Validate(); err != nil {
		t.Fatalf("migrated proposal does not validate: %v", err)
	}
}
