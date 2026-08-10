package modelcatalog

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

func testSigningKey(t *testing.T, keyID string, now time.Time) (ed25519.PrivateKey, TrustRoot) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return privateKey, TrustRoot{KeyID: keyID, PublicKey: publicKey, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
}

func testSnapshot(now time.Time, sequence uint64, entries ...SnapshotEntry) Snapshot {
	return Snapshot{
		SchemaVersion: MinReadableSchema, Sequence: sequence,
		GeneratedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		CapabilityDictionaryVersion: CapabilityDictionaryVersion, Entries: entries,
	}
}

func signedSnapshotJSON(t *testing.T, snapshot Snapshot, keyID string, privateKey ed25519.PrivateKey) []byte {
	t.Helper()
	snapshot, payload, err := PrepareSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	signed := SignedSnapshot{Payload: snapshot, Signatures: []Signature{{
		KeyID: keyID, Algorithm: SignatureAlgorithmEd25519,
		Value: base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}}}
	encoded, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func dynamicOpenAIEntry(model string) SnapshotEntry {
	return SnapshotEntry{
		ProviderType: domain.ProviderOpenAI, ProfileID: domain.ProfileOpenAIChatEmbeddings,
		TargetKind: domain.TargetModelID, Model: model,
		Capabilities: domain.ProviderCapabilities{Chat: true, Streaming: true, MaxContextTokens: 8192},
	}
}

func TestBundledSnapshotUsesRemoteEntrySchema(t *testing.T) {
	snapshot := BundledSnapshot()
	if snapshot.SchemaVersion != MinReadableSchema || snapshot.CapabilityDictionaryVersion != CapabilityDictionaryVersion {
		t.Fatalf("bundled schema=%#v", snapshot)
	}
	revision, err := ComputeSnapshotRevision(snapshot)
	if len(snapshot.Entries) != Builtin().Len() || err != nil || snapshot.CatalogRevision != revision {
		t.Fatalf("bundled snapshot drifted from builtin catalog")
	}
	for _, entry := range snapshot.Entries {
		if entry.ProviderType == "" || entry.ProfileID == "" || entry.Model == "" {
			t.Fatalf("bundled entry is incomplete: %#v", entry)
		}
	}
}

func TestSignedSnapshotAddsExactModelWithoutNameInference(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	privateKey, root := testSigningKey(t, "root-a", now)
	payload := signedSnapshotJSON(t, testSnapshot(now, 1, dynamicOpenAIEntry("gpt-5.future-exact")), root.KeyID, privateKey)
	snapshot, _, err := DecodeAndVerifySignedSnapshot(payload, VerifyOptions{Now: now, TrustRoots: []TrustRoot{root}, MaxEntries: 100})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := MergeSnapshots(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	entry, found := merged.Lookup(Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, TargetKind: domain.TargetModelID, Model: "gpt-5.future-exact"})
	if !found || entry.Source != SourceSignedCatalog || !entry.Source.PreselectsCapabilities() || !entry.Capabilities.Chat {
		t.Fatalf("dynamic entry=%#v found=%v", entry, found)
	}
	if _, found := merged.Lookup(Key{ProviderType: domain.ProviderOpenAI, Profile: domain.ProfileOpenAIChatEmbeddings, TargetKind: domain.TargetModelID, Model: "gpt-5.future-exact-alias"}); found {
		t.Fatal("dynamic catalog widened an exact model name")
	}
}

func TestSignedSnapshotRejectsVersionExpiryPinAndUnknownCapability(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	privateKey, root := testSigningKey(t, "root-a", now)
	base := testSnapshot(now, 1, dynamicOpenAIEntry("gpt-future"))
	tests := []struct {
		name   string
		mutate func(*Snapshot)
		pin    string
		want   string
	}{
		{"future schema", func(value *Snapshot) { value.SchemaVersion = MaxReadableSchema + 1 }, "", "schema"},
		{"old schema", func(value *Snapshot) { value.SchemaVersion = MinReadableSchema - 1 }, "", "schema"},
		{"dictionary", func(value *Snapshot) { value.CapabilityDictionaryVersion++ }, "", "dictionary"},
		{"expired", func(value *Snapshot) { value.ExpiresAt = now.Add(-time.Second) }, "", "expired"},
		{"future", func(value *Snapshot) { value.GeneratedAt = now.Add(10 * time.Minute) }, "", "future"},
		{"pin", func(*Snapshot) {}, "sha256:" + strings.Repeat("0", 64), "pinned"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := base
			test.mutate(&snapshot)
			payload := signedSnapshotJSON(t, snapshot, root.KeyID, privateKey)
			_, _, err := DecodeAndVerifySignedSnapshot(payload, VerifyOptions{Now: now, TrustRoots: []TrustRoot{root}, MaxEntries: 100, PinnedRevision: test.pin})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v, want %q", err, test.want)
			}
		})
	}
	payload := signedSnapshotJSON(t, base, root.KeyID, privateKey)
	payload = []byte(strings.Replace(string(payload), `"capabilities":{"chat":true`, `"capabilities":{"future_protocol":true,"chat":true`, 1))
	if _, _, err := DecodeAndVerifySignedSnapshot(payload, VerifyOptions{Now: now, TrustRoots: []TrustRoot{root}, MaxEntries: 100}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown capability was not rejected: %v", err)
	}
	tamperedSignature := signedSnapshotJSON(t, base, root.KeyID, privateKey)
	var envelope SignedSnapshot
	if err := json.Unmarshal(tamperedSignature, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope.Signatures[0].Value = strings.Repeat("A", 88)
	tamperedSignature, _ = json.Marshal(envelope)
	if _, _, err := DecodeAndVerifySignedSnapshot(tamperedSignature, VerifyOptions{Now: now, TrustRoots: []TrustRoot{root}, MaxEntries: 100}); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("tampered signature was not rejected: %v", err)
	}
}

