package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/ledger"
)

// A successor keeps the prior complete policy in the active slot until its own
// original chain completes. Only one digest links the two fixed records; no
// prior-policy list or recursively embedded completion is retained.
type archiveDateCompletionInput struct {
	state archiveDateActivePolicyState
	raw   []byte
	file  *os.File
	info  os.FileInfo
	path  string
}

func (e *Exporter) readDateCompletion(ctx context.Context, current ArchivePosition, allowed ledger.Watermark) (in *archiveDateCompletionInput, err error) {
	path := filepath.Join(e.root, archiveDateActivePolicyName)
	state, raw, file, info, err := readDateActivePolicyState(ctx, path)
	if err != nil {
		return nil, errors.Join(err, ErrArchiveDateProof)
	}
	in = &archiveDateCompletionInput{state, raw, file, info, path}
	defer func() {
		if err != nil {
			err = errors.Join(err, file.Close())
			in = nil
		}
	}()
	if err = e.verifyDateActivePolicy(ctx, state, allowed); err != nil {
		return in, err
	}
	_, candidate, err := state.Policy.positions()
	if err != nil || candidate.Retention != current.Retention || candidate.LastSequence > current.LastSequence {
		return in, errors.Join(err, ErrArchiveDateProof)
	}
	return in, in.check(ctx)
}

func (in *archiveDateCompletionInput) digest() string {
	digest := sha256.Sum256(in.raw)
	return hex.EncodeToString(digest[:])
}

func (in *archiveDateCompletionInput) check(ctx context.Context) error {
	if err := checkDateBuildState(in.file, in.info, in.path); err != nil {
		return err
	}
	raw, err := io.ReadAll(io.NewSectionReader(in.file, 0, archiveDateActivePolicyBytes+1))
	if err != nil || !bytes.Equal(raw, in.raw) {
		return errors.Join(err, ErrArchiveDateProof)
	}
	return ctx.Err()
}

func (e *Exporter) verifyDateSuccession(ctx context.Context, next archiveDateActivationState, previous archiveDateActivePolicyState, raw []byte, allowed ledger.Watermark) error {
	if err := e.verifyDateActivePolicy(ctx, previous, allowed); err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	old, _, err := next.positions()
	_, prior, priorErr := previous.Policy.positions()
	if err != nil || priorErr != nil || next.PreviousCompletion != hex.EncodeToString(digest[:]) ||
		old.Retention != prior.Retention || old.LastSequence < prior.LastSequence {
		return errors.Join(err, priorErr, ErrArchiveDateProof)
	}
	return e.verifyDatePolicy(ctx, next, allowed)
}
