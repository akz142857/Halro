package usage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ledger"
)

var ErrArchiveDateProof = errors.New("date catalog activation requires a matching fresh comparison")

const archiveDateActivationName = ".archive-date-activation-v1.json"
const archiveDateActivationBytes = 128 << 10
const archiveDatePublishedVerifyName = ".archive-date-published-verify-v1.json"

// An obligation is an untrusted recovery hint, never source/deletion authority.
// It preserves exactly one observed old root and one intended new root. Future
// reclamation must independently establish current-reference and native scope.
type archiveDateActivationState struct {
	Version            int
	Phase              string
	OldRoot, NewRoot   []byte
	Receipt            *archiveDatePolicyReceipt `json:",omitempty"`
	PreviousCompletion string                    `json:",omitempty"`
}

// Replay binds the roots independently of the derived barrier phase. A retry
// may finish intent -> published without making its own saved comparison stale.
// The single-use process proof additionally binds the exact observed raw bytes.
func dateActivationBinding(s archiveDateActivationState) ([32]byte, error) {
	s.Phase = "intent"
	raw, err := json.Marshal(s)
	return sha256.Sum256(raw), err
}

func (s archiveDateActivationState) positions() (old, next ArchivePosition, err error) {
	if (s.Version != 1 && s.Version != 2) || s.Version == 1 && s.Receipt != nil || s.Version == 2 && s.Receipt == nil || s.Phase != "intent" && s.Phase != "published" || len(s.OldRoot) == 0 || len(s.OldRoot) > archiveManifestValueLimit || len(s.NewRoot) == 0 || len(s.NewRoot) > archiveDateBuildBytes {
		return old, next, ErrDiskUsageCorrupt
	}
	old, err = readArchiveManifest(context.Background(), bytes.NewReader(s.OldRoot), nil)
	if err != nil {
		return old, next, err
	}
	next, err = readArchiveManifest(context.Background(), bytes.NewReader(s.NewRoot), nil)
	if err != nil {
		return old, next, err
	}
	if old.CatalogVersion != 1 || next.SchemaVersion != archiveDateCatalogSchema || next.CatalogVersion != 1 || next.CompletePrefix || old.LastSequence != next.LastSequence || next.Retention.PruneThroughSequence != old.LastSequence || next.Files > old.Files || old.Retention != (ArchiveDateRetention{}) && next.Retention.RetainFromDate < old.Retention.RetainFromDate {
		return old, next, ErrDiskUsageCorrupt
	}
	canonical, expected, err := archiveCatalogRootBytes(next.LastSequence, false, next.CatalogHead, next.Retention)
	if err != nil || expected != next || !bytes.Equal(canonical, s.NewRoot) {
		return old, next, errors.Join(err, ErrDiskUsageCorrupt)
	}
	return old, next, nil
}

type archiveDateProof struct {
	owner                                      [16]byte
	head                                       ledger.Watermark
	window                                     DiskWindowState
	position, candidate                        ArchivePosition
	digest, buildSHA, verifySHA, activationSHA [32]byte
	identity, directory                        os.FileInfo
	published                                  bool
}

// Published reports logical coverage and a durable old-root obligation only.
// ReclamationPending is deliberately true; no physical/native deletion occurs.
type DateCatalogActivation struct {
	Previous, Current  ArchivePosition
	ReclamationPending bool
	AlreadyPublished   bool
}

func readDateActivationState(ctx context.Context, path string) (state archiveDateActivationState, raw []byte, f *os.File, info os.FileInfo, err error) {
	named, err := os.Lstat(path)
	if err != nil {
		return state, nil, nil, nil, err
	}
	if !named.Mode().IsRegular() || named.Size() > archiveDateActivationBytes {
		return state, nil, nil, nil, ErrDiskUsageCorrupt
	}
	f, err = os.Open(path)
	if err != nil {
		return state, nil, nil, nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, f.Close())
			f = nil
		}
	}()
	info, err = f.Stat()
	if err != nil || !os.SameFile(named, info) {
		return state, nil, f, nil, errors.Join(err, ErrDiskUsageCorrupt)
	}
	raw, err = io.ReadAll(io.LimitReader(f, archiveDateActivationBytes+1))
	if err != nil || len(raw) > archiveDateActivationBytes || json.Unmarshal(raw, &state) != nil {
		return state, raw, f, info, errors.Join(err, ErrDiskUsageCorrupt)
	}
	canonical, err := json.Marshal(state)
	if err != nil || !bytes.Equal(canonical, raw) {
		return state, raw, f, info, errors.Join(err, ErrDiskUsageCorrupt)
	}
	if _, _, err := state.positions(); err != nil {
		return state, raw, f, info, err
	}
	return state, raw, f, info, errors.Join(checkDateBuildState(f, info, path), ctx.Err())
}

