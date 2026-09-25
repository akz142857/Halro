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

// TestCallerIdempotencyReplicatesWithTheJournal.
//
// HA Phase 0b (#106) requires that the caller idempotency contract for
// Chat/Embeddings keeps its lifecycle records in a class-A bucket, so they
// travel with the journal rather than being rebuilt per node. The design says
// why: a Replica promoted after a Primary failed must answer a retried
// Idempotency-Key with the original resource, and it can only do that if the
// record reached it.
//
// The contract is already satisfied — both buckets are authoritative today, so
// this test passes the day it is written. That is the point of writing it now.
// Nothing else states the dependency: the classification table reads as a list
// of buckets, and moving either of these to class C or E would be a one-line
// edit that looks local, passes every other test, and silently makes a retried
// key answerable on one node and not on another. The failure would first appear
// during a promotion, which is the worst place to learn it.
func TestCallerIdempotencyReplicatesWithTheJournal(t *testing.T) {
	// The state `store_providers.go` reads and writes to answer a repeated
	// Idempotency-Key: the resource itself, and the index from key to resource.
	for _, bucket := range []string{
		string(bucketProviderResources),
		string(bucketProviderResourceIdem),
	} {
		class, known := bucketClasses[bucket]
		if !known {
			t.Fatalf("%s has no journal class; caller idempotency writes to it", bucket)
		}
		if class != classAuthoritative {
			t.Errorf("%s is class %v; caller idempotency needs it journalled, or a promoted "+
				"Replica cannot answer a retried Idempotency-Key with the original resource "+
				"(HA design §5.2, #106)", bucket, class)
		}
	}
}

// TestWithdrawnAuthorityCannotSurviveAPromotion.
//
// The class table pins its other three classes already — C and E through
// TestDerivedAndNodeLocalStateIsNotJournalled, D through
// TestKeyEnvelopesAreJournalled. Class A is the largest and the only one with
// nothing holding its security-critical members in place, which is backwards:
// a bucket wrongly marked derived is not merely rebuilt somewhere, it is a
// bucket whose *removals* never reach the other node.
//
// That is the failure this names. Halro's answer to "is this request allowed"
// is stored, not computed: a Gateway Key is disabled by a flag on its record,
// an admin is disabled the same way, an MFA recovery code is spent by being
// consumed, a Provider credential is withdrawn by being deleted. Every one of
// those is a *withdrawal*, and a withdrawal that does not replicate is a
// promoted Replica honouring access the Primary had already taken away — the
// exact fail-open this project refuses everywhere else.
//
// It reads as obvious, which is why it is worth writing down: each of these is
// one word in a table, and changing that word looks like a local decision
// about where some state belongs.
func TestWithdrawnAuthorityCannotSurviveAPromotion(t *testing.T) {
	for _, subject := range []struct {
		bucket    []byte
		withdraws string
	}{
		{bucketGatewayKeys, "a Gateway Key disabled on the Primary"},
		{bucketGatewayKeyHash, "the lookup that finds that key, which would resolve to it regardless"},
		{bucketAdminUsers, "an administrator disabled or deleted"},
		{bucketAdminMFAAuthenticators, "a second factor unenrolled"},
		{bucketAdminMFARecoveryCodes, "a one-time recovery code already spent"},
		{bucketCredentials, "a Provider credential withdrawn"},
		{bucketProjects, "a Project's CIDR allowlist, budget or limits tightened"},
		{bucketTokenGuardPolicies, "a Token Guard policy tightened"},
		{bucketRedactionPolicies, "a redaction rule added"},
	} {
		name := string(subject.bucket)
		class, known := bucketClasses[name]
		if !known {
			t.Errorf("%s has no journal class", name)
			continue
		}
		if !class.replicated() {
			t.Errorf("%s is class %v, so %s would not reach a Replica; "+
				"a promotion would then honour what the Primary had withdrawn",
				name, class, subject.withdraws)
		}
	}
}

// crossClassWriteBudget is how many transactions in this package are allowed to
// write both sets at once.
//
// Two, and the number is here so that a third has to be typed deliberately.
// Each entry is an exemption from the fail-closed refusal that is the whole
// point of the classification: a Replica applies only the recorded half, so
// every exemption is a promise that the unrecorded half does not matter there.
// Both existing promises are argued in the list itself, and both were found by
// measuring rather than by reading the code — a third that arrives without the
// same argument is how the guarantee erodes one convenient transaction at a
// time.
//
// Raising this is allowed. Raising it silently is not.
const crossClassWriteBudget = 2

func TestTheCrossClassAllowlistDoesNotGrowByItself(t *testing.T) {
	if len(crossClassWrites) != crossClassWriteBudget {
		t.Fatalf("crossClassWrites holds %d rules, budget is %d; "+
			"a new exemption needs its safety argued in the list and this number moved "+
			"in the same change, with the reason in the commit",
			len(crossClassWrites), crossClassWriteBudget)
	}
}

// TestEveryCrossClassRuleIsShapedLikeItsName.
//
// TestEveryCrossClassRuleNamesRealState next door checks that a rule names
// state that exists. It does not check that the state is on the side the rule
// claims, and the rule only means anything if it is: a `local` that is actually
// replicated exempts a transaction that never needed exempting, and hides that
// the real mixed write is somewhere else. A `with` entry that is not replicated
// is two node-local writes being called a cross-class transaction, which is the
// same mistake read from the other end.
func TestEveryCrossClassRuleIsShapedLikeItsName(t *testing.T) {
	classOf := func(t *testing.T, identity string) journalClass {
		t.Helper()
		if bucket, key, found := strings.Cut(identity, "/"); found && bucket == string(bucketMeta) {
			class, known := metaKeyClasses[key]
			if !known {
				t.Fatalf("%q has no journal class", identity)
			}
			return class
		}
		class, known := bucketClasses[identity]
		if !known {
			t.Fatalf("%q has no journal class", identity)
		}
		return class
	}
	for _, rule := range crossClassWrites {
		if classOf(t, rule.local).replicated() {
			t.Errorf("cross-class rule calls %q its node-local write, but that state replicates; "+
				"the rule exempts a transaction that did not need exempting", rule.local)
		}
		for _, companion := range rule.with {
			if !classOf(t, companion).replicated() {
				t.Errorf("cross-class rule for %q lists %q as a replicated companion, but that "+
					"state does not replicate; nothing about this transaction crosses classes",
					rule.local, companion)
			}
		}
	}
}
