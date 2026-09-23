package bolt

import (
	"fmt"
	"slices"
	"strings"
)

// journalClass is §5.2 of docs/todo/halro-ha-architecture.zh-CN.md: what a
// given (bucket, key) is, and therefore whether a write to it belongs in the
// metadata journal.
//
// The granularity is per key rather than per bucket because `meta` holds four
// classes at once — the schema version and the runtime settings are
// authoritative, the usage checkpoint is derived from this node's own Ledger,
// the vault keyring is a key envelope, and the shutdown counter is this node's
// telemetry backplane. A bucket-level table would have to pick one of those and
// would be wrong about the rest.
type journalClass uint8

const (
	// classUnknown is not a class. It is what classify returns for a bucket or
	// key no one has classified, and the entry refuses the transaction rather
	// than guessing. Guessing in the replicated direction ships bytes that
	// should stay local; guessing in the local direction silently drops an
	// authoritative write from the journal, which is the worse of the two and
	// invisible until a failover.
	classUnknown journalClass = iota
	// classAuthoritative (A) is metadata this instance owns and nothing else
	// can rebuild: Credentials, Projects, Routes, Deployments, pricing,
	// administrators, durable intents.
	classAuthoritative
	// classSession (B) is admin_sessions. Journalled like A, and invalidated
	// wholesale on promotion.
	classSession
	// classNodeDerived (C) is rebuildable from this node's own Ledger, Audit or
	// Governance log, or learned from this node's own traffic. It is never
	// journalled: a Replica advances its own from its own copies of those logs,
	// and shipping the Primary's would state its observations as the Replica's.
	classNodeDerived
	// classKeyEnvelope (D) is the wrapped subsystem keys and the format gates:
	// a node cannot open the Ledger or the Audit log without them. Journalled;
	// the Master Key itself travels out of band.
	classKeyEnvelope
	// classNodeLocal (E) is this node's own operational counters. Not
	// journalled, not accounting, not derived from any log.
	classNodeLocal
	// classJournalBookkeeping is the journal's own position in the projection.
	// It is written by the transaction entry itself, inside the same bbolt
	// transaction as the work it describes, and is never recorded as an
	// operation — a frame that recorded the sequence it was assigned would be
	// describing itself.
	classJournalBookkeeping
)

// replicated says whether a class's writes go into the journal.
func (c journalClass) replicated() bool {
	switch c {
	case classAuthoritative, classSession, classKeyEnvelope:
		return true
	}
	return false
}

func (c journalClass) String() string {
	switch c {
	case classAuthoritative:
		return "authoritative"
	case classSession:
		return "session"
	case classNodeDerived:
		return "node-derived"
	case classKeyEnvelope:
		return "key-envelope"
	case classNodeLocal:
		return "node-local"
	case classJournalBookkeeping:
		return "journal-bookkeeping"
	}
	return "unclassified"
}

