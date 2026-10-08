package usage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/ledger"
)

const archiveDateActivePolicyName = ".archive-date-active-policy-v1.json"
const archiveDateActivePolicyBytes = archiveDateActivationBytes + archiveDateBuildBytes

// One persistent current policy and its authentic historical old-chain result.
// The record retains no prior-policy list or global orphan absence claim.
type archiveDateActivePolicyState struct {
	Version       int
	Policy        archiveDateActivationState
	OriginalChain archiveDatePagesState
	MAC           string
}

func (e *Exporter) signDateActivePolicy(s *archiveDateActivePolicyState) error {
	if s.Version != 1 || !e.datePolicyReady {
		return ErrArchiveDateProof
	}
	if _, _, err := s.Policy.positions(); err != nil {
		return err
	}
	if err := e.verifyDatePages(s.OriginalChain); err != nil {
		return err
	}
	old, _, _ := s.Policy.positions()
	binding, err := dateActivationBinding(s.Policy)
	if err != nil {
		return err
	}
	if s.OriginalChain.BindingSHA != hex.EncodeToString(binding[:]) || s.OriginalChain.Old != old.CatalogHead || s.OriginalChain.Next != (ArchivePageRef{}) || s.OriginalChain.Pending || s.OriginalChain.Processed != old.CatalogHead.Pages {
		return ErrArchiveDateProof
	}
	s.MAC = ""
	raw, err := json.Marshal(struct {
		Domain string
		State  archiveDateActivePolicyState
	}{"halro/usage/date-policy/active-completion/v1", *s})
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, e.datePolicyKey[:])
	_, _ = mac.Write(raw)
	s.MAC = hex.EncodeToString(mac.Sum(nil))
	return nil
}

func (e *Exporter) verifyDateActivePolicy(ctx context.Context, s archiveDateActivePolicyState, allowed ledger.Watermark) error {
	if err := e.verifyDatePolicy(ctx, s.Policy, allowed); err != nil {
		return err
	}
	signature, err := hex.DecodeString(s.MAC)
	if err != nil || len(signature) != sha256.Size || hex.EncodeToString(signature) != s.MAC {
		return ErrArchiveDateProof
	}
	if err := e.signDateActivePolicy(&s); err != nil {
		return err
	}
	expected, _ := hex.DecodeString(s.MAC)
	if !hmac.Equal(signature, expected) {
		return ErrArchiveDateProof
	}
	return ctx.Err()
}

func readDateActivePolicyState(ctx context.Context, path string) (s archiveDateActivePolicyState, raw []byte, f *os.File, info os.FileInfo, err error) {
	named, err := os.Lstat(path)
	if err != nil {
		return
	}
	if !named.Mode().IsRegular() || named.Size() < 1 || named.Size() > archiveDateActivePolicyBytes {
		err = ErrDiskUsageCorrupt
		return
	}
	f, err = os.Open(path)
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, f.Close())
			f = nil
		}
	}()
	info, err = f.Stat()
	if err != nil || !os.SameFile(info, named) || info.Size() != named.Size() {
		err = errors.Join(err, ErrDiskUsageCorrupt)
		return
	}
	raw, err = io.ReadAll(io.LimitReader(f, archiveDateActivePolicyBytes+1))
	if err != nil || len(raw) > archiveDateActivePolicyBytes || json.Unmarshal(raw, &s) != nil {
		err = errors.Join(err, ErrDiskUsageCorrupt)
		return
	}
	canonical, err := json.Marshal(s)
	if err != nil || !bytes.Equal(canonical, raw) {
		err = errors.Join(err, ErrDiskUsageCorrupt)
		return
	}
	err = errors.Join(checkDateBuildState(f, info, path), ctx.Err())
	return
}

type archiveDatePolicyInput struct {
	state archiveDateActivationState
	raw   []byte
	file  *os.File
	info  os.FileInfo
	path  string
}

