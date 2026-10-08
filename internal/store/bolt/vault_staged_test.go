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
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"golang.org/x/sys/unix"
)

func vaultStagedFixture(t *testing.T, path string, n int) *Store {
	t.Helper()
	s, err := OpenForWholeFilePublish(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.migrate(func(tx *Tx) error {
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("row-%04d", i)
			values := []struct {
				bucket, key []byte
				value       any
			}{
				{bucketCredentials, []byte(id), domain.Credential{ID: id, Name: id, Type: "staged-test", Audience: "test", Ciphertext: []byte("old"), KeyVersion: 1, Revision: 7, CreatedAt: now, UpdatedAt: now}},
				{bucketAdminMFAAuthenticators, []byte(adminMFAKey(id, id)), domain.AdminMFAAuthenticator{ID: id, Username: id, Name: id, Type: domain.AdminMFATypeTOTP, Status: domain.AdminMFAStatusActive, SecretCiphertext: []byte("old-mfa"), CreatedAt: now, ConfirmedAt: &now, Revision: 9}},
				{bucketAdminUsers, []byte(id), domain.AdminUser{Username: id, SessionGeneration: 4, Revision: 8}},
			}
			for _, v := range values {
				raw, err := json.Marshal(v.value)
				if err != nil {
					return err
				}
				if err := tx.Bucket(v.bucket).Put(v.key, raw); err != nil {
					return err
				}
			}
			if err := tx.Bucket(bucketAdminSessions).Put([]byte(id), []byte("obsolete-session")); err != nil {
				return err
			}
			if err := tx.Bucket(bucketAdminMFAChallenges).Put([]byte(id), []byte("obsolete-challenge")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}
func vaultStagedOptions(ctx context.Context) VaultRewrite {
	return VaultRewrite{Context: ctx, VaultKeyCheck: []byte("new-check"), AuditHMACEnvelope: []byte("new-audit"), LedgerHMACEnvelope: []byte("new-ledger"), Keyring: VaultKeyring{FormatVersion: 1, ActiveKeyVersion: 2, ActiveFingerprint: "sha256:" + strings.Repeat("4", 64), PreviousFingerprint: "sha256:" + strings.Repeat("3", 64), RecoveryEnvelope: []byte("bridge")},
		Transform: func(v domain.Credential) (domain.Credential, error) {
			v.Ciphertext = []byte("new")
			v.KeyVersion = 2
			return v, nil
		},
		TransformAdminMFA: func(v domain.AdminMFAAuthenticator) (domain.AdminMFAAuthenticator, error) {
			v.SecretCiphertext = []byte("new-mfa")
			return v, nil
		}}
}
func TestVaultRewriteStagedUsesFiniteTransactionsAndPreservesFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	s := vaultStagedFixture(t, path, 130)
	defer s.Close()
	options := vaultStagedOptions(context.Background())
	transform := options.Transform
	previousID, changes, count := 0, 0, 0
	options.Transform = func(v domain.Credential) (domain.Credential, error) {
		info, err := s.Info()
		if err != nil {
			return v, err
		}
		if int(info.TxID) != previousID {
			changes++
			previousID = int(info.TxID)
		}
		count++
		return transform(v)
	}
	if err := s.RewriteVaultMaterialStaged(options); err != nil {
		t.Fatal(err)
	}
	if changes != 3 || count != 130 {
		t.Fatalf("transaction views=%d rows=%d", changes, count)
	}
	for i := 0; i < 130; i++ {
		id := fmt.Sprintf("row-%04d", i)
		v, err := s.GetCredential(context.Background(), id)
		if err != nil || v.Revision != 7 || v.KeyVersion != 2 || !bytes.Equal(v.Ciphertext, []byte("new")) {
			t.Fatalf("credential=%+v err=%v", v, err)
		}
		mfa, err := s.GetAdminMFAAuthenticator(context.Background(), id, id)
		if err != nil || mfa.Revision != 9 || !bytes.Equal(mfa.SecretCiphertext, []byte("new-mfa")) {
			t.Fatalf("MFA=%+v err=%v", mfa, err)
		}
		user, err := s.GetAdminUser(context.Background(), id)
		if err != nil || user.Revision != 9 || user.SessionGeneration != 5 {
			t.Fatalf("user=%+v err=%v", user, err)
		}
	}
	if err := s.view(func(tx *Tx) error {
		if tx.Bucket(bucketMeta).Get(vaultRewritePendingKey) != nil || binary.BigEndian.Uint64(tx.Bucket(bucketMeta).Get(keySchemaVersion)) != schemaVersion {
			return ErrVaultRewriteIncomplete
		}
		for _, b := range [][]byte{bucketAdminSessions, bucketAdminMFAChallenges} {
			k, _ := tx.Bucket(b).Cursor().First()
			if k != nil {
				return errors.New("invalidated identity survived")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestVaultRewriteStagedCancellationAndChangedSourceCannotOpen(t *testing.T) {
	for _, mode := range []string{"cancel", "changed", "transform"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stage.db")
			s := vaultStagedFixture(t, path, 130)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			options := vaultStagedOptions(ctx)
			transform := options.Transform
			count := 0
			boom := errors.New("transform failure")
			options.Transform = func(v domain.Credential) (domain.Credential, error) {
				count++
				if count == 65 {
					switch mode {
					case "cancel":
						cancel()
					case "changed":
						if err := s.migrate(func(tx *Tx) error {
							return tx.Bucket(bucketAdminSessions).Put([]byte("behind-cursor"), []byte("changed"))
						}); err != nil {
							return v, err
						}
					case "transform":
						return v, boom
					}
				}
				return transform(v)
			}
			err := s.RewriteVaultMaterialStaged(options)
			if err == nil {
				t.Fatal("partial rewrite succeeded")
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if mode == "changed" && !errors.Is(err, ErrPricingRecoveryChanged) {
				t.Fatal(err)
			}
			if mode == "transform" && !errors.Is(err, boom) {
				t.Fatal(err)
			}
			first, err := s.GetCredential(context.Background(), "row-0000")
			if err != nil || first.KeyVersion != 2 {
				t.Fatal("first committed page missing", err)
			}
			later, err := s.GetCredential(context.Background(), "row-0064")
			if err != nil || later.KeyVersion != 1 {
				t.Fatal("incomplete page committed", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			for _, open := range []func(string) (*Store, error){Open, OpenReadOnly, OpenPrimary, OpenReplica, OpenForWholeFilePublish} {
				store, err := open(path)
				if store != nil || !errors.Is(err, ErrVaultRewriteIncomplete) {
					if store != nil {
						store.Close()
					}
					t.Fatal("unpublished partial image accepted", err)
				}
			}
		})
	}
}
func TestVaultRewriteStagedPreflightAndWideRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stage.db")
	s := vaultStagedFixture(t, path, 1)
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.RewriteVaultMaterialStaged(vaultStagedOptions(ctx)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	options := vaultStagedOptions(context.Background())
	options.Keyring.RecoveryEnvelope = make([]byte, pricingRecoveryWideRowBytes)
	if err := s.RewriteVaultMaterialStaged(options); !errors.Is(err, ErrPricingRecoveryLimit) {
		t.Fatal(err)
	}
	if err := s.view(func(tx *Tx) error {
		if tx.Bucket(bucketMeta).Get(vaultRewritePendingKey) != nil {
			return ErrVaultRewriteIncomplete
		}
		return nil
	}); err != nil {
		t.Fatal("preflight changed image", err)
	}
	if err := s.migrate(func(tx *Tx) error {
		return tx.Bucket(bucketCredentials).Put([]byte("row-0000"), bytes.Repeat([]byte("x"), pricingRecoveryWideRowBytes+1))
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RewriteVaultMaterialStaged(vaultStagedOptions(context.Background())); !errors.Is(err, ErrPricingRecoveryLimit) {
		t.Fatal("wide row decoded", err)
	}
}

type vaultCustomMarshaler struct{ called *bool }

func (v vaultCustomMarshaler) MarshalJSON() ([]byte, error) {
	*v.called = true
	return []byte(`"unexpected"`), nil
}
func TestVaultRewriteGraphBoundsPrecedeCustomCodec(t *testing.T) {
	called := false
	values := []any{vaultCustomMarshaler{&called}, strings.Repeat("<", 1024), bytes.Repeat([]byte{1}, 1024), make([]string, 4097)}
	type node struct{ Next *node }
	cycle := &node{}
	cycle.Next = cycle
	values = append(values, cycle)
	for _, v := range values {
		if _, err := encodeVaultRewriteRow(v, 1024); !errors.Is(err, ErrPricingRecoveryLimit) {
			t.Fatal("unbounded codec accepted", err)
		}
	}
	if called {
		t.Fatal("custom codec ran before admission")
	}
}
func TestVaultMaintenanceWalkDetectsWritesBehindCursor(t *testing.T) {
	s := vaultStagedFixture(t, filepath.Join(t.TempDir(), "stage.db"), 130)
	defer s.Close()
	count := 0
	err := s.WalkCredentialsMaintenance(context.Background(), func(domain.Credential) error {
		count++
		if count == 1 {
			return s.migrate(func(tx *Tx) error { return tx.Bucket(bucketAdminSessions).Put([]byte("behind"), []byte("new")) })
		}
		return nil
	})
	if !errors.Is(err, ErrPricingRecoveryChanged) {
		t.Fatal("changed complete scan succeeded", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.WalkCredentialsMaintenance(ctx, func(domain.Credential) error { t.Fatal("cancelled callback"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	count = 0
	if err := s.WalkAdminMFAAuthenticatorsMaintenance(context.Background(), func(v domain.AdminMFAAuthenticator) error {
		if v.ID != v.Username {
			t.Fatal(v)
		}
		count++
		return nil
	}); err != nil || count != 130 {
		t.Fatal("MFA complete walk", count, err)
	}
}
func TestVaultRewriteStagedSIGKILLPreservesOriginalAndRejectsStage(t *testing.T) {
	if path := os.Getenv("HALRO_VAULT_STAGED_KILL_PATH"); path != "" {
		s, err := OpenForWholeFilePublish(path)
		if err != nil {
			t.Fatal(err)
		}
		options := vaultStagedOptions(context.Background())
		base := options.Transform
		count := 0
		options.Transform = func(v domain.Credential) (domain.Credential, error) {
			count++
			if count == 65 {
				if err := unix.Kill(os.Getpid(), unix.SIGKILL); err != nil {
					return v, err
				}
			}
			return base(v)
		}
		if err := s.RewriteVaultMaterialStaged(options); err != nil {
			t.Fatal(err)
		}
		t.Fatal("SIGKILL child survived")
	}
	dir := t.TempDir()
	live := filepath.Join(dir, "live.db")
	s := vaultStagedFixture(t, live, 130)
	stage := filepath.Join(dir, "stage.db")
	if _, _, err := s.SnapshotCreateOwnedContext(context.Background(), stage); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(original)
	cmd := exec.Command(os.Args[0], "-test.run=^TestVaultRewriteStagedSIGKILLPreservesOriginalAndRejectsStage$")
	cmd.Env = append(os.Environ(), "HALRO_VAULT_STAGED_KILL_PATH="+stage)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("child not killed err=%v output=%s", err, output)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("child failed without SIGKILL: %v %s", err, output)
	}
	after, err := os.ReadFile(live)
	if err != nil || sha256.Sum256(after) != hash {
		t.Fatal("original source changed", err)
	}
	if store, err := OpenReadOnly(stage); store != nil || !errors.Is(err, ErrVaultRewriteIncomplete) {
		if store != nil {
			store.Close()
		}
		t.Fatal("crashed staged image accepted", err)
	}
}
