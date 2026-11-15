package bolt

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/metadatajournal"
	bbolt "go.etcd.io/bbolt"
)

func TestMigration43BackfillsCacheWriteRatesAndProposalDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := openForTest(t, path)
	if err != nil {
		t.Fatal(err)
	}
	const version = `{"input_micros_per_million":400000,"cached_input_micros_per_million":200000,"output_micros_per_million":2000000}`
	const free = `{"billing_mode":"free","input_micros_per_million":0,"cached_input_micros_per_million":0}`
	const schedule = `{"timezone":"UTC","windows":[{"start_minute":1,"end_minute":20,"input_micros_per_million":123456}]}`
	const scheduled = `{"input_micros_per_million":300000,"schedule":` + schedule + `}`
	const oldDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	const proposal = `{
		"id":"proposal_legacy","deployment_id":"dep_legacy","provider_id":"provider_legacy","provider_model":"gpt-legacy",
		"billing_mode":"metered","currency":"USD","formula_version":"usd_token_v1","input_micros_per_million":800000,
		"output_micros_per_million":2000000,"match":"exact","status":"pending","created_by":"admin",
		"fetched_at":"2026-01-01T00:00:00Z","expires_at":"2026-01-08T00:00:00Z","created_at":"2026-01-01T00:00:00Z",
		"revision":1,"digest":"` + oldDigest + `","schedule":` + schedule + `,
		"source":{"type":"official_url","assurance":"asserted","received_at":"2026-01-01T00:00:00Z",
			"uri":"https://example.test/pricing","retrieved_at":"2026-01-01T00:00:00Z","reference":"public standard tier",
			"content_sha256":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`
	err = store.update(func(tx *Tx) error {
		for id, raw := range map[string]string{"base": version, "free": free, "scheduled": scheduled} {
			if err := tx.Bucket(bucketDeploymentPriceVersions).Put([]byte(id), []byte(raw)); err != nil {
				return err
			}
		}
		if err := tx.Bucket(bucketDeploymentPriceProposals).Put([]byte("proposal_legacy"), []byte(proposal)); err != nil {
			return err
		}
		var schema [8]byte
		binary.BigEndian.PutUint64(schema[:], 42)
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, schema[:])
	})
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := OpenWithJournalRecovery(path, func(*Store) ([]byte, error) { return testJournalKey(), nil })
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()

	checkRates := func(tier domain.PriceTier, want int64) {
		t.Helper()
		if tier.CacheWriteMicrosPerMillion != want || tier.CacheWriteLongMicrosPerMillion != want {
			t.Errorf("write rates in %+v should both be %d", tier, want)
		}
	}
	err = viewRaw(migrated.db, func(tx *Tx) error {
		for id, want := range map[string]int64{"base": 400_000, "free": 0, "scheduled": 300_000} {
			var price domain.DeploymentPriceVersion
			if err := json.Unmarshal(tx.Bucket(bucketDeploymentPriceVersions).Get([]byte(id)), &price); err != nil {
				return err
			}
			checkRates(price.BaseTier(), want)
			if id == "scheduled" {
				checkRates(price.Schedule.Windows[0].Tier(), 123_456)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := migrated.ListDeploymentPriceProposals(context.Background(), "dep_legacy")
	if err != nil || len(proposals) != 1 {
		t.Fatalf("proposals=%+v err=%v", proposals, err)
	}
	got := proposals[0]
	checkRates(got.BaseTier(), 800_000)
	checkRates(got.Schedule.Windows[0].Tier(), 123_456)
	if err := got.Validate(); err != nil || got.Digest == oldDigest {
		t.Fatalf("migration did not refresh digest: %s (%v)", got.Digest, err)
	}
}

func TestMigration43ReplaysJournalBeforeBackfillAndRecoversEpochPublication(t *testing.T) {
	for _, epochAlreadyPublished := range []bool{false, true} {
		name := "interrupted before epoch publication"
		if epochAlreadyPublished {
			name = "interrupted after epoch publication"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "metadata.db")
			store := openJournalledStore(t, path)
			journalPath := store.MetadataJournalPath()
			if err := store.db.Update(func(tx *bbolt.Tx) error {
				return tx.Bucket(bucketMeta).Put(keySchemaVersion, encodeUint64(42))
			}); err != nil {
				t.Fatal(err)
			}
			// Old process reached the durable journal write but never committed
			// the pricing transaction into the bbolt projection.
			if _, err := store.journal.Append([]metadatajournal.Op{{
				Kind: metadatajournal.OpPut, Path: []string{string(bucketDeploymentPriceVersions)},
				Key: []byte("pending_price"), Value: []byte(`{"input_micros_per_million":123456}`),
			}}); err != nil {
				t.Fatal(err)
			}
			head := journalHead(t, store)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if opened, err := Open(path); !errors.Is(err, ErrSchemaVersionMismatch) {
				if opened != nil {
					opened.Close()
				}
				t.Fatalf("unkeyed upgrade: %v", err)
			}
			wrongKey := make([]byte, metadatajournal.KeySize)
			if opened, err := OpenWithJournalRecovery(path, func(*Store) ([]byte, error) { return wrongKey, nil }); err == nil {
				opened.Close()
				t.Fatal("upgraded without authenticating the old journal")
			}
			legacy, err := OpenKeySlotMetadata(path)
			if err != nil {
				t.Fatal(err)
			}
			version, versionErr := legacy.SchemaVersion()
			if err := errors.Join(versionErr, legacy.Close()); err != nil || version != 42 {
				t.Fatalf("schema after refused upgrade: %d (%v)", version, err)
			}

			upgraded, err := OpenWithJournalRecovery(path, func(*Store) ([]byte, error) { return testJournalKey(), nil })
			if err != nil {
				t.Fatal(err)
			}
			if err := upgraded.Close(); err != nil {
				t.Fatal(err)
			}
			if epochAlreadyPublished {
				log, err := metadatajournal.StartEpoch(journalPath, testJournalKey(), metadatajournal.EpochHeader{
					Epoch: head.Epoch + 1, PreviousEpoch: head.Epoch, PreviousChainHead: head.Hash[:], Reason: "schema migration",
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := log.Close(); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			state, err := reopened.AttachMetadataJournal(testJournalKey(), "restart")
			if err != nil || !state.StartedEpoch || state.Epoch != head.Epoch+1 || state.Applied != 0 {
				t.Fatalf("journal state after retry: %+v (%v)", state, err)
			}
			position, err := reopened.journalPosition()
			if err != nil || position.pendingEpochHead != nil || position.epoch != state.Epoch {
				t.Fatalf("saved journal position: %+v (%v)", position, err)
			}
			var price domain.DeploymentPriceVersion
			if err := reopened.view(func(tx *Tx) error {
				return json.Unmarshal(tx.Bucket(bucketDeploymentPriceVersions).Get([]byte("pending_price")), &price)
			}); err != nil || price.CacheWriteMicrosPerMillion != 123_456 || price.CacheWriteLongMicrosPerMillion != 123_456 {
				t.Fatalf("replayed price was not migrated: %+v (%v)", price, err)
			}
		})
	}
}
