package bolt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Heimdall/internal/domain"
	"github.com/akz142857/Heimdall/internal/ledger"
	bbolt "go.etcd.io/bbolt"
)

func TestAdminMFAChallengeClaimAndAuthenticatorInvariantsAreAtomic(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	hash := sha256.Sum256([]byte("challenge"))
	challenge := domain.AdminMFAChallenge{IDHash: hash, Username: "admin", Purpose: domain.AdminMFAChallengeLogin, CreatedAt: now, ExpiresAt: now.Add(time.Minute), AttemptsRemaining: 5, SessionGeneration: 1}
	if err := store.PutAdminMFAChallenge(context.Background(), challenge); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.ClaimAdminMFAChallenge(context.Background(), hash, now)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("challenge claims succeeded=%d", successes)
	}
	if err := store.FailAdminMFAChallenge(context.Background(), hash); err != nil {
		t.Fatal(err)
	}
	replacementHash := sha256.Sum256([]byte("replacement-challenge"))
	replacement := challenge
	replacement.IDHash, replacement.CreatedAt, replacement.ExpiresAt = replacementHash, now.Add(time.Second), now.Add(time.Minute+time.Second)
	if err := store.PutAdminMFAChallenge(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetAdminMFAChallenge(context.Background(), hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old challenge survived replacement: %v", err)
	}
	storedReplacement, err := store.GetAdminMFAChallenge(context.Background(), replacementHash)
	if err != nil || storedReplacement.AttemptsRemaining != 4 {
		t.Fatalf("replacement attempts=%d err=%v", storedReplacement.AttemptsRemaining, err)
	}

	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("active-%d", i)
		confirmed := now
		_, err := store.PutAdminMFAAuthenticator(context.Background(), domain.AdminMFAAuthenticator{ID: id, Username: "admin", Name: id, Type: domain.AdminMFATypeTOTP, SecretCiphertext: []byte("ciphertext"), Status: domain.AdminMFAStatusActive, CreatedAt: now, ConfirmedAt: &confirmed}, 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	expiry := now.Add(time.Minute)
	_, err = store.PutAdminMFAAuthenticator(context.Background(), domain.AdminMFAAuthenticator{ID: "pending", Username: "admin", Name: "pending", Type: domain.AdminMFATypeTOTP, SecretCiphertext: []byte("ciphertext"), Status: domain.AdminMFAStatusPending, CreatedAt: now, ExpiresAt: &expiry}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateAdminMFAAuthenticator(context.Background(), "admin", "pending", 1, now, 5); !errors.Is(err, ErrMFALimit) {
		t.Fatalf("activate over limit err=%v", err)
	}

	for i := 1; i < 5; i++ {
		if err := store.RevokeAdminMFAAuthenticator(context.Background(), "admin", fmt.Sprintf("active-%d", i), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RevokeAdminMFAAuthenticator(context.Background(), "admin", "active-0", true); !errors.Is(err, ErrMFARequired) {
		t.Fatalf("required last revoke err=%v", err)
	}
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("race-%d", i)
		confirmed := now
		_, err := store.PutAdminMFAAuthenticator(context.Background(), domain.AdminMFAAuthenticator{ID: id, Username: "race-admin", Name: id, Type: domain.AdminMFATypeTOTP, SecretCiphertext: []byte("ciphertext"), Status: domain.AdminMFAStatusActive, CreatedAt: now, ConfirmedAt: &confirmed}, 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	results = make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			results <- store.RevokeAdminMFAAuthenticator(context.Background(), "race-admin", id, true)
		}(fmt.Sprintf("race-%d", i))
	}
	wg.Wait()
	close(results)
	revoked := 0
	for err := range results {
		if err == nil {
			revoked++
		}
	}
	if revoked != 1 {
		t.Fatalf("concurrent required revocations succeeded=%d", revoked)
	}
}

func TestAdminMFAEnrollmentCommitsRecoveryIdentityAndAuditTogether(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	user := domain.AdminUser{
		Username: "admin", Locale: "en-US", PasswordVersion: 1,
		PasswordSalt: bytes.Repeat([]byte{1}, 16), PasswordHash: bytes.Repeat([]byte{2}, 32),
		ArgonMemoryKiB: 64 * 1024, ArgonIterations: 3, ArgonParallelism: 1,
		SessionGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, err = store.PutAdminUser(ctx, user, 0); err != nil {
		t.Fatal(err)
	}
	expires := now.Add(time.Minute)
	authenticator := domain.AdminMFAAuthenticator{ID: "mfa_1", Username: user.Username, Name: "Phone", Type: domain.AdminMFATypeTOTP, SecretCiphertext: []byte("ciphertext"), Status: domain.AdminMFAStatusPending, CreatedAt: now, ExpiresAt: &expires}
	if _, err = store.PutAdminMFAAuthenticator(ctx, authenticator, 0); err != nil {
		t.Fatal(err)
	}
	codeHash := sha256.Sum256([]byte("recovery"))
	codes := []domain.AdminMFARecoveryCode{{ID: "mrc_1", Username: user.Username, CodeHash: codeHash, CreatedAt: now, Generation: 1}}
	intent := domain.AdminMFAAuditIntent{EventID: "aud_1", OccurredAt: now, ActorID: user.Username, Action: "admin.mfa.authenticator.added", TargetType: "admin_mfa_authenticator", TargetID: authenticator.ID}
	rotated, first, err := store.ConfirmAdminMFAEnrollment(ctx, user.Username, authenticator.ID, 42, now, 5, codes, intent)
	if err != nil || !first || rotated.SessionGeneration != 2 {
		t.Fatalf("rotated=%#v first=%v err=%v", rotated, first, err)
	}
	storedAuthenticator, err := store.GetAdminMFAAuthenticator(ctx, user.Username, authenticator.ID)
	if err != nil || storedAuthenticator.Status != domain.AdminMFAStatusActive || storedAuthenticator.LastAcceptedTimeStep != 42 {
		t.Fatalf("authenticator=%#v err=%v", storedAuthenticator, err)
	}
	if remaining, err := store.CountUnusedAdminMFARecoveryCodes(ctx, user.Username); err != nil || remaining != 1 {
		t.Fatalf("remaining=%d err=%v", remaining, err)
	}
	pending, err := store.ListPendingAdminMFAAudits(ctx)
	if err != nil || len(pending) != 1 || pending[0].EventID != intent.EventID {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	other := intent
	other.EventID = "aud_2"
	if _, err = store.ReplaceAdminMFARecoveryCodesAndRotate(ctx, user.Username, codes, other); err == nil {
		t.Fatal("a second lifecycle mutation overwrote an undelivered audit intent")
	}
	unchanged, err := store.GetAdminUser(ctx, user.Username)
	if err != nil || unchanged.SessionGeneration != 2 || unchanged.PendingMFAAudit == nil || unchanged.PendingMFAAudit.EventID != intent.EventID {
		t.Fatalf("user after rejected mutation=%#v err=%v", unchanged, err)
	}
}

func TestMetadataMigrationFromV1IsAtomicAndRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	createV1Metadata(t, path)
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion()
	if err != nil || version != schemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	history, err := store.MigrationHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 8 ||
		history[0] != (MigrationRecord{Version: 1, Name: "initial_schema"}) ||
		history[1] != (MigrationRecord{Version: 2, Name: "migration_history"}) ||
		history[2] != (MigrationRecord{Version: 3, Name: "deployments"}) ||
		history[3] != (MigrationRecord{Version: 4, Name: "provider_profiles"}) ||
		history[4] != (MigrationRecord{Version: 5, Name: "provider_resources"}) ||
		history[5] != (MigrationRecord{Version: 6, Name: "phase2_capability_evidence"}) ||
		history[6] != (MigrationRecord{Version: 7, Name: "provider_resource_creation_status"}) ||
		history[7] != (MigrationRecord{Version: 8, Name: "admin_mfa"}) {
		t.Fatalf("history=%#v", history)
	}
}

func TestProviderProfileMigrationFromV3IsAtomicAndConservative(t *testing.T) {
	root := t.TempDir()
	templatePath := filepath.Join(root, "metadata-v3.db")
	createV3ProviderMetadata(t, templatePath)
	template, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, killPoint := range []string{"before_migrate_provider_profiles", "after_migrate_provider_profiles"} {
		t.Run(killPoint, func(t *testing.T) {
			path := filepath.Join(root, killPoint+".db")
			if err := os.WriteFile(path, template, 0o600); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected v4 failure")
			_, err := openWithMigrationStepHook(path, func(version uint64, point string) error {
				if version == 4 && point == killPoint {
					return injected
				}
				return nil
			})
			if !errors.Is(err, injected) {
				t.Fatalf("migration error=%v", err)
			}
			db, err := bbolt.Open(path, 0o600, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = db.View(func(tx *bbolt.Tx) error {
				if version := binary.BigEndian.Uint64(tx.Bucket(bucketMeta).Get(keySchemaVersion)); version != 3 {
					t.Fatalf("schema changed after rollback: %d", version)
				}
				var instance domain.ProviderInstance
				if err := json.Unmarshal(tx.Bucket(bucketProviders).Get([]byte("provider_v3")), &instance); err != nil {
					return err
				}
				if instance.ProfileID != "" || len(instance.CapabilityEvidence) != 0 {
					t.Fatalf("partial v4 provider survived rollback: %#v", instance)
				}
				return nil
			})
			if closeErr := db.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				t.Fatal(err)
			}

			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			credential, _ := store.GetCredential(context.Background(), "credential_v3")
			instance, _ := store.GetProvider(context.Background(), "provider_v3")
			deployment, _ := store.GetDeployment(context.Background(), "deployment_v3")
			if credential.AccessSurface != domain.SurfaceBedrockRuntime || credential.Scheme != domain.CredentialAWSSigV4Explicit ||
				instance.ProfileID != domain.ProfileBedrockConverseText || instance.CapabilityEvidence["chat"] != domain.EvidenceLegacy ||
				instance.Capabilities.DeveloperRole || deployment.ProfileID != domain.ProfileBedrockConverseText ||
				deployment.CapabilityEvidence["chat"] != domain.EvidenceLegacy || deployment.Capabilities.DeveloperRole {
				t.Fatalf("credential=%#v provider=%#v deployment=%#v", credential, instance, deployment)
			}
		})
	}
}

func createV3ProviderMetadata(t *testing.T, path string) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range requiredBuckets() {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		credential := domain.Credential{
			ID: "credential_v3", Name: "AWS", Type: domain.ProviderBedrock,
			Audience: "https://bedrock-runtime.us-east-1.amazonaws.com:443|bedrock", Ciphertext: []byte("encrypted"),
			KeyVersion: 1, CreatedAt: now, UpdatedAt: now, Revision: 1,
		}
		capabilities := domain.ProviderCapabilities{Chat: true, Streaming: true, DeveloperRole: true, StreamUsage: true}
		instance := domain.ProviderInstance{
			ID: "provider_v3", Name: "Bedrock", Type: domain.ProviderBedrock,
			BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com", CredentialID: credential.ID,
			AllowedHosts: []string{"bedrock-runtime.us-east-1.amazonaws.com"}, Capabilities: capabilities,
			Enabled: true, CreatedAt: now, UpdatedAt: now, Revision: 1,
		}
		deployment := domain.Deployment{
			ID: "deployment_v3", Name: "Claude", ProviderID: instance.ID, ProviderModel: "model",
			Capabilities: capabilities, Weight: 1, Enabled: true, CreatedAt: now, UpdatedAt: now, Revision: 1,
		}
		for _, record := range []struct {
			bucket []byte
			id     string
			value  any
		}{
			{bucketCredentials, credential.ID, credential},
			{bucketProviders, instance.ID, instance},
			{bucketDeployments, deployment.ID, deployment},
		} {
			encoded, err := json.Marshal(record.value)
			if err != nil {
				return err
			}
			if err := tx.Bucket(record.bucket).Put([]byte(record.id), encoded); err != nil {
				return err
			}
		}
		for version, name := range map[uint64]string{1: "initial_schema", 2: "migration_history", 3: "deployments"} {
			record, _ := json.Marshal(MigrationRecord{Version: version, Name: name})
			if err := tx.Bucket(bucketMigrationHistory).Put(versionKey(version), record); err != nil {
				return err
			}
		}
		var encodedVersion [8]byte
		binary.BigEndian.PutUint64(encodedVersion[:], 3)
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, encodedVersion[:])
	})
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestMetadataSnapshotIsConsistentAndReopenable(t *testing.T) {
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(root, "stage", "metadata.db")
	info, err := store.Snapshot(snapshotPath)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if info.SchemaVersion != schemaVersion || info.TxID == 0 {
		t.Fatalf("snapshot info=%#v", info)
	}
	snapshot, err := Open(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	version, err := snapshot.SchemaVersion()
	if err != nil || version != schemaVersion {
		t.Fatalf("snapshot schema=%d err=%v", version, err)
	}
}

func TestV2RouteMigrationMaterializesDeploymentAndPreservesRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	legacy := domain.Route{
		ID: "route_legacy", PublicModel: "chat", ProviderID: "provider_legacy",
		ProviderModel: "gpt-legacy", InputMicrosPerMillion: 10, OutputMicrosPerMillion: 20,
		Priority: 3, Enabled: true, CreatedAt: now, UpdatedAt: now, Revision: 4,
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range requiredBuckets() {
			if bytes.Equal(name, bucketDeployments) {
				continue
			}
			if _, createErr := tx.CreateBucketIfNotExists(name); createErr != nil {
				return createErr
			}
		}
		encodedRoute, encodeErr := json.Marshal(legacy)
		if encodeErr != nil {
			return encodeErr
		}
		if putErr := tx.Bucket(bucketRoutes).Put([]byte(legacy.ID), encodedRoute); putErr != nil {
			return putErr
		}
		for version, name := range map[uint64]string{1: "initial_schema", 2: "migration_history"} {
			record, encodeErr := json.Marshal(MigrationRecord{Version: version, Name: name})
			if encodeErr != nil {
				return encodeErr
			}
			if putErr := tx.Bucket(bucketMigrationHistory).Put(versionKey(version), record); putErr != nil {
				return putErr
			}
		}
		var encodedVersion [8]byte
		binary.BigEndian.PutUint64(encodedVersion[:], 2)
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, encodedVersion[:])
	})
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	route, err := store.GetRoute(context.Background(), legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if route.DeploymentID != "dep_migrated_"+legacy.ID || route.Revision != legacy.Revision {
		t.Fatalf("migrated route=%#v", route)
	}
	deployment, err := store.GetDeployment(context.Background(), route.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if deployment.ProviderID != legacy.ProviderID || deployment.ProviderModel != legacy.ProviderModel ||
		deployment.InputMicrosPerMillion != legacy.InputMicrosPerMillion || deployment.Revision != 1 {
		t.Fatalf("migrated deployment=%#v", deployment)
	}
}

func TestInterruptedMetadataMigrationRollsBackToV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	createV1Metadata(t, path)
	injected := errors.New("injected migration failure")
	if _, err := openWithMigrationHook(path, func(version uint64) error {
		if version == 2 {
			return injected
		}
		return nil
	}); !errors.Is(err, injected) {
		t.Fatalf("unexpected migration error: %v", err)
	}
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keySchemaVersion)
		if binary.BigEndian.Uint64(raw) != 1 {
			t.Fatalf("schema changed after rollback: %x", raw)
		}
		if tx.Bucket(bucketMigrationHistory) != nil {
			t.Fatal("migration bucket survived rolled-back transaction")
		}
		return nil
	})
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("retry migration: %v", err)
	}
	store.Close()
}

