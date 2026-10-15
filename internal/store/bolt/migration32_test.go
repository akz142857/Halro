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

// Migration 32 replaces json_mode with json_object and structured_outputs.
//
// The record it has to bring forward cannot be written with today's structs,
// because today's structs no longer have the member. So it is written with them
// and then spelled backwards: every capability set and evidence set in the
// encoded record loses the two new names and gains the one they replaced, which
// is exactly what a v31 record on disk looks like.
//
// The reads are deliberately through the store rather than the raw bucket, and
// the assertions include Validate on both records, because capabilities and
// evidence are held to a biconditional: an enabled capability may not be
// unsupported and a disabled one may not be anything else. A migration that
// patched one side and not the other leaves a record that cannot be written
// back.
//
// It does not leave one that cannot be read: the store normalises an evidence
// set on the way out, so a stale json_mode entry is dropped there rather than
// refused. That is exactly why this is asserted here — the damage would surface
// on the operator's next save, far from the upgrade that caused it.
func TestMigration32SplitsJSONModeIntoItsTwoHalves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata-v31.db")
	now := time.Now().UTC().Truncate(time.Second)

	capabilities := domain.ProviderCapabilities{
		Chat: true, Streaming: true, StreamUsage: true, JSONObject: true, StructuredOutputs: true,
	}
	// The snapshot establishes more than the deployment uses, and the extra is
	// the capability being split: operator_disabled therefore names json_mode,
	// which is a name the dictionary stops carrying.
	established := capabilities
	established.Tools = true

	instance := domain.ProviderInstance{
		ID: "prov_v31", Name: "OpenAI", Type: domain.ProviderOpenAI,
		BaseURL: "https://api.openai.com", CredentialID: "cred_v31",
		AllowedHosts:  []string{"api.openai.com"},
		AccessSurface: domain.SurfaceOpenAI, ProfileID: domain.ProfileOpenAIChatEmbeddings,
		CredentialScheme: domain.CredentialBearerStatic,
		Capabilities:     capabilities,
		CapabilityEvidence: domain.EvidenceForCapabilities(
			capabilities, domain.EvidenceDeclared),
		Bindings: []domain.ProviderProfileBinding{{
			ID:         domain.DefaultProviderProfileBindingID("prov_v31", domain.ProfileOpenAIChatEmbeddings),
			ProviderID: "prov_v31", AccessSurface: domain.SurfaceOpenAI,
			ProfileID:        domain.ProfileOpenAIChatEmbeddings,
			CredentialScheme: domain.CredentialBearerStatic,
			Capabilities:     capabilities,
			CapabilityEvidence: domain.EvidenceForCapabilities(
				capabilities, domain.EvidenceDeclared),
			Enabled: true,
		}},
		Enabled: true, CreatedAt: now, UpdatedAt: now, Revision: 1,
	}
	deployment := domain.Deployment{
		ID: "dep_v31", Name: "GPT", ProviderID: instance.ID, ProviderModel: "gpt-test",
		TargetKind: domain.TargetModelID, AccessSurface: domain.SurfaceOpenAI,
		ProfileID: domain.ProfileOpenAIChatEmbeddings, Capabilities: capabilities,
		CapabilityEvidence: domain.EvidenceForCapabilities(capabilities, domain.EvidenceDeclared),
		ModelCapabilitySnapshot: domain.ModelCapabilitySnapshot{
			ProviderModel: "gpt-test", ModelRevision: "sha256:test", Source: "operator_declared",
			Status: "partial", CapturedAt: now, Capabilities: established,
			Evidence: domain.EvidenceForCapabilities(established, domain.EvidenceDeclared),
		},
		OperatorDisabled: []string{"json_mode", "tools"},
		MaxConcurrency:   1, CreatedAt: now, UpdatedAt: now, Revision: 1,
	}
	writeV31Records(t, path, instance, deployment)

	store, err := Open(path)
	if err != nil {
		t.Fatalf("a v31 directory was refused rather than brought forward: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	migratedProvider, err := store.GetProvider(ctx, instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Off for everyone, the same shape migrations 28 and 31 used: the old switch
	// cannot say which half its target had, and off refuses a request where on
	// would forward one the upstream rejects after the budget is reserved.
	assertJSONHalvesOff(t, "provider", migratedProvider.Capabilities, migratedProvider.CapabilityEvidence)
	if len(migratedProvider.Bindings) != 1 {
		t.Fatalf("bindings=%#v", migratedProvider.Bindings)
	}
	assertJSONHalvesOff(t, "binding",
		migratedProvider.Bindings[0].Capabilities, migratedProvider.Bindings[0].CapabilityEvidence)

	migratedDeployment, err := store.GetDeployment(ctx, deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONHalvesOff(t, "deployment",
		migratedDeployment.Capabilities, migratedDeployment.CapabilityEvidence)
	assertJSONHalvesOff(t, "snapshot",
		migratedDeployment.ModelCapabilitySnapshot.Capabilities,
		migratedDeployment.ModelCapabilitySnapshot.Evidence)
	// The name the dictionary dropped is gone from the switched-off list, and the
	// name it never carried is untouched. Leaving json_mode there would fail
	// validation on the next read for a reason nothing on screen explains.
	if len(migratedDeployment.OperatorDisabled) != 1 || migratedDeployment.OperatorDisabled[0] != "tools" {
		t.Fatalf("operator_disabled=%#v", migratedDeployment.OperatorDisabled)
	}
	if err := migratedDeployment.Validate(); err != nil {
		t.Fatalf("migrated deployment does not validate: %v", err)
	}
	if err := migratedProvider.Validate(); err != nil {
		t.Fatalf("migrated provider does not validate: %v", err)
	}

	// A stored detection is keyed by capability name and fingerprinted with the
	// detector contract version, both of which moved. Reusing one would answer a
	// question no longer asked.
	detections, err := store.ListModelCapabilityDetections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(detections) != 0 {
		t.Fatalf("detections survived the split: %#v", detections)
	}
}

func assertJSONHalvesOff(t *testing.T, where string, capabilities domain.ProviderCapabilities, evidence domain.CapabilityEvidenceSet) {
	t.Helper()
	if capabilities.JSONObject || capabilities.StructuredOutputs {
		t.Fatalf("%s: json_mode was carried into a half it could not establish: %#v", where, capabilities)
	}
	for _, name := range jsonModeSuccessors {
		if evidence[name] != domain.EvidenceUnsupported {
			t.Fatalf("%s: evidence for %s is %q", where, name, evidence[name])
		}
	}
	if _, present := evidence["json_mode"]; present {
		t.Fatalf("%s: evidence still names json_mode: %#v", where, evidence)
	}
	if err := evidence.Validate(capabilities); err != nil {
		t.Fatalf("%s: migrated evidence does not validate: %v", where, err)
	}
}

// writeV31Records lays down one provider and one deployment as v31 wrote them,
// then stamps the schema version back so the next Open runs the migration.
func writeV31Records(t *testing.T, path string, instance domain.ProviderInstance, deployment domain.Deployment) {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = store.db.Update(func(tx *bbolt.Tx) error {
		providerRecord, err := asV31Record(instance)
		if err != nil {
			return err
		}
		if err := spellJSONModeBackwards(providerRecord, "capabilities", "capability_evidence"); err != nil {
			return err
		}
		if err := patchArrayMember(providerRecord, "bindings", func(binding map[string]json.RawMessage) error {
			return spellJSONModeBackwards(binding, "capabilities", "capability_evidence")
		}); err != nil {
			return err
		}
		if err := putV31Record(tx, bucketProviders, instance.ID, providerRecord); err != nil {
			return err
		}

		deploymentRecord, err := asV31Record(deployment)
		if err != nil {
			return err
		}
		if err := spellJSONModeBackwards(deploymentRecord, "capabilities", "capability_evidence"); err != nil {
			return err
		}
		var snapshot map[string]json.RawMessage
		if err := json.Unmarshal(deploymentRecord["model_capability_snapshot"], &snapshot); err != nil {
			return err
		}
		if err := spellJSONModeBackwards(snapshot, "capabilities", "evidence"); err != nil {
			return err
		}
		encodedSnapshot, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		deploymentRecord["model_capability_snapshot"] = encodedSnapshot
		if err := putV31Record(tx, bucketDeployments, deployment.ID, deploymentRecord); err != nil {
			return err
		}

		var version [8]byte
		binary.BigEndian.PutUint64(version[:], 31)
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, version[:])
	})
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}

func asV31Record(value any) (map[string]json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &record); err != nil {
		return nil, err
	}
	return record, nil
}