func (e *Exporter) saveDateActivationState(ctx context.Context, path string, state archiveDateActivationState, old *os.File, oldInfo os.FileInfo, missing bool) (err error) {
	directory, err := os.Lstat(e.root)
	if err != nil || !directory.IsDir() {
		return errors.Join(err, ErrArchiveDateProof)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return e.saveDatePolicyRecord(ctx, path, raw, archiveDateActivationBytes, old, oldInfo, missing)
}

func (e *Exporter) saveDatePolicyRecord(ctx context.Context, path string, raw []byte, max int64, old *os.File, oldInfo os.FileInfo, missing bool) (err error) {
	directory, err := os.Lstat(e.root)
	if err != nil || !directory.IsDir() {
		return errors.Join(err, ErrArchiveDateProof)
	}
	if int64(len(raw)) > max {
		return ErrDiskUsageLimit
	}
	work, err := e.newArchiveWork(ctx, max)
	if err != nil {
		return err
	}
	releaseShared, err := e.reserveArchiveDisk(ctx, max)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, releaseShared()) }()
	f, err := os.CreateTemp(e.root, ".archive-date-obligation-*.tmp")
	if err != nil {
		return err
	}
	closed := false
	identity, err := f.Stat()
	defer func() { err = errors.Join(err, cleanupArchiveTemporary(f, closed, identity)) }()
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	writer := &archiveWriter{ctx: ctx, out: f, remaining: max, work: work}
	if _, err := writer.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	identity, err = f.Stat()
	if err != nil {
		return err
	}
	err = f.Close()
	closed = true
	if err != nil {
		return err
	}
	if err := checkArchiveTemporary(f, identity); err != nil {
		return err
	}
	if missing {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(err, ErrDiskUsageCorrupt)
		}
	} else if err := checkDateBuildState(old, oldInfo, path); err != nil {
		return err
	}
	if err := e.archiveReadError(ctx); err != nil {
		return err
	}
	namedDirectory, err := os.Lstat(e.root)
	if err != nil || !os.SameFile(directory, namedDirectory) {
		return errors.Join(err, ErrArchiveDateProof)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return durable.SyncDirectory(e.root)
}

// The caller independently authenticates the selected native/role/Metadata cut
// before this entry point. It consumes only this Exporter's successful in-process
// full candidate/current-root comparison. Saved Ready/phase/root scalar fields
// cannot supply a proof. No window/native trim authority is granted.
func (e *Exporter) ActivateRetainedDateCatalog(ctx context.Context, p *DiskProjection, retainFrom time.Time) (DateCatalogActivation, error) {
	return e.activateRetainedDateCatalog(ctx, p, retainFrom, nil)
}