func TestDeploymentMigrationSurvivesEveryInjectedKillPoint(t *testing.T) {
	const routeCount = 8
	root := t.TempDir()
	templatePath := filepath.Join(root, "metadata-v2.db")
	createV2MetadataWithRoutes(t, templatePath, routeCount)
	template, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatal(err)
	}

	var points []string
	probePath := filepath.Join(root, "probe.db")
	if err := os.WriteFile(probePath, template, 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := openWithMigrationStepHook(probePath, func(version uint64, point string) error {
		if version == 3 {
			points = append(points, point)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if len(points) < routeCount*4+7 {
		t.Fatalf("only %d migration fault points were exercised: %v", len(points), points)
	}

	for killPoint := range points {
		killPoint := killPoint
		t.Run(fmt.Sprintf("%03d_%s", killPoint, points[killPoint]), func(t *testing.T) {
			path := filepath.Join(root, fmt.Sprintf("kill-%03d.db", killPoint))
			if err := os.WriteFile(path, template, 0o600); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected process death")
			seen := 0
			_, err := openWithMigrationStepHook(path, func(version uint64, _ string) error {
				if version != 3 {
					return nil
				}
				current := seen
				seen++
				if current == killPoint {
					return injected
				}
				return nil
			})
			if !errors.Is(err, injected) {
				t.Fatalf("kill point %d returned %v", killPoint, err)
			}
			assertV2MetadataUnchanged(t, path, routeCount)

			retried, err := Open(path)
			if err != nil {
				t.Fatalf("retry after kill point %d: %v", killPoint, err)
			}
			defer retried.Close()
			version, err := retried.SchemaVersion()
			if err != nil || version != schemaVersion {
				t.Fatalf("schema=%d err=%v", version, err)
			}
			for index := 0; index < routeCount; index++ {
				routeID := fmt.Sprintf("route_%02d", index)
				route, err := retried.GetRoute(context.Background(), routeID)
				if err != nil || route.DeploymentID != "dep_migrated_"+routeID {
					t.Fatalf("route %s after retry=%#v err=%v", routeID, route, err)
				}
				if _, err := retried.GetDeployment(context.Background(), route.DeploymentID); err != nil {
					t.Fatalf("deployment %s after retry: %v", route.DeploymentID, err)
				}
			}
		})
	}
}

func createV2MetadataWithRoutes(t *testing.T, path string, count int) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range requiredBuckets() {
			if bytes.Equal(name, bucketDeployments) {
				continue
			}
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		var encodedVersion [8]byte
		binary.BigEndian.PutUint64(encodedVersion[:], 2)
		if err := tx.Bucket(bucketMeta).Put(keySchemaVersion, encodedVersion[:]); err != nil {
			return err
		}
		for version, name := range map[uint64]string{1: "initial_schema", 2: "migration_history"} {
			record, err := json.Marshal(MigrationRecord{Version: version, Name: name})
			if err != nil {
				return err
			}
			if err := tx.Bucket(bucketMigrationHistory).Put(versionKey(version), record); err != nil {
				return err
			}
		}
		now := time.Unix(1_700_000_000, 0).UTC()
		for index := 0; index < count; index++ {
			route := domain.Route{
				ID: fmt.Sprintf("route_%02d", index), PublicModel: fmt.Sprintf("chat-%02d", index),
				ProviderID: "provider_legacy", ProviderModel: fmt.Sprintf("model-%02d", index),
				InputMicrosPerMillion: 10, OutputMicrosPerMillion: 20,
				Priority: index, Enabled: true, CreatedAt: now, UpdatedAt: now, Revision: 1,
			}
			encoded, err := json.Marshal(route)
			if err != nil {
				return err
			}
			if err := tx.Bucket(bucketRoutes).Put([]byte(route.ID), encoded); err != nil {
				return err
			}
		}
		return nil
	})
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}

func assertV2MetadataUnchanged(t *testing.T, path string, routeCount int) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(bucketDeployments) != nil {
			return errors.New("deployments bucket survived rolled-back migration")
		}
		rawVersion := tx.Bucket(bucketMeta).Get(keySchemaVersion)
		if len(rawVersion) != 8 || binary.BigEndian.Uint64(rawVersion) != 2 {
			return fmt.Errorf("schema changed after rollback: %x", rawVersion)
		}
		routes := tx.Bucket(bucketRoutes)
		if routes.Stats().KeyN != routeCount {
			return fmt.Errorf("route count changed: %d", routes.Stats().KeyN)
		}
		return routes.ForEach(func(_, raw []byte) error {
			var route domain.Route
			if err := json.Unmarshal(raw, &route); err != nil {
				return err
			}
			if route.DeploymentID != "" || route.ProviderID != "provider_legacy" || route.ProviderModel == "" {
				return fmt.Errorf("legacy route was partially mutated: %#v", route)
			}
			return nil
		})
	})
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestMetadataNewerSchemaIsRejectedWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	createV1Metadata(t, path)
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], schemaVersion+1)
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, encoded[:])
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("newer metadata schema was accepted")
	}
	db, err = bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(bucketMigrationHistory) != nil {
			t.Fatal("rejected newer schema was mutated")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func createV1Metadata(t *testing.T, path string) {
	t.Helper()
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range requiredBuckets() {
			if bytes.Equal(name, bucketMigrationHistory) || bytes.Equal(name, bucketDeployments) {
				continue
			}
			if _, err := tx.CreateBucket(name); err != nil {
				return err
			}
		}
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], 1)
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, encoded[:])
	})
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestUsageCheckpointPersistenceAndMonotonicity(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := ledger.Watermark{Generation: 1, Offset: 100, Sequence: 2}
	if err := store.PutUsageCheckpoint(first, []byte(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	got, payload, err := store.UsageCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if got != first || string(payload) != `{"version":1}` {
		t.Fatalf("watermark=%#v payload=%s", got, payload)
	}
	older := ledger.Watermark{Generation: 1, Offset: 50, Sequence: 1}
	if err := store.PutUsageCheckpoint(older, []byte(`{"version":1}`)); err == nil {
		t.Fatal("expected a backwards checkpoint to be rejected")
	}
}

func TestAuditCheckpointPersistenceAndMonotonicity(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.PutAuditCheckpoint(AuditCheckpoint{}); err != nil {
		t.Fatal(err)
	}
	var hash [32]byte
	hash[0] = 1
	checkpoint := AuditCheckpoint{Records: 1, Bytes: 100, LastHash: hash}
	if err := store.PutAuditCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	got, err := store.AuditCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if got != checkpoint {
		t.Fatalf("checkpoint=%#v", got)
	}
	if err := store.PutAuditCheckpoint(AuditCheckpoint{}); err == nil {
		t.Fatal("expected backwards checkpoint rejection")
	}
	conflict := checkpoint
	conflict.Bytes++
	if err := store.PutAuditCheckpoint(conflict); err == nil {
		t.Fatal("expected same-sequence conflict")
	}
}

func TestCredentialPersistenceAndRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	credential := domain.Credential{
		ID:         "cred_1",
		Name:       "openai",
		Type:       domain.ProviderOpenAI,
		Audience:   "https://api.openai.com:443",
		Ciphertext: []byte("ciphertext"),
		KeyVersion: 1,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}
	credential, err = store.PutCredential(ctx, credential, 0)
	if err != nil {
		t.Fatal(err)
	}
	if credential.Revision != 1 {
		t.Fatalf("unexpected revision: %d", credential.Revision)
	}
	credential.Name = "renamed"
	if _, err := store.PutCredential(ctx, credential, 99); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("expected revision conflict, got %v", err)
	}
	credential, err = store.PutCredential(ctx, credential, 1)
	if err != nil {
		t.Fatal(err)
	}
	if credential.Revision != 2 {
		t.Fatalf("unexpected updated revision: %d", credential.Revision)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.GetCredential(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || !bytes.Equal(got.Ciphertext, credential.Ciphertext) {
		t.Fatalf("unexpected credential after reopen: %#v", got)
	}
}

func TestGatewayKeyHashIndex(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	project := domain.Project{ID: "prj_1", Name: "project", Enabled: true}
	if _, err := store.PutProject(ctx, project, 0); err != nil {
		t.Fatal(err)
	}
	var hash [32]byte
	copy(hash[:], []byte("unique-hash"))
	key := domain.GatewayKey{
		ID:          "key_1",
		ProjectID:   project.ID,
		Name:        "production",
		HashVersion: 1,
		KeyHash:     hash,
		Enabled:     true,
		CreatedAt:   time.Now().UTC(),
	}
	if _, err := store.PutGatewayKey(ctx, key, 0); err != nil {
		t.Fatal(err)
	}
	got, err := store.FindGatewayKeyByHash(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != key.ID {
		t.Fatalf("unexpected key: %#v", got)
	}
}

func TestProviderAndRouteReferencesAndUniqueness(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	credential := domain.Credential{
		ID:         "cred_1",
		Name:       "openai",
		Type:       domain.ProviderOpenAI,
		Audience:   "https://api.openai.com:443",
		Ciphertext: []byte("ciphertext"),
		KeyVersion: 1,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}
	if _, err := store.PutCredential(ctx, credential, 0); err != nil {
		t.Fatal(err)
	}
	instance := domain.ProviderInstance{
		ID:           "provider_1",
		Name:         "OpenAI",
		Type:         domain.ProviderOpenAI,
		BaseURL:      "https://api.openai.com",
		CredentialID: credential.ID,
		AllowedHosts: []string{"api.openai.com"},
		Enabled:      true,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	profile, _ := domain.DefaultProviderProfile(instance.Type)
	instance.AccessSurface = profile.AccessSurface
	instance.ProfileID = profile.ProfileID
	instance.CredentialScheme = profile.CredentialScheme
	instance.Capabilities = domain.DefaultProviderCapabilities(instance.Type)
	instance.CapabilityEvidence = domain.EvidenceForCapabilities(instance.Capabilities, domain.EvidenceDeclared)
	if _, err := store.PutProvider(ctx, instance, 0); err != nil {
		t.Fatal(err)
	}
	route := domain.Route{
		ID:                    "route_1",
		PublicModel:           "chat",
		ProviderID:            instance.ID,
		ProviderModel:         "gpt-test",
		InputMicrosPerMillion: 100,
		Enabled:               true,
		CreatedAt:             time.Now().UTC(),
		UpdatedAt:             time.Now().UTC(),
	}
	if _, err := store.PutRoute(ctx, route, 0); err != nil {
		t.Fatal(err)
	}
	fallback := route
	fallback.ID = "route_2"
	fallback.Priority = 1
	if _, err := store.PutRoute(ctx, fallback, 0); err != nil {
		t.Fatalf("store fallback route: %v", err)
	}
	routes, err := store.ListRoutes(ctx)
	if err != nil || len(routes) != 2 || routes[0].ProviderID != instance.ID {
		t.Fatalf("unexpected routes=%#v err=%v", routes, err)
	}
}

func TestStoreRejectsProfileAwareDefaultGrantsAndDeploymentEscalation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	profile, _ := domain.DefaultProviderProfile(domain.ProviderOpenAI)
	credential, err := store.PutCredential(ctx, domain.Credential{
		ID: "cred_profile", Name: "OpenAI", Type: domain.ProviderOpenAI,
		AccessSurface: profile.AccessSurface, Scheme: profile.CredentialScheme,
		Audience: "audience", Ciphertext: []byte("ciphertext"), KeyVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.PutProvider(ctx, domain.ProviderInstance{
		ID: "provider_empty", Name: "Empty", Type: domain.ProviderOpenAI,
		AccessSurface: profile.AccessSurface, ProfileID: profile.ProfileID, CredentialScheme: profile.CredentialScheme,
		BaseURL: "https://api.openai.com", CredentialID: credential.ID, AllowedHosts: []string{"api.openai.com"},
		CapabilityEvidence: domain.EvidenceForCapabilities(domain.ProviderCapabilities{}, domain.EvidenceDeclared),
		CreatedAt:          now, UpdatedAt: now,
	}, 0)
	if err == nil {
		t.Fatal("profile-aware provider received implicit default capabilities")
	}
	providerCapabilities := domain.ProviderCapabilities{Chat: true, Streaming: true}
	instance, err := store.PutProvider(ctx, domain.ProviderInstance{
		ID: "provider_profile", Name: "OpenAI", Type: domain.ProviderOpenAI,
		AccessSurface: profile.AccessSurface, ProfileID: profile.ProfileID, CredentialScheme: profile.CredentialScheme,
		BaseURL: "https://api.openai.com", CredentialID: credential.ID, AllowedHosts: []string{"api.openai.com"},
		Capabilities:       providerCapabilities,
		CapabilityEvidence: domain.EvidenceForCapabilities(providerCapabilities, domain.EvidenceDeclared),
		CreatedAt:          now, UpdatedAt: now,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	badCapabilities := domain.ProviderCapabilities{Chat: true, Tools: true}
	_, err = store.PutDeployment(ctx, domain.Deployment{
		ID: "deployment_capability", Name: "Escalated", ProviderID: instance.ID, ProviderModel: "model",
		AccessSurface: instance.AccessSurface, ProfileID: instance.ProfileID, Capabilities: badCapabilities,
		CapabilityEvidence: domain.EvidenceForCapabilities(badCapabilities, domain.EvidenceDeclared),
		Weight:             1, CreatedAt: now, UpdatedAt: now,
	}, 0)
	if err == nil {
		t.Fatal("deployment exceeded provider capabilities")
	}
	verified := domain.EvidenceForCapabilities(providerCapabilities, domain.EvidenceVerified)
	_, err = store.PutDeployment(ctx, domain.Deployment{
		ID: "deployment_evidence", Name: "Escalated evidence", ProviderID: instance.ID, ProviderModel: "model",
		AccessSurface: instance.AccessSurface, ProfileID: instance.ProfileID, Capabilities: providerCapabilities,
		CapabilityEvidence: verified, Weight: 1, CreatedAt: now, UpdatedAt: now,
	}, 0)
	if err == nil {
		t.Fatal("deployment exceeded provider capability evidence")
	}
	validDeployment, err := store.PutDeployment(ctx, domain.Deployment{
		ID: "deployment_valid", Name: "Valid", ProviderID: instance.ID, ProviderModel: "model",
		AccessSurface: instance.AccessSurface, ProfileID: instance.ProfileID, Capabilities: providerCapabilities,
		CapabilityEvidence: domain.EvidenceForCapabilities(providerCapabilities, domain.EvidenceDeclared),
		Weight:             1, CreatedAt: now, UpdatedAt: now,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.PutProject(ctx, domain.Project{ID: "project_resource", Name: "Resource owner", Enabled: true, AllowedRoutes: []string{"model"}, RPM: 10, TPM: 1000, MaxConcurrency: 1, CreatedAt: now, UpdatedAt: now}, 0)
	if err != nil {
		t.Fatal(err)
	}
	keyHash := sha256.Sum256([]byte("create-once"))
	resource := domain.ProviderResource{ID: "file_external", Kind: domain.ResourceFile, ProjectID: project.ID, ProviderID: instance.ID, DeploymentID: validDeployment.ID, PublicModel: "model", ProfileID: instance.ProfileID, IdempotencyKeyHash: keyHash, CreationStatus: "completed", Status: "uploaded", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	resource, err = store.PutProviderResource(ctx, resource, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutProviderResource(ctx, domain.ProviderResource{ID: "file_duplicate", Kind: domain.ResourceFile, ProjectID: project.ID, ProviderID: instance.ID, DeploymentID: validDeployment.ID, PublicModel: "model", ProfileID: instance.ProfileID, IdempotencyKeyHash: keyHash, CreationStatus: "reserved", Status: "pending", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}, 0); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate idempotency reservation err=%v", err)
	}
	if _, err := store.ProviderResource(ctx, "another_project", resource.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project read err=%v", err)
	}
	resource.ExpiresAt = now.Add(-time.Second)
	resource.UpdatedAt = now
	if _, err := store.PutProviderResource(ctx, resource, resource.Revision); err != nil {
		t.Fatal(err)
	}
	active := domain.ProviderResource{ID: "batch_active", Kind: domain.ResourceBatch, ProjectID: project.ID, ProviderID: instance.ID, DeploymentID: validDeployment.ID, PublicModel: "model", ProfileID: instance.ProfileID, CreationStatus: "completed", Status: "in_progress", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now, ExpiresAt: now.Add(-time.Second)}
	active, err = store.PutProviderResource(ctx, active, 0)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := store.ExpiredProviderResources(ctx, now)
	if err != nil || len(expired) != 1 || expired[0].ID != resource.ID {
		t.Fatalf("expired=%#v err=%v", expired, err)
	}
	if _, err := store.ProviderResource(ctx, project.ID, active.ID); err != nil {
		t.Fatalf("active expired resource lost its owner mapping: %v", err)
	}
	if _, err := store.ProviderResource(ctx, project.ID, resource.ID); err != nil {
		t.Fatalf("expired resource was removed before object cleanup: %v", err)
	}
	_ = validDeployment
	reduced := instance
	reduced.Capabilities.Streaming = false
	reduced.CapabilityEvidence = domain.EvidenceForCapabilities(reduced.Capabilities, domain.EvidenceDeclared)
	if _, err := store.PutProvider(ctx, reduced, instance.Revision); err == nil {
		t.Fatal("provider update invalidated an existing deployment")
	}
}