func putV31Record(tx *bbolt.Tx, bucket []byte, id string, record map[string]json.RawMessage) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return tx.Bucket(bucket).Put([]byte(id), encoded)
}

// spellJSONModeBackwards turns one record's capability and evidence sets into
// what v31 would have stored: the two successors are removed and the member
// they replaced takes their place, carrying whatever they agreed on.
func spellJSONModeBackwards(record map[string]json.RawMessage, capabilityField, evidenceField string) error {
	if err := rewriteMember(record, capabilityField, func(set map[string]json.RawMessage) {
		enabled := string(set["json_object"]) == "true" || string(set["structured_outputs"]) == "true"
		delete(set, "json_object")
		delete(set, "structured_outputs")
		set["json_mode"] = json.RawMessage("false")
		if enabled {
			set["json_mode"] = json.RawMessage("true")
		}
	}); err != nil {
		return err
	}
	return rewriteMember(record, evidenceField, func(set map[string]json.RawMessage) {
		evidence := set["json_object"]
		if string(evidence) == `"`+string(domain.EvidenceUnsupported)+`"` {
			evidence = set["structured_outputs"]
		}
		delete(set, "json_object")
		delete(set, "structured_outputs")
		set["json_mode"] = evidence
	})
}

func rewriteMember(record map[string]json.RawMessage, field string, patch func(map[string]json.RawMessage)) error {
	var set map[string]json.RawMessage
	if err := json.Unmarshal(record[field], &set); err != nil {
		return err
	}
	patch(set)
	encoded, err := json.Marshal(set)
	if err != nil {
		return err
	}
	record[field] = encoded
	return nil
}