func TestSignedSnapshotTrustRootOverlapAndRetirement(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	oldPrivate, oldRoot := testSigningKey(t, "old", now)
	newPrivate, newRoot := testSigningKey(t, "new", now)
	snapshot := testSnapshot(now, 1, dynamicOpenAIEntry("gpt-future"))
	for _, signed := range [][]byte{
		signedSnapshotJSON(t, snapshot, oldRoot.KeyID, oldPrivate),
		signedSnapshotJSON(t, snapshot, newRoot.KeyID, newPrivate),
	} {
		if _, _, err := DecodeAndVerifySignedSnapshot(signed, VerifyOptions{Now: now, TrustRoots: []TrustRoot{oldRoot, newRoot}, MaxEntries: 100}); err != nil {
			t.Fatalf("overlap signature rejected: %v", err)
		}
	}
	oldRoot.NotAfter = now
	if _, _, err := DecodeAndVerifySignedSnapshot(signedSnapshotJSON(t, snapshot, oldRoot.KeyID, oldPrivate), VerifyOptions{Now: now, TrustRoots: []TrustRoot{oldRoot, newRoot}, MaxEntries: 100}); err == nil {
		t.Fatal("retired trust root was accepted")
	}
}

func TestAssembleRejectsPrivateKeyBytesPassedAsDetachedSignature(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	privateKey, root := testSigningKey(t, "root", now)
	_, payload, err := PrepareSnapshot(testSnapshot(now, 1, dynamicOpenAIEntry("gpt-future")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AssembleSignedSnapshot(payload, root.KeyID, base64.StdEncoding.EncodeToString(privateKey), []TrustRoot{root}); err == nil {
		t.Fatal("64-byte private key was accepted as a detached signature")
	}
	signature := ed25519.Sign(privateKey, payload)
	if _, err := AssembleSignedSnapshot(payload, root.KeyID, base64.StdEncoding.EncodeToString(signature), []TrustRoot{root}); err != nil {
		t.Fatalf("valid detached signature rejected: %v", err)
	}
}

func TestSnapshotValidityMustFitSigningRootValidity(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	privateKey, root := testSigningKey(t, "root", now)
	snapshot := testSnapshot(now, 1, dynamicOpenAIEntry("gpt-future"))
	root.NotAfter = snapshot.ExpiresAt.Add(-time.Second)
	payload := signedSnapshotJSON(t, snapshot, root.KeyID, privateKey)
	if _, _, err := DecodeAndVerifySignedSnapshot(payload, VerifyOptions{Now: now, TrustRoots: []TrustRoot{root}, MaxEntries: 100}); err == nil {
		t.Fatal("snapshot validity exceeded signing-root validity")
	}
}

func TestSignedRevocationRemovesOnlyExactBundledEntry(t *testing.T) {
	entry := Builtin().Entries()[0]
	snapshot := Snapshot{Entries: []SnapshotEntry{{ProviderType: entry.Key.ProviderType, ProfileID: entry.Key.Profile, TargetKind: defaultTargetKind(entry.Key.ProviderType, entry.Key.Profile), Model: entry.Key.Model, Region: entry.Key.Region, Revoked: true}}}
	merged, err := MergeSnapshots(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := merged.Lookup(entry.Key); found {
		t.Fatal("exact revoked entry remained present")
	}
	if merged.Len() != Builtin().Len()-1 {
		t.Fatalf("revocation removed unrelated entries: len=%d", merged.Len())
	}
}

func TestRevokedEntryCannotCarryFeatureOrLimitClaims(t *testing.T) {
	for _, capabilities := range []domain.ProviderCapabilities{{Tools: true}, {MaxContextTokens: 1}} {
		item := dynamicOpenAIEntry("gpt-revoked")
		item.Revoked = true
		item.Capabilities = capabilities
		if _, err := MergeSnapshots(Snapshot{Entries: []SnapshotEntry{item}}); err == nil {
			t.Fatalf("revoked capability claims accepted: %#v", capabilities)
		}
	}
}

func TestSnapshotUnsupportedCanonicalizationIsSemantic(t *testing.T) {
	item := dynamicOpenAIEntry("gpt-unsupported")
	item.Unsupported = []string{"vision", "tools", "vision"}
	snapshot, _, err := PrepareSnapshot(testSnapshot(time.Now().UTC(), 1, item))
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Entries[0].Unsupported; !slices.Equal(got, []string{"tools", "vision"}) {
		t.Fatalf("unsupported not normalized: %#v", got)
	}
	catalog, err := catalogFromSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	entry, found := catalog.Lookup(Key{ProviderType: item.ProviderType, Profile: item.ProfileID, TargetKind: item.TargetKind, Model: item.Model})
	if !found || entry.Revision() == "" {
		t.Fatalf("normalized entry missing: %#v", entry)
	}
}

func TestSignedTargetKindDoesNotCrossInvocationKinds(t *testing.T) {
	item := SnapshotEntry{ProviderType: domain.ProviderBedrock, ProfileID: domain.ProfileBedrockConverseText, TargetKind: domain.TargetBedrockFoundationModel, Model: "shared-id", Capabilities: domain.ProviderCapabilities{Chat: true}}
	catalog, err := catalogFromSnapshot(Snapshot{Entries: []SnapshotEntry{item}})
	if err != nil {
		t.Fatal(err)
	}
	if _, found := catalog.Lookup(Key{ProviderType: item.ProviderType, Profile: item.ProfileID, TargetKind: domain.TargetBedrockInferenceProfile, Model: item.Model}); found {
		t.Fatal("signed foundation-model claim crossed into inference-profile scope")
	}
}

func TestSignedRevocationStillRequiresARegisteredExactIdentity(t *testing.T) {
	for _, item := range []SnapshotEntry{
		{ProviderType: domain.ProviderOpenAI, ProfileID: domain.ProfileOpenAIChatEmbeddings, TargetKind: domain.TargetModelID, Model: "", Revoked: true},
		{ProviderType: domain.ProviderOpenAI, ProfileID: domain.ProfileGeminiText, TargetKind: domain.TargetModelID, Model: "cross-provider", Revoked: true},
		{ProviderType: domain.ProviderOpenAI, ProfileID: domain.ProfileOpenAIChatEmbeddings, TargetKind: domain.TargetModelID, Model: "model", Region: " us-east-1", Revoked: true},
	} {
		if _, err := MergeSnapshots(Snapshot{Entries: []SnapshotEntry{item}}); err == nil {
			t.Fatalf("invalid revoked identity was accepted: %#v", item)
		}
	}
}
