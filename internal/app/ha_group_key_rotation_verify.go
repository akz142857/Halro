package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"os"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/vault"
)

var ErrHAGroupKeyRotationPending = errors.New("HA group DEK rotation is pending; resume the same complete offline group operation")

type haGroupKeyVerificationKey struct{}
type haGroupKeyVerification struct {
	rotation *haGroupFileRotation
	member   int
}

const haGroupRotationDoneBytes = 136

func haGroupRotationDoneBytesFor(i *haGroupRotationIntent, raw []byte, node string) []byte {
	r := make([]byte, 0, haGroupRotationDoneBytes)
	r = append(r, []byte("HGRDONE1")...)
	d := sha256.Sum256(raw)
	r = append(r, d[:]...)
	d = sha256.Sum256([]byte(node))
	r = append(r, d[:]...)
	d = sha256.Sum256(i.NewKey)
	r = append(r, d[:]...)
	h := hmac.New(sha256.New, i.ClusterKey[:])
	h.Write([]byte("halro:ha:group:completed:v1"))
	h.Write(r)
	return h.Sum(r)
}

// Completion only clears this key-operation gate. Actual Member/Ordering,
// source-carrier, role/cut, seven roots and Runtime readiness are still obtained
// by the ordinary independent opener immediately afterward.
func checkHAGroupKeyRotationPending(ctx context.Context, cfg config.Config, key []byte, manager *resourcebudget.Manager) error {
	if cfg.Replication == nil {
		return nil
	}
	for slot := range haGroupRotationIntentSlots {
		path := haGroupRotationIntentPath(cfg.Storage.DataDir, slot)
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		work, err := haGroupRotationIntentWorkspace(ctx, manager)
		if err != nil {
			return err
		}
		err = func() error {
			raw, info, err := readHAGroupRotationBytes(path)
			defer clear(raw)
			if err != nil {
				return err
			}
			if p, n := groupResumeProof(ctx, cfg); p != nil && n >= 0 && p.checkRaw(cfg, slot, key, raw, info) {
				return nil
			}
			if proof, ok := ctx.Value(haGroupKeyVerificationKey{}).(*haGroupKeyVerification); ok && proof != nil && proof.rotation != nil {
				r := proof.rotation
				n := proof.member
				if n >= 0 && n < int(r.intent.Count) && slot == r.slot && r.intent.Members[n].DataDir == cfg.Storage.DataDir && r.intent.Members[n].Source.Member.NodeID == cfg.Replication.NodeID && bytes.Equal(key, r.intent.NewKey) && sha256.Sum256(raw) == r.intentDigest && r.switched[n] {
					if err := r.checkIntent(ctx, n); err != nil {
						return err
					}
					return nil
				}
			}
			keys, err := historicalHAGroupKeys(ctx, cfg, key)
			if err != nil {
				return err
			}
			defer clearHAGroupHistoricalKeys(&keys)
			intent, historical, err := decodeHAGroupWithHistory(raw, key, keys)
			if err != nil {
				return errors.Join(ErrHAGroupKeyRotationPending, err)
			}
			defer intent.clearKeys()
			return validateHAGroupCompletion(cfg, key, raw, intent, historical, keys, slot)
		}()
		if err != nil {
			err = errors.Join(ErrHAGroupKeyRotationPending, err)
		}
		if errors.Is(err, ownedfile.ErrRetirement) {
			return &haGroupRotationGateFailure{Cause: err, Work: work}
		}
		if err = errors.Join(err, work.Release()); err != nil {
			return err
		}
	}
	return nil
}

type haGroupRotationGateFailure struct {
	Cause error
	Work  resourcebudget.Permit
}

func (e *haGroupRotationGateFailure) Error() string { return e.Cause.Error() }
func (e *haGroupRotationGateFailure) Unwrap() error { return e.Cause }

func verifyHAGroupNewMaterial(ctx context.Context, f *haSelectedRuntimeFoundation, i *haGroupRotationIntent, n int) error {
	if f == nil || f.metadataStore == nil || f.replication.clusterKey != i.ClusterKey {
		return authenticatedstate.ErrSealIneligible
	}
	ring, err := f.metadataStore.VaultKeyring()
	if err != nil || ring.ActiveFingerprint != i.NewFingerprint || ring.ActiveKeyVersion != i.Members[n].VaultVersion || ring.RotationOperationID != i.OperationID || len(ring.RecoveryEnvelope) != 0 {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	v, err := vault.New(i.NewKey)
	if err != nil {
		return err
	}
	defer v.Close()
	check, err := f.metadataStore.VaultKeyCheck()
	if err != nil {
		return err
	}
	plain, err := v.DecryptCredential(vaultKeyCheckID, vaultKeyCheckProvider, vaultKeyCheckAudience, check)
	defer clear(plain)
	if err != nil || !bytes.Equal(plain, []byte(vaultKeyCheckPlaintext)) {
		return errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	options, err := selectedVaultRewrite(ctx, v, v, ring.ActiveKeyVersion, ring.ActiveFingerprint, f.bootstrap.AuditKey, f.bootstrap.LedgerKey, f.bootstrap.LegacyMetadataKey)
	if err != nil {
		return err
	}
	options.Keyring = ring
	options.Transform = func(c domain.Credential) (domain.Credential, error) {
		p, err := v.DecryptCredential(c.ID, string(c.Type), c.Audience, c.Ciphertext)
		clear(p)
		return c, err
	}
	options.TransformAdminMFA = func(a domain.AdminMFAAuthenticator) (domain.AdminMFAAuthenticator, error) {
		if a.Status == domain.AdminMFAStatusRevoked {
			return a, nil
		}
		p, err := v.DecryptAdminMFA(a.ID, a.Username, a.SecretCiphertext)
		clear(p)
		return a, err
	}
	return f.metadataStore.VerifyJournaledVaultMaterial(options)
}