// bucketClasses classifies every bucket but `meta`, whose keys are classified
// individually below. A nested bucket takes its root's class: a deployment's
// price timeline is part of pricing wherever it is stored.
//
// Adding a bucket without adding it here is caught the first time anything
// writes to it, by classify's refusal — which is deliberate. A default would
// make the omission invisible until a failover.
var bucketClasses = map[string]journalClass{
	// A — authoritative metadata.
	"credentials":                                  classAuthoritative,
	"projects":                                     classAuthoritative,
	"gateway_keys":                                 classAuthoritative,
	"gateway_key_hash":                             classAuthoritative,
	"providers":                                    classAuthoritative,
	"provider_egress_proxies":                      classAuthoritative,
	"deployments":                                  classAuthoritative,
	"routes":                                       classAuthoritative,
	"redaction_policies":                           classAuthoritative,
	"token_guard_policies":                         classAuthoritative,
	"alert_webhooks":                               classAuthoritative,
	"admin_users":                                  classAuthoritative,
	"admin_mfa_authenticators":                     classAuthoritative,
	"admin_mfa_recovery_codes":                     classAuthoritative,
	"admin_mfa_challenges":                         classAuthoritative,
	"provider_resources":                           classAuthoritative,
	"provider_resource_idempotency":                classAuthoritative,
	"deployment_price_versions":                    classAuthoritative,
	"deployment_price_timeline":                    classAuthoritative,
	"deployment_price_next_version":                classAuthoritative,
	"deployment_pricing_high_water":                classAuthoritative,
	"deployment_price_pin_intents":                 classAuthoritative,
	"deployment_price_proposals":                   classAuthoritative,
	"pricing_audit_intents":                        classAuthoritative,
	"pricing_idempotency":                          classAuthoritative,
	"pricing_proposal_idempotency":                 classAuthoritative,
	"pricing_migration_resolutions":                classAuthoritative,
	"admin_audit_intents":                          classAuthoritative,
	"cost_adjustment_intents":                      classAuthoritative,
	"model_capability_detections":                  classAuthoritative,
	"model_capability_detection_idempotency":       classAuthoritative,
	"model_capability_detection_fingerprint_index": classAuthoritative,
	"outcome_definitions":                          classAuthoritative,
	"run_governance_idempotency":                   classAuthoritative,
	"run_governance_index":                         classAuthoritative,
	"migration_history":                            classAuthoritative,

	// B — sessions. Replicated so a promotion does not have to mean every
	// administrator is signed out mid-incident; invalidated on promotion
	// anyway, which is a policy decision rather than a data one.
	"admin_sessions": classSession,

	// C — derived from this node's own authoritative logs, or from its own
	// traffic. Never journalled.
	"usage_checkpoint_segments":      classNodeDerived,
	"usage_daily_rollup":             classNodeDerived,
	"governance_checkpoint_segments": classNodeDerived,
	"audit_anchors":                  classNodeDerived,
	// route_suspensions is learned from what this node's own requests were
	// refused. A Replica has no traffic to learn from, and replicating the
	// Primary's observations would state them as the Replica's own; after a
	// promotion each target is re-learned at the cost of one failed request.
	"route_suspensions": classNodeDerived,
}

// metaKeyClasses classifies `meta`, which holds four classes at once.
var metaKeyClasses = map[string]journalClass{
	// A — authoritative.
	"schema_version":                   classAuthoritative,
	"runtime_settings":                 classAuthoritative,
	"instance_ui_settings":             classAuthoritative,
	"instance_usage_settings":          classAuthoritative,
	"instance_accounting_settings":     classAuthoritative,
	"instance_id":                      classAuthoritative,
	"admin_bootstrap_completion":       classAuthoritative,
	"key_slot_audit_intent":            classAuthoritative,
	"master_key_rotation_audit_intent": classAuthoritative,
	"minimum_ledger_reader_version":    classAuthoritative,
	"ledger_feature_epoch":             classAuthoritative,

	// C — derived from this node's own Ledger, Audit or Governance log.
	"usage_checkpoint":          classNodeDerived,
	"usage_rollup_state":        classNodeDerived,
	"token_guard_checkpoint":    classNodeDerived,
	"audit_checkpoint":          classNodeDerived,
	"ledger_chain_checkpoint":   classNodeDerived,
	"governance_checkpoint":     classNodeDerived,
	"governance_journal_anchor": classNodeDerived,

	// D — key envelopes and format gates. A node cannot open the Ledger or the
	// Audit log without these, so they travel with the metadata rather than
	// being rebuilt.
	"vault_keyring":          classKeyEnvelope,
	"key_slot_descriptor":    classKeyEnvelope,
	"audit_hmac_envelope":    classKeyEnvelope,
	"ledger_hmac_envelope":   classKeyEnvelope,
	"vault_key_check":        classKeyEnvelope,
	"metadata_hmac_envelope": classKeyEnvelope,

	// E — this node's own telemetry backplane. Not accounting, not derived
	// from any log, and meaningless on another node.
	"shutdown_truncated_attempts_total": classNodeLocal,

	// The journal's own position in the projection, written by the transaction
	// entry rather than by any caller.
	"applied_journal_sequence": classJournalBookkeeping,
	"metadata_journal_epoch":   classJournalBookkeeping,
}