// At most two fixed records. Every present slot authenticates independently;
// corruption is never hidden by falling back to the other slot. Selection uses
// exact retention (including original H), then fresh comparison proves ancestry.
func (e *Exporter) readCurrentDatePolicy(ctx context.Context, current ArchivePosition, allowed ledger.Watermark) (out *archiveDatePolicyInput, err error) {
	var marker, active *archiveDatePolicyInput
	defer func() {
		for _, in := range []*archiveDatePolicyInput{marker, active} {
			if in != nil && in != out {
				err = errors.Join(err, in.file.Close())
			}
		}
		if err != nil && out != nil {
			err = errors.Join(err, out.file.Close())
			out = nil
		}
	}()
	markerPath := filepath.Join(e.root, archiveDateActivationName)
	s, raw, file, info, readErr := readDateActivationState(ctx, markerPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	if readErr == nil {
		marker = &archiveDatePolicyInput{s, raw, file, info, markerPath}
		if err := e.verifyDatePolicy(ctx, s, allowed); err != nil {
			return nil, err
		}
	}
	activePath := filepath.Join(e.root, archiveDateActivePolicyName)
	stored, activeRaw, activeFile, activeInfo, readErr := readDateActivePolicyState(ctx, activePath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	if readErr == nil {
		active = &archiveDatePolicyInput{stored.Policy, activeRaw, activeFile, activeInfo, activePath}
		if err := e.verifyDateActivePolicy(ctx, stored, allowed); err != nil {
			return nil, err
		}
	}
	matches := func(in *archiveDatePolicyInput) bool {
		if in == nil {
			return false
		}
		_, next, err := in.state.positions()
		return err == nil && current.Retention != (ArchiveDateRetention{}) && next.Retention == current.Retention && next.LastSequence <= current.LastSequence
	}
	if matches(marker) {
		out = marker
	} else if matches(active) {
		out = active
	} else {
		return nil, ErrArchiveDateProof
	}
	if err := checkDateBuildState(out.file, out.info, out.path); err != nil {
		return out, err
	}
	return out, ctx.Err()
}

// OriginalChainComplete describes authentic historical processing of the named
// old chain. OrphansPending stays true; no global filesystem absence, shared fee
// refund or native trim permission is supplied here. A later signed intent may
// bind this exact completion to hand off the two fixed policy slots.
type DateObligationCompletion struct {
	Current                                                                 ArchivePosition
	OriginalChainComplete, PolicyPreserved, AlreadyRecorded, OrphansPending bool
}

func (e *Exporter) CompleteRetainedDateObligation(ctx context.Context, p *DiskProjection, retainFrom time.Time) (DateObligationCompletion, error) {
	return e.completeRetainedDateObligation(ctx, p, retainFrom, nil, nil)
}

func (e *Exporter) completeRetainedDateObligation(ctx context.Context, p *DiskProjection, retainFrom time.Time, beforeSave, afterSave func() error) (out DateObligationCompletion, err error) {
	err = e.withDateReclaimAuthority(ctx, p, retainFrom, DefaultDateFileReclaimLimits(), func(ctx context.Context, a *archiveDateReclaimAuthority) (jobErr error) {
		if err := a.readBudget.reserve(archiveDateBuildBytes + archiveDateActivePolicyBytes); err != nil {
			return err
		}
		pagesPath := filepath.Join(e.root, archiveDatePagesName)
		pages, pagesRaw, pagesFile, pagesInfo, err := readDatePagesState(ctx, pagesPath)
		if err != nil {
			return err
		}
		defer func() { jobErr = errors.Join(jobErr, pagesFile.Close()) }()
		policy := a.state
		policy.Phase = "published" // Fresh published proof makes derived-phase replay idempotent.
		state := archiveDateActivePolicyState{Version: 1, Policy: policy, OriginalChain: pages}
		if err := e.signDateActivePolicy(&state); err != nil {
			return err
		}
		path := filepath.Join(e.root, archiveDateActivePolicyName)
		previous, raw, file, info, err := readDateActivePolicyState(ctx, path)
		missing := errors.Is(err, os.ErrNotExist)
		if err != nil && !missing {
			return err
		}
		if file != nil {
			defer func() { jobErr = errors.Join(jobErr, file.Close()) }()
			if err := e.verifyDateActivePolicy(ctx, previous, a.proof.head); err != nil {
				return err
			}
		}
		canonical, err := json.Marshal(state)
		if err != nil {
			return err
		}
		replacing := !missing && !bytes.Equal(raw, canonical)
		if replacing {
			if err := e.verifyDateSuccession(ctx, state.Policy, previous, raw, a.proof.head); err != nil {
				return err
			}
		}
		check := func() error {
			if err := a.check(); err != nil {
				return err
			}
			if err := checkDateBuildState(pagesFile, pagesInfo, pagesPath); err != nil {
				return err
			}
			digest := sha256.Sum256(pagesRaw)
			if err := hashDateReclaimFile(ctx, pagesFile, pagesInfo.Size(), hex.EncodeToString(digest[:]), nil, a.readBudget); err != nil {
				return err
			}
			if file != nil {
				if err := checkDateBuildState(file, info, path); err != nil {
					return err
				}
				digest := sha256.Sum256(raw)
				if err := hashDateReclaimFile(ctx, file, info.Size(), hex.EncodeToString(digest[:]), nil, a.readBudget); err != nil {
					return err
				}
			} else if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				return errors.Join(err, ErrArchiveDateProof)
			}
			return nil
		}
		if beforeSave != nil {
			if err := beforeSave(); err != nil {
				return err
			}
		}
		if err := check(); err != nil {
			return err
		}
		if err := pagesFile.Sync(); err != nil {
			return err
		}
		if missing || replacing {
			if err := a.advance(); err != nil {
				return err
			}
			if err := e.saveDatePolicyRecord(ctx, path, canonical, archiveDateActivePolicyBytes, file, info, missing); err != nil {
				return err
			}
		} else {
			if err := file.Sync(); err != nil {
				return err
			}
			if err := syncDateRootDirectory(e.root, a.directory); err != nil {
				return err
			}
		}
		if afterSave != nil {
			if err := afterSave(); err != nil {
				return err
			}
		}
		actual, actualRaw, actualFile, actualInfo, err := readDateActivePolicyState(ctx, path)
		if err != nil {
			return err
		}
		defer func() { jobErr = errors.Join(jobErr, actualFile.Close()) }()
		if !bytes.Equal(actualRaw, canonical) {
			return ErrArchiveDateProof
		}
		if err := e.verifyDateActivePolicy(ctx, actual, a.proof.head); err != nil {
			return err
		}
		if err := checkDateBuildState(actualFile, actualInfo, path); err != nil {
			return err
		}
		// Marker/transition files stay present. A successor replaces this slot only
		// after its own original chain completes and its receipt binds the prior MAC.
		out = DateObligationCompletion{Current: a.proof.position, OriginalChainComplete: true, PolicyPreserved: true, AlreadyRecorded: !missing && !replacing, OrphansPending: true}
		return errors.Join(checkDateBuildState(pagesFile, pagesInfo, pagesPath), a.check())
	})
	if err != nil {
		out = DateObligationCompletion{}
	}
	return out, err
}
