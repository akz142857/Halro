package bolt

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryBucketOnDiskIsClassified walks a real, fully migrated database
// rather than a list somebody maintained by hand.
//
// A bucket that exists and has no class is the failure this test is for: the
// entry refuses to write to it, which is the right answer at runtime and a very
// late one to discover. Migrations create buckets, so the only honest source
// for "which buckets exist" is a database that has run all of them.
func TestEveryBucketOnDiskIsClassified(t *testing.T) {
	store, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var unclassified []string
	if err := store.view(func(tx *Tx) error {
		return tx.ForEach(func(name []byte, _ *Bucket) error {
			if string(name) == string(bucketMeta) {
				return nil
			}
			if _, known := bucketClasses[string(name)]; !known {
				unclassified = append(unclassified, string(name))
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if len(unclassified) > 0 {
		t.Fatalf("buckets exist with no journal class: %v", unclassified)
	}
}

// TestTheClassTableHasNoBucketsThatDoNotExist is the other direction. A stale
// entry is not dangerous the way a missing one is, but it is a claim about the
// schema that stopped being true, and this table is read as documentation.
func TestTheClassTableHasNoBucketsThatDoNotExist(t *testing.T) {
	store, err := openForTest(t, filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	onDisk := map[string]struct{}{}
	if err := store.view(func(tx *Tx) error {
		return tx.ForEach(func(name []byte, _ *Bucket) error {
			onDisk[string(name)] = struct{}{}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	// pricing_migration_resolutions is created on first use rather than by a
	// migration (pricing_migration.go), so a fresh database legitimately does
	// not have it. It is named here rather than exempted by a rule, so a second
	// lazily created bucket has to be thought about rather than absorbed.
	createdOnFirstUse := map[string]struct{}{"pricing_migration_resolutions": {}}
	for name := range bucketClasses {
		if _, exists := onDisk[name]; exists {
			continue
		}
		if _, lazy := createdOnFirstUse[name]; lazy {
			continue
		}
		t.Fatalf("bucketClasses classifies %q, which a migrated database does not have", name)
	}
}

// TestEveryDeclaredMetaKeyIsClassified references the key variables themselves,
// so a rename breaks the build here rather than silently leaving a key
// unclassified.
//
// `meta` is the reason classification is per key at all: these four classes sit
// in one bucket, and a bucket-level table would be wrong about three of them.
func TestEveryDeclaredMetaKeyIsClassified(t *testing.T) {
	declared := [][]byte{
		keySchemaVersion, keyVaultCheck, keyUsageCheckpoint, keyUsageRollupState,
		keyTokenGuardCheckpoint, keyAuditCheckpoint, keyAuditHMACEnvelope,
		keyLedgerHMACEnvelope, keyLedgerChainCheckpoint, keyGovernanceCheckpoint,
		keyGovernanceJournalAnchor, keyVaultKeyring, keyKeySlotDescriptor,
		keyKeySlotAuditIntent, keyMasterKeyRotationAuditIntent, keyRuntimeSettings,
		keyInstanceUISettings, keyInstanceUsageSettings, keyInstanceAccountingSettings,
		keyInstanceID, keyAdminBootstrapCompletion, keyMinimumLedgerReaderVersion,
		keyLedgerFeatureEpoch, keyShutdownTruncatedAttempts,
		keyMetadataHMACEnvelope, keyAppliedJournalSequence, keyMetadataJournalEpoch,
	}
	for _, key := range declared {
		class, err := classify([]string{string(bucketMeta)}, key)
		if err != nil {
			t.Fatalf("meta key %q: %v", key, err)
		}
		if class == classUnknown {
			t.Fatalf("meta key %q classified as unknown", key)
		}
	}
	if len(metaKeyClasses) != len(declared) {
		t.Fatalf("metaKeyClasses holds %d keys and %d are declared; one side has drifted",
			len(metaKeyClasses), len(declared))
	}
}

// TestClassifyFailsClosed. Guessing in the local direction drops an
// authoritative write from the journal, and nothing says so until a failover —
// which is exactly why there is no default.
func TestClassifyFailsClosed(t *testing.T) {
	if _, err := classify([]string{"a_bucket_nobody_classified"}, []byte("k")); err == nil {
		t.Fatal("an unclassified bucket was accepted")
	}
	if _, err := classify([]string{string(bucketMeta)}, []byte("a_key_nobody_classified")); err == nil {
		t.Fatal("an unclassified meta key was accepted")
	}
	if _, err := classify(nil, []byte("k")); err == nil {
		t.Fatal("a write with no bucket path was accepted")
	}
	// meta holds keys, not nested buckets, and a path that claims otherwise is
	// a caller bug rather than something to classify.
	if _, err := classify([]string{string(bucketMeta), "nested"}, []byte("k")); err == nil {
		t.Fatal("a nested bucket under meta was accepted")
	}
}

// TestNestedBucketsInheritTheirRoot. A deployment's price timeline lives in its
// own bucket under deployment_price_timeline; it is pricing wherever it is
// stored, and classifying only the root is what makes that true.
func TestNestedBucketsInheritTheirRoot(t *testing.T) {
	class, err := classify([]string{"deployment_price_timeline", "dep_1"}, []byte("v1"))
	if err != nil {
		t.Fatal(err)
	}
	if class != classAuthoritative || !class.replicated() {
		t.Fatalf("a nested price timeline classified as %s", class)
	}
}

// TestDerivedAndNodeLocalStateIsNotJournalled. Shipping a Primary's usage
// checkpoint or its learned route suspensions would state this node's
// observations as another node's.
func TestDerivedAndNodeLocalStateIsNotJournalled(t *testing.T) {
	for _, testCase := range []struct {
		path []string
		key  []byte
		want journalClass
	}{
		{[]string{"usage_daily_rollup"}, []byte("2026-09-24"), classNodeDerived},
		{[]string{"route_suspensions"}, []byte("scope"), classNodeDerived},
		{[]string{"audit_anchors"}, []byte("a1"), classNodeDerived},
		{[]string{string(bucketMeta)}, keyUsageCheckpoint, classNodeDerived},
		{[]string{string(bucketMeta)}, keyTokenGuardCheckpoint, classNodeDerived},
		{[]string{string(bucketMeta)}, keyShutdownTruncatedAttempts, classNodeLocal},
		{[]string{string(bucketMeta)}, keyAppliedJournalSequence, classJournalBookkeeping},
	} {
		class, err := classify(testCase.path, testCase.key)
		if err != nil {
			t.Fatal(err)
		}
		if class != testCase.want {
			t.Fatalf("%v/%s classified as %s, want %s", testCase.path, testCase.key, class, testCase.want)
		}
		if class.replicated() {
			t.Fatalf("%v/%s would be journalled", testCase.path, testCase.key)
		}
	}
}

// TestKeyEnvelopesAreJournalled. A Replica that cannot open the Ledger or the
// Audit log is not a Replica, and it cannot open either without these.
func TestKeyEnvelopesAreJournalled(t *testing.T) {
	for _, key := range [][]byte{
		keyVaultKeyring, keyKeySlotDescriptor, keyAuditHMACEnvelope,
		keyLedgerHMACEnvelope, keyVaultCheck, keyMetadataHMACEnvelope,
	} {
		class, err := classify([]string{string(bucketMeta)}, key)
		if err != nil {
			t.Fatal(err)
		}
		if class != classKeyEnvelope || !class.replicated() {
			t.Fatalf("meta key %q classified as %s", key, class)
		}
	}
}

// TestMixedTransactionsAreRefusedExceptTheUnderstoodShapes.
//
// A Replica applies only the recorded half of a transaction, so a node-local
// write the replicated half depends on would simply not happen there — and
// nothing would say so until a promotion.
func TestMixedTransactionsAreRefusedExceptTheUnderstoodShapes(t *testing.T) {
	set := func(names ...string) map[string]struct{} {
		out := map[string]struct{}{}
		for _, name := range names {
			out[name] = struct{}{}
		}
		return out
	}
	if err := mixedWriteAllowed(set("routes"), set("usage_daily_rollup")); err == nil {
		t.Fatal("a route write sharing a transaction with a rollup write was accepted")
	}
	if err := mixedWriteAllowed(set("routes"), nil); err != nil {
		t.Fatalf("an all-replicated transaction was refused: %v", err)
	}
	if err := mixedWriteAllowed(nil, set("usage_daily_rollup", "audit_anchors")); err != nil {
		t.Fatalf("an all-node-local transaction was refused: %v", err)
	}
	// Shape one: clearing a suspension and its audit intent.
	if err := mixedWriteAllowed(set("admin_audit_intents"), set("route_suspensions")); err != nil {
		t.Fatalf("clearing a route suspension with its audit intent was refused: %v", err)
	}
	if err := mixedWriteAllowed(set("admin_audit_intents", "routes"), set("route_suspensions")); err == nil {
		t.Fatal("the suspension shape widened to carry an unrelated authoritative write")
	}
	// Shape two: publishing the key state with the checkpoint that matches it.
	if err := mixedWriteAllowed(
		set("meta/key_slot_descriptor", "meta/vault_keyring", "meta/audit_hmac_envelope"),
		set("meta/audit_checkpoint"),
	); err != nil {
		t.Fatalf("publishing the key state with its audit checkpoint was refused: %v", err)
	}
	if err := mixedWriteAllowed(set("routes"), set("meta/audit_checkpoint")); err == nil {
		t.Fatal("the key-state shape widened to carry a route write")
	}
}

// TestMetaIsClassifiedPerKeyInTheCrossClassRule.
//
// `meta` holds authoritative settings, derived checkpoints, key envelopes and a
// node-local counter at once. A rule that asked only "which bucket" would find
// every `meta` transaction mixed and refuse all of them — which is exactly what
// the first implementation did, and what the identity below fixes.
func TestMetaIsClassifiedPerKeyInTheCrossClassRule(t *testing.T) {
	if got := writeIdentity([]string{string(bucketMeta)}, keyUsageCheckpoint); got != "meta/usage_checkpoint" {
		t.Fatalf("meta write identity is %q", got)
	}
	if got := writeIdentity([]string{"deployment_price_timeline", "dep_1"}, []byte("v1")); got != "deployment_price_timeline" {
		t.Fatalf("a nested bucket write identity is %q", got)
	}
}

// TestEveryCrossClassRuleNamesRealState. A rule that points at a bucket or key
// nobody has classified is a rule that will never fire, and it reads as though
// the case were handled.
func TestEveryCrossClassRuleNamesRealState(t *testing.T) {
	classified := func(identity string) bool {
		if bucket, key, found := strings.Cut(identity, "/"); found && bucket == string(bucketMeta) {
			_, known := metaKeyClasses[key]
			return known
		}
		_, known := bucketClasses[identity]
		return known
	}
	for _, rule := range crossClassWrites {
		if !classified(rule.local) {
			t.Fatalf("cross-class rule names %q, which has no journal class", rule.local)
		}
		for _, companion := range rule.with {
			if !classified(companion) {
				t.Fatalf("cross-class rule for %q names %q, which has no journal class", rule.local, companion)
			}
		}
	}
}
