package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/vault"
)

const haGroupHistoryLimit = 8 << 10
const haGroupHistoryDomain = "halro:ha:group:key-history:v1"

// These fixed entries supply a key for a previously completed capsule only.
// Neither its observed seals nor these hashes grant role, source or readiness.
type haGroupHistoricalKey struct {
	Key       [32]byte
	Raw, Done [32]byte
}
type haGroupHistoryCarrier struct {
	Version         uint8
	Node, Directory [32]byte
	Fingerprint     string
	Entries         [haGroupRotationIntentSlots]haGroupHistoricalKey
}

func haGroupHistoryPath(cfg config.Config) string {
	return filepath.Join(cfg.Storage.DataDir, "ha-group-key-history")
}
func decodeHAGroupHistory(raw, key []byte, cfg config.Config) (entries [haGroupRotationIntentSlots]haGroupHistoricalKey, err error) {
	if len(raw) == 0 || len(raw) > haGroupHistoryLimit {
		return entries, authenticatedstate.ErrLimit
	}
	v, err := vault.New(key)
	if err != nil {
		return entries, err
	}
	defer v.Close()
	plain, err := v.DecryptCredential("system:ha-group-key-history", "system", haGroupHistoryDomain, raw)
	if err != nil {
		return entries, err
	}
	defer clear(plain)
	var c haGroupHistoryCarrier
	d := json.NewDecoder(bytes.NewReader(plain))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return entries, err
	}
	if d.Decode(new(any)) != io.EOF || cfg.Replication == nil || c.Version != 1 || c.Node != sha256.Sum256([]byte(cfg.Replication.NodeID)) || c.Directory != sha256.Sum256([]byte(cfg.Storage.DataDir)) || c.Fingerprint != keyFingerprint(key) {
		return entries, authenticatedstate.ErrSealIneligible
	}
	for _, e := range c.Entries {
		if e != (haGroupHistoricalKey{}) && (e.Key == ([32]byte{}) || e.Raw == ([32]byte{}) || e.Done == ([32]byte{})) {
			return entries, authenticatedstate.ErrCorrupt
		}
	}
	return c.Entries, nil
}
func encodeHAGroupHistory(cfg config.Config, key []byte, entries [haGroupRotationIntentSlots]haGroupHistoricalKey) ([]byte, error) {
	c := haGroupHistoryCarrier{Version: 1, Node: sha256.Sum256([]byte(cfg.Replication.NodeID)), Directory: sha256.Sum256([]byte(cfg.Storage.DataDir)), Fingerprint: keyFingerprint(key), Entries: entries}
	plain, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	v, err := vault.New(key)
	if err != nil {
		return nil, err
	}
	defer v.Close()
	raw, err := v.EncryptCredential("system:ha-group-key-history", "system", haGroupHistoryDomain, plain)
	if len(raw) > haGroupHistoryLimit {
		return nil, authenticatedstate.ErrLimit
	}
	return raw, err
}
func clearHAGroupHistoricalKeys(entries *[haGroupRotationIntentSlots]haGroupHistoricalKey) {
	for n := range entries {
		clear(entries[n].Key[:])
	}
}

func validateHAGroupIntentHistory(i *haGroupRotationIntent) error {
	for n := 0; n < int(i.Count); n++ {
		m := i.Members[n]
		if len(m.SourceHistory) > haGroupHistoryLimit {
			return authenticatedstate.ErrLimit
		}
		for slot, e := range m.History {
			if e != (haGroupHistoricalKey{}) && (e.Key == ([32]byte{}) || e.Raw == ([32]byte{}) || e.Done == ([32]byte{}) || bytes.Equal(e.Key[:], i.NewKey)) {
				return authenticatedstate.ErrSealIneligible
			}
			first := i.Members[0].History[slot]
			if e.Key != first.Key || e.Raw != first.Raw || (e == (haGroupHistoricalKey{})) != (first == (haGroupHistoricalKey{})) {
				return authenticatedstate.ErrSealIneligible
			}
		}
		if len(m.SourceHistory) == 0 {
			continue
		}
		cfg := config.Config{Storage: config.Storage{DataDir: m.DataDir}, Replication: &config.Replication{NodeID: m.Source.Member.NodeID}}
		old, err := decodeHAGroupHistory(m.SourceHistory, i.OldKey, cfg)
		if err != nil {
			return err
		}
		for slot, e := range old {
			if e != (haGroupHistoricalKey{}) && e != m.History[slot] {
				clearHAGroupHistoricalKeys(&old)
				return authenticatedstate.ErrSealIneligible
			}
		}
		clearHAGroupHistoricalKeys(&old)
	}
	return nil
}