// classify answers what a write to one (bucket path, key) is.
//
// It fails closed on anything it has not been told about, and the error names
// what was unclassified so the fix is a table entry rather than an
// investigation.
func classify(path []string, key []byte) (journalClass, error) {
	if len(path) == 0 {
		return classUnknown, fmt.Errorf("metadata write has no bucket path")
	}
	root := path[0]
	if root == string(bucketMeta) {
		if len(path) > 1 {
			return classUnknown, fmt.Errorf("meta holds keys, not buckets; refusing %q", strings.Join(path, "/"))
		}
		if len(key) == 0 {
			// A bucket-level operation on `meta` — creating or deleting the
			// bucket itself. Only the schema path does that, and it is part of
			// the projection the epoch publishes rather than an operation.
			return classAuthoritative, nil
		}
		class, known := metaKeyClasses[string(key)]
		if !known {
			return classUnknown, fmt.Errorf(
				"meta key %q has no journal class; classify it in metaKeyClasses before writing it", key)
		}
		return class, nil
	}
	class, known := bucketClasses[root]
	if !known {
		return classUnknown, fmt.Errorf(
			"bucket %q has no journal class; classify it in bucketClasses before writing it", root)
	}
	return class, nil
}

// writeIdentity names a write the way the cross-class rule has to see it.
//
// A bucket name is enough for every bucket but `meta`, which is the whole
// reason classification is per key: it holds authoritative settings, derived
// checkpoints, key envelopes and a node-local counter at once, so a rule that
// asked only "which bucket" would find every `meta` transaction mixed and
// refuse all of them.
func writeIdentity(path []string, key []byte) string {
	if path[0] == string(bucketMeta) {
		return string(bucketMeta) + "/" + string(key)
	}
	return path[0]
}

// crossClassWrites is every transaction in this package that writes both the
// replicated set and the node-local set, and what makes each of them safe.
//
// The design (§5.2) records one such transaction. Building the recorder found
// two: the survey below was taken by letting the check report instead of refuse
// and running the whole suite, rather than by reading the code and believing
// the result.
//
// Each rule names one node-local write and the complete set of replicated
// writes it may share a transaction with. Everything else is refused, because a
// Replica applies only the recorded half: a node-local write the replicated
// half depends on would simply not happen there, and nothing would say so until
// a promotion.
var crossClassWrites = []crossClassWrite{
	{
		// Clearing a route suspension is an administrative action, so the
		// removal and its Audit record have to commit together. Safe because a
		// Replica has no suspension to clear — it learns its own from its own
		// traffic, at the cost of one failed request per target after a
		// promotion.
		local: "route_suspensions",
		with:  []string{"admin_audit_intents"},
	},
	{
		// Key Slot initialization and the Master Key rewrite publish the whole
		// key state at once: a node holding the Audit HMAC envelope without a
		// matching checkpoint would have a key for a chain with no trusted
		// head. Safe because the checkpoint is a node's own — a Replica
		// advances it from its own Audit log — and at publish time it is the
		// empty one every node starts from anyway.
		local: "meta/audit_checkpoint",
		with: []string{
			"meta/key_slot_descriptor", "meta/vault_keyring", "meta/vault_key_check",
			"meta/audit_hmac_envelope", "meta/ledger_hmac_envelope", "meta/metadata_hmac_envelope",
			// The rewrite also invalidates every Admin identity and pre-auth
			// challenge in the same transaction as the ciphertext it rotates.
			"admin_users", "admin_sessions", "admin_mfa_challenges", "admin_mfa_recovery_codes",
			"admin_mfa_authenticators", "credentials", "meta/master_key_rotation_audit_intent",
			"meta/key_slot_audit_intent",
		},
	},
}

type crossClassWrite struct {
	local string
	with  []string
}

// mixedWriteAllowed says whether a transaction touching both sets is one of the
// understood shapes above.
func mixedWriteAllowed(replicated, local map[string]struct{}) error {
	if len(replicated) == 0 || len(local) == 0 {
		return nil
	}
	for name := range local {
		rule, known := findCrossClassWrite(name)
		if !known {
			return fmt.Errorf(
				"a metadata transaction may not write both replicated and node-local state; %q is node-local",
				name)
		}
		for companion := range replicated {
			if !slices.Contains(rule.with, companion) {
				return fmt.Errorf(
					"%q may not share a transaction with %q", name, companion)
			}
		}
	}
	return nil
}

func findCrossClassWrite(local string) (crossClassWrite, bool) {
	for _, rule := range crossClassWrites {
		if rule.local == local {
			return rule, true
		}
	}
	return crossClassWrite{}, false
}