func (e *Exporter) activateRetainedDateCatalog(ctx context.Context, p *DiskProjection, retainFrom time.Time, afterRename func() error) (out DateCatalogActivation, err error) {
	if p == nil || retainFrom.IsZero() || retainFrom.UTC().Year() < 1 || retainFrom.UTC().Year() > 9999 {
		return out, ErrArchiveDateProof
	}
	err = e.withArchiveReclamation(ctx, func(advance func() error) (jobErr error) {
		releaseShared, reserveErr := e.reserveArchiveDisk(ctx, archiveManifestValueLimit)
		if reserveErr != nil {
			return reserveErr
		}
		defer func() { jobErr = errors.Join(jobErr, releaseShared()) }()
		proof := e.dateProof
		defer func() { e.dateProof = nil }() // single use, including any accepted failure
		if proof == nil || proof.owner != p.generation || proof.candidate.Retention.RetainFromDate != retainFrom.UTC().Format("2006-01-02") {
			return ErrArchiveDateProof
		}
		_, jobErr = p.archiveJob(ctx, DefaultDateCatalogVerifyLimits().Source, func(ctx context.Context) (_ DiskArchiveCut, physicalErr error) {
			defer func() {
				physicalErr = errors.Join(physicalErr, e.archiveReadError(ctx))
				if p.retired.Load() {
					physicalErr = errors.Join(physicalErr, ErrDiskUsageClosed)
				}
			}()
			directory, err := os.Lstat(e.root)
			if err != nil || !directory.IsDir() || !os.SameFile(directory, proof.directory) {
				return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateProof)
			}
			defer func() {
				now, err := os.Lstat(e.root)
				if err != nil || !os.SameFile(directory, now) {
					physicalErr = errors.Join(physicalErr, err, ErrArchiveDateProof)
				}
			}()
			stats, err := p.ReadStats(ctx)
			if err != nil || stats.Watermark != proof.head || stats.Window != proof.window {
				return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateProof)
			}
			root, err := openArchiveManifest(e.root)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			defer func() { physicalErr = errors.Join(physicalErr, root.file.Close()) }()
			if root.info.Size() > archiveManifestValueLimit || !os.SameFile(root.info, proof.identity) || root.info.Size() != proof.identity.Size() || root.info.ModTime() != proof.identity.ModTime() {
				return DiskArchiveCut{}, ErrArchiveDateProof
			}
			position, digest, err := root.walk(ctx, nil)
			if err != nil || position != proof.position || digest != proof.digest {
				return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateProof)
			}
			path := filepath.Join(e.root, archiveDateActivationName)
			var state archiveDateActivationState
			var raw []byte
			var stateFile *os.File
			var stateInfo os.FileInfo
			missing := false
			if proof.published {
				input, err := e.readCurrentDatePolicy(ctx, position, proof.head)
				if err != nil {
					return DiskArchiveCut{}, err
				}
				state, raw, stateFile, stateInfo, path = input.state, input.raw, input.file, input.info, input.path
			} else {
				state, raw, stateFile, stateInfo, err = readDateActivationState(ctx, path)
				missing = errors.Is(err, os.ErrNotExist)
				if err != nil && !missing {
					return DiskArchiveCut{}, err
				}
			}
			if stateFile != nil {
				defer func() { physicalErr = errors.Join(physicalErr, stateFile.Close()) }()
				if err := e.verifyDatePolicy(ctx, state, proof.head); err != nil {
					return DiskArchiveCut{}, err
				}
			}
			if proof.published {
				if missing || sha256.Sum256(raw) != proof.activationSHA {
					return DiskArchiveCut{}, ErrArchiveDateProof
				}
				old, candidate, err := state.positions()
				if err != nil || candidate != proof.candidate || position.Retention != candidate.Retention || position.LastSequence < candidate.LastSequence {
					return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateProof)
				}
				verifyPath := filepath.Join(e.root, archiveDatePublishedVerifyName)
				verify, verifyRaw, verifyFile, verifyInfo, err := readDateVerifyState(ctx, verifyPath)
				if err != nil {
					return DiskArchiveCut{}, err
				}
				defer func() { physicalErr = errors.Join(physicalErr, verifyFile.Close()) }()
				binding, err := dateActivationBinding(state)
				if err != nil {
					return DiskArchiveCut{}, err
				}
				if sha256.Sum256(verifyRaw) != proof.verifySHA || verify.Phase != "ready" || !verify.Published || !verify.AncestorSeen || verify.ActivationSHA != hex.EncodeToString(binding[:]) || verify.BuildSHA != hex.EncodeToString(proof.buildSHA[:]) || verify.SourceHead != proof.head {
					return DiskArchiveCut{}, ErrArchiveDateProof
				}
				if err := errors.Join(root.check(), checkDateBuildState(stateFile, stateInfo, path), checkDateBuildState(verifyFile, verifyInfo, verifyPath), e.archiveReadError(ctx)); err != nil {
					return DiskArchiveCut{}, err
				}
				// Full current-root comparison also proved immutable-page ancestry.
				// Repeat actual barriers without replacing an appended root.
				if err := root.file.Sync(); err != nil {
					return DiskArchiveCut{}, err
				}
				if err := durable.SyncDirectory(e.root); err != nil {
					return DiskArchiveCut{}, err
				}
				out = DateCatalogActivation{Previous: old, Current: position, ReclamationPending: true, AlreadyPublished: true}
				if path == filepath.Join(e.root, archiveDateActivePolicyName) {
					// The active record's completion MAC must remain intact. A
					// future intent in the other slot is not published by this retry.
					if err := stateFile.Sync(); err != nil {
						return DiskArchiveCut{}, err
					}
					if err := durable.SyncDirectory(e.root); err != nil {
						return DiskArchiveCut{}, err
					}
					finalStats, err := p.ReadStats(ctx)
					if err != nil || finalStats.Watermark != proof.head || finalStats.Window != proof.window {
						return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateProof)
					}
					return DiskArchiveCut{}, errors.Join(root.check(), checkDateBuildState(stateFile, stateInfo, path), e.archiveReadError(ctx))
				}
			} else {
				buildPath := filepath.Join(e.root, archiveDateBuildName)
				build, buildRaw, buildFile, buildInfo, err := readDateBuildState(ctx, buildPath)
				if err != nil {
					return DiskArchiveCut{}, err
				}
				defer func() { physicalErr = errors.Join(physicalErr, buildFile.Close()) }()
				verifyPath := filepath.Join(e.root, archiveDateVerifyName)
				verify, verifyRaw, verifyFile, verifyInfo, err := readDateVerifyState(ctx, verifyPath)
				if err != nil {
					return DiskArchiveCut{}, err
				}
				defer func() { physicalErr = errors.Join(physicalErr, verifyFile.Close()) }()
				if sha256.Sum256(buildRaw) != proof.buildSHA || sha256.Sum256(verifyRaw) != proof.verifySHA || verify.Phase != "ready" || !verify.valid(build, proof.buildSHA, proof.head) || build.Baseline != position || build.BaselineSHA != hex.EncodeToString(digest[:]) {
					return DiskArchiveCut{}, ErrArchiveDateProof
				}
				newRaw, expected, err := archiveCatalogRootBytes(position.LastSequence, false, build.Output, proof.candidate.Retention)
				if err != nil || expected != proof.candidate {
					return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateProof)
				}
				oldRaw := make([]byte, int(root.info.Size()))
				if _, err := root.file.ReadAt(oldRaw, 0); err != nil {
					return DiskArchiveCut{}, err
				}
				if sha256.Sum256(oldRaw) != digest {
					return DiskArchiveCut{}, ErrArchiveDateProof
				}
				var previous *archiveDateCompletionInput
				replace := !missing && (!bytes.Equal(state.OldRoot, oldRaw) || !bytes.Equal(state.NewRoot, newRaw))
				if missing || replace {
					if position.Retention != (ArchiveDateRetention{}) {
						previous, err = e.readDateCompletion(ctx, position, proof.head)
						if err != nil {
							return DiskArchiveCut{}, err
						}
						defer func() { physicalErr = errors.Join(physicalErr, previous.file.Close()) }()
						// A different future intent cannot be silently abandoned. Only
						// the marker for this completed current policy may roll over.
						if replace {
							markerBinding, bindErr := dateActivationBinding(state)
							priorBinding, priorErr := dateActivationBinding(previous.state.Policy)
							if bindErr != nil || priorErr != nil || markerBinding != priorBinding {
								return DiskArchiveCut{}, ErrDiskUsageBusy
							}
						}
						if err := previous.file.Sync(); err != nil {
							return DiskArchiveCut{}, err
						}
					} else if replace {
						return DiskArchiveCut{}, ErrDiskUsageBusy
					}
					state = archiveDateActivationState{Version: 1, Phase: "intent", OldRoot: oldRaw, NewRoot: newRaw}
					if previous != nil {
						state.PreviousCompletion = previous.digest()
					}
					if err := e.sealDatePolicy(ctx, &state, proof.head); err != nil {
						return DiskArchiveCut{}, err
					}
				}
				if previous != nil {
					if err := previous.check(ctx); err != nil {
						return DiskArchiveCut{}, err
					}
				}
				if previous == nil && state.PreviousCompletion != "" {
					previous, err = e.readDateCompletion(ctx, position, proof.head)
					if err != nil {
						return DiskArchiveCut{}, err
					}
					defer func() { physicalErr = errors.Join(physicalErr, previous.file.Close()) }()
					if err := e.verifyDateSuccession(ctx, state, previous.state, previous.raw, proof.head); err != nil {
						return DiskArchiveCut{}, err
					}
				}

				if _, _, err := state.positions(); err != nil {
					return DiskArchiveCut{}, err
				}
				if err := e.saveDateActivationState(ctx, path, state, stateFile, stateInfo, missing); err != nil {
					return DiskArchiveCut{}, err
				}
				intent, _, intentFile, intentInfo, err := readDateActivationState(ctx, path)
				if err != nil {
					return DiskArchiveCut{}, err
				}
				defer func() { physicalErr = errors.Join(physicalErr, intentFile.Close()) }()
				intentBinding, bindErr := dateActivationBinding(intent)
				stateBinding, stateErr := dateActivationBinding(state)
				if bindErr != nil || stateErr != nil || intent.Phase != state.Phase || intentBinding != stateBinding {
					return DiskArchiveCut{}, ErrArchiveDateProof
				}
				// Intent is durable before logical coverage changes. Successful root
				// rename can never strand its old-root reclamation obligation.
				if err := root.check(); err != nil {
					return DiskArchiveCut{}, err
				}
				if err := checkDateBuildState(buildFile, buildInfo, buildPath); err != nil {
					return DiskArchiveCut{}, err
				}
				if err := checkDateBuildState(verifyFile, verifyInfo, verifyPath); err != nil {
					return DiskArchiveCut{}, err
				}
				stats, err = p.ReadStats(ctx)
				if err != nil || stats.Watermark != proof.head || stats.Window != proof.window {
					return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateProof)
				}
				if err := advance(); err != nil {
					return DiskArchiveCut{}, err
				}
				beforeRename := func() error {
					if previous != nil {
						if err := previous.check(ctx); err != nil {
							return err
						}
					}
					named, err := os.Lstat(e.root)
					if err != nil || !os.SameFile(named, directory) {
						return errors.Join(err, ErrArchiveDateProof)
					}
					if err := errors.Join(checkDateBuildState(intentFile, intentInfo, path), checkDateBuildState(buildFile, buildInfo, buildPath), checkDateBuildState(verifyFile, verifyInfo, verifyPath), e.archiveReadError(ctx)); err != nil {
						return err
					}
					stats, err := p.ReadStats(ctx)
					if err != nil || stats.Watermark != proof.head || stats.Window != proof.window {
						return errors.Join(err, ErrArchiveDateProof)
					}
					return nil
				}
				if err := e.publishArchiveCatalogRootWithRetentionHook(ctx, root, false, expected.LastSequence, false, expected.CatalogHead, expected.Retention, &archiveWork{remaining: archiveManifestValueLimit}, nil, beforeRename, afterRename); err != nil {
					return DiskArchiveCut{}, err
				}
				out = DateCatalogActivation{Previous: position, Current: expected, ReclamationPending: true}
			}
			// Capture the now-current intent descriptor; the old pre-save FD may
			// name a replaced inode. Phase is derived, never a saved permission.
			current, currentRaw, currentFile, currentInfo, err := readDateActivationState(ctx, path)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			defer func() { physicalErr = errors.Join(physicalErr, currentFile.Close()) }()
			if err := e.verifyDatePolicy(ctx, current, proof.head); err != nil {
				return DiskArchiveCut{}, err
			}
			if !bytes.Equal(current.OldRoot, state.OldRoot) || !bytes.Equal(current.NewRoot, state.NewRoot) {
				return DiskArchiveCut{}, ErrArchiveDateProof
			}
			if proof.published && sha256.Sum256(currentRaw) != proof.activationSHA {
				return DiskArchiveCut{}, ErrArchiveDateProof
			}
			current.Phase = "published"
			if err := e.saveDateActivationState(ctx, path, current, currentFile, currentInfo, false); err != nil {
				return DiskArchiveCut{}, err
			}
			finalRoot, err := openArchiveManifest(e.root)
			if err != nil {
				return DiskArchiveCut{}, err
			}
			defer func() { physicalErr = errors.Join(physicalErr, finalRoot.file.Close()) }()
			actual, _, err := finalRoot.walk(ctx, nil)
			if err != nil || actual != out.Current {
				return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateProof)
			}
			if err := finalRoot.check(); err != nil {
				return DiskArchiveCut{}, err
			}
			finalStats, err := p.ReadStats(ctx)
			if err != nil || finalStats.Watermark != proof.head || finalStats.Window != proof.window {
				return DiskArchiveCut{}, errors.Join(err, ErrArchiveDateProof)
			}
			return DiskArchiveCut{}, nil
		})
		return jobErr
	})
	if err != nil {
		out = DateCatalogActivation{}
	}
	return out, err
}