func historicalHAGroupKeys(ctx context.Context, cfg config.Config, key []byte) ([haGroupRotationIntentSlots]haGroupHistoricalKey, error) {
	if p, n := groupResumeProof(ctx, cfg); p != nil {
		return p.intent.Members[n].History, nil
	}
	if proof, _ := ctx.Value(haGroupKeyVerificationKey{}).(*haGroupKeyVerification); proof != nil && proof.rotation != nil {
		r := proof.rotation
		n := proof.member
		if n >= 0 && n < int(r.intent.Count) && r.switched[n] && r.intent.Members[n].DataDir == cfg.Storage.DataDir && r.intent.Members[n].Source.Member.NodeID == cfg.Replication.NodeID && bytes.Equal(key, r.intent.NewKey) {
			if err := r.checkIntent(ctx, n); err != nil {
				return [haGroupRotationIntentSlots]haGroupHistoricalKey{}, err
			}
			return r.intent.Members[n].History, nil
		}
	}
	raw, _, err := readHAGroupRotationBytes(haGroupHistoryPath(cfg))
	defer clear(raw)
	if errors.Is(err, os.ErrNotExist) {
		return [haGroupRotationIntentSlots]haGroupHistoricalKey{}, nil
	}
	if err != nil {
		return [haGroupRotationIntentSlots]haGroupHistoricalKey{}, err
	}
	return decodeHAGroupHistory(raw, key, cfg)
}
func decodeHAGroupWithHistory(raw, key []byte, entries [haGroupRotationIntentSlots]haGroupHistoricalKey) (*haGroupRotationIntent, bool, error) {
	i, err := decodeHAGroupRotationIntent(raw, key)
	if err == nil {
		return i, false, nil
	}
	digest := sha256.Sum256(raw)
	for _, e := range entries {
		if e.Raw != digest || e.Key == ([32]byte{}) {
			continue
		}
		i, err := decodeHAGroupRotationIntent(raw, e.Key[:])
		if err != nil {
			return nil, false, err
		}
		if !bytes.Equal(i.NewKey, e.Key[:]) {
			i.clearKeys()
			return nil, false, authenticatedstate.ErrSealIneligible
		}
		return i, true, nil
	}
	return nil, false, err
}
func validateHAGroupCompletion(cfg config.Config, key, raw []byte, i *haGroupRotationIntent, historical bool, entries [haGroupRotationIntentSlots]haGroupHistoricalKey, slot int) error {
	done, _, err := readHAGroupRotationBytes(haGroupRotationIntentPath(cfg.Storage.DataDir, slot) + ".complete")
	defer clear(done)
	if err != nil {
		return err
	}
	if !hmac.Equal(done, haGroupRotationDoneBytesFor(i, raw, cfg.Replication.NodeID)) {
		return ErrHAGroupKeyRotationPending
	}
	if historical {
		e := entries[slot]
		if e.Raw != sha256.Sum256(raw) || e.Done != sha256.Sum256(done) || !bytes.Equal(e.Key[:], i.NewKey) {
			return authenticatedstate.ErrSealIneligible
		}
	} else if !bytes.Equal(key, i.NewKey) {
		return ErrHAGroupKeyRotationPending
	}
	for n := 0; n < int(i.Count); n++ {
		e := i.Members[n]
		if e.DataDir == cfg.Storage.DataDir && e.KeyFile == haGroupKeyDestination(cfg) && e.Source.Member.NodeID == cfg.Replication.NodeID {
			return nil
		}
	}
	return authenticatedstate.ErrSealIneligible
}

// All completed histories are fully MAC checked, including each original done
// marker, before the next operation's intent or target KMS wrap. They remain
// untouched and paid in the original Manager. Only the fixed key carrier changes.
func (r *haGroupFileRotation) captureCompletedHistory(ctx context.Context) error {
	for n := 0; n < int(r.intent.Count); n++ {
		cfg := r.members[n].cfg
		e := &r.intent.Members[n]
		keys, err := historicalHAGroupKeys(ctx, cfg, r.intent.OldKey)
		if err != nil {
			return err
		}
		defer clearHAGroupHistoricalKeys(&keys)
		raw, _, err := readHAGroupRotationBytes(haGroupHistoryPath(cfg))
		if err == nil {
			e.SourceHistory = raw
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for slot := range haGroupRotationIntentSlots {
			if slot == r.slot {
				continue
			}
			raw, _, err := readHAGroupRotationBytes(haGroupRotationIntentPath(cfg.Storage.DataDir, slot))
			if errors.Is(err, os.ErrNotExist) {
				if keys[slot] != (haGroupHistoricalKey{}) {
					return authenticatedstate.ErrSealIneligible
				}
				continue
			}
			if err != nil {
				return err
			}
			previous, historical, err := decodeHAGroupWithHistory(raw, r.intent.OldKey, keys)
			if err == nil {
				err = validateHAGroupCompletion(cfg, r.intent.OldKey, raw, previous, historical, keys, slot)
			}
			if err == nil && (previous.Count != r.intent.Count || previous.ClusterKey != r.intent.ClusterKey) {
				err = authenticatedstate.ErrSealIneligible
			}
			if err != nil {
				clear(raw)
				if previous != nil {
					previous.clearKeys()
				}
				return err
			}
			for member := 0; member < int(previous.Count); member++ {
				a, b := previous.Members[member], r.intent.Members[member]
				if a.DataDir != b.DataDir || a.KeyFile != b.KeyFile || a.Source.Member.NodeID != b.Source.Member.NodeID {
					clear(raw)
					previous.clearKeys()
					return authenticatedstate.ErrSealIneligible
				}
			}
			done, _, err := readHAGroupRotationBytes(haGroupRotationIntentPath(cfg.Storage.DataDir, slot) + ".complete")
			if err != nil {
				clear(raw)
				previous.clearKeys()
				return err
			}
			var entry haGroupHistoricalKey
			copy(entry.Key[:], previous.NewKey)
			entry.Raw = sha256.Sum256(raw)
			entry.Done = sha256.Sum256(done)
			clear(raw)
			clear(done)
			previous.clearKeys()
			e.History[slot] = entry
			fee, err := r.members[n].owners.Components.Budget.TryAcquireRecovery(resourcebudget.Cost{Disk: 2 * haGroupRotationIntentLimit})
			if err != nil {
				return err
			}
			r.historyFees[n][slot] = fee
		}
		clearHAGroupHistoricalKeys(&keys)
	}
	return nil
}
