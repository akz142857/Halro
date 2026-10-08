package usage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/akz142857/Halro/internal/ledger"
)

// A receipt authenticates a policy decision, not a saved projection or cursor.
// The caller resolves its native prefix from complete authenticated history under
// exclusive ownership and the independently selected role scope on every use.
type archiveDatePolicyReceipt struct {
	Prefix ledger.ProjectionPrefix
	MAC    string
}

func datePolicyMessage(s archiveDateActivationState) ([]byte, error) {
	if s.Version != 2 || s.Receipt == nil {
		return nil, ErrArchiveDateProof
	}
	old, next, err := s.positions()
	if err != nil || next.Retention.PruneThroughSequence > s.Receipt.Prefix.Head.Sequence || old.LastSequence > s.Receipt.Prefix.Head.Sequence {
		return nil, errors.Join(err, ErrArchiveDateProof)
	}
	// Exact root bytes bind the date, original baseline, immutable page chains,
	// schemas and all counters. Phase alone is a derived durability hint.
	if s.PreviousCompletion != "" {
		digest, err := hex.DecodeString(s.PreviousCompletion)
		if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != s.PreviousCompletion {
			return nil, ErrArchiveDateProof
		}
		return json.Marshal(struct {
			Domain             string
			Old, New           [32]byte
			Prefix             ledger.ProjectionPrefix
			PreviousCompletion string
		}{"halro/usage/date-policy/receipt/v2", sha256.Sum256(s.OldRoot), sha256.Sum256(s.NewRoot), s.Receipt.Prefix, s.PreviousCompletion})
	}
	return json.Marshal(struct {
		Domain   string
		Old, New [32]byte
		Prefix   ledger.ProjectionPrefix
	}{"halro/usage/date-policy/receipt/v1", sha256.Sum256(s.OldRoot), sha256.Sum256(s.NewRoot), s.Receipt.Prefix})
}

func (e *Exporter) sealDatePolicy(ctx context.Context, s *archiveDateActivationState, head ledger.Watermark) error {
	if !e.datePolicyReady || e.datePolicyPrefix == nil || s.Receipt != nil {
		return ErrArchiveDateProof
	}
	prefix, err := e.datePolicyPrefix(ctx, head)
	if err != nil || prefix.Head != head {
		return errors.Join(err, ErrArchiveDateProof)
	}
	s.Version = 2
	s.Receipt = &archiveDatePolicyReceipt{Prefix: prefix}
	raw, err := datePolicyMessage(*s)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, e.datePolicyKey[:])
	_, _ = mac.Write(raw)
	s.Receipt.MAC = hex.EncodeToString(mac.Sum(nil))
	return ctx.Err()
}

func (e *Exporter) verifyDatePolicy(ctx context.Context, s archiveDateActivationState, allowed ledger.Watermark) error {
	if !e.datePolicyReady || e.datePolicyPrefix == nil || s.Receipt == nil || s.Receipt.Prefix.Head.After(allowed) {
		return ErrArchiveDateProof
	}
	raw, err := datePolicyMessage(s)
	if err != nil {
		return err
	}
	signature, err := hex.DecodeString(s.Receipt.MAC)
	if err != nil || len(signature) != sha256.Size || hex.EncodeToString(signature) != s.Receipt.MAC {
		return ErrArchiveDateProof
	}
	mac := hmac.New(sha256.New, e.datePolicyKey[:])
	_, _ = mac.Write(raw)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return ErrArchiveDateProof
	}
	prefix, err := e.datePolicyPrefix(ctx, s.Receipt.Prefix.Head)
	if err != nil || prefix != s.Receipt.Prefix {
		return errors.Join(err, ErrArchiveDateProof)
	}
	return ctx.Err()
}
