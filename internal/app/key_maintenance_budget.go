package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/vault"
)

const keyMaintenanceRAMBytes int64 = 96 << 20

var errKeyMaintenanceRetirement = errors.New("key maintenance physical retirement is uncertain")

type keyMaintenanceContextKey struct{}
type keyMaintenanceCloser interface{ Close() error }

type keyMaintenanceFailure struct {
	scope *keyMaintenanceScope
	cause error
}

func (e *keyMaintenanceFailure) Error() string { return e.cause.Error() }
func (e *keyMaintenanceFailure) Unwrap() error { return e.cause }

type keyMaintenanceStage struct {
	path      string
	owner     *ownedfile.Owner
	published bool
}
type keyMaintenanceScope struct {
	root       *commandResourceScope
	permit     resourcebudget.Permit
	parent     string
	parentInfo os.FileInfo
	live       string
	target     os.FileInfo
	closers    [32]keyMaintenanceCloser
	stages     [8]keyMaintenanceStage
	lock       keyMaintenanceCloser
	debt       error
}

func keyMaintenanceContext(ctx context.Context, cfg config.Config) (context.Context, func() error, error) {
	ctx, pressure, err := commandBudgetContext(ctx, cfg)
	if err != nil {
		return ctx, nil, err
	}
	root, _ := ctx.Value(commandResourceScopeKey{}).(*commandResourceScope)
	if old, ok := ctx.Value(keyMaintenanceContextKey{}).(*keyMaintenanceScope); ok {
		if old.root != root {
			return ctx, nil, resourcebudget.ErrInvalid
		}
		return ctx, func() error { return nil }, nil
	}
	s := &keyMaintenanceScope{root: root}
	if cfg.ResourceBudget != nil {
		if pressure.logicalBudget() == nil {
			return ctx, nil, resourcebudget.ErrInvalid
		}
		s.permit, err = pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: keyMaintenanceRAMBytes})
		if err != nil {
			return ctx, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, errors.Join(err, s.release())
	}
	return context.WithValue(ctx, keyMaintenanceContextKey{}, s), s.finish, nil
}
func keyScope(ctx context.Context) *keyMaintenanceScope {
	s, _ := ctx.Value(keyMaintenanceContextKey{}).(*keyMaintenanceScope)
	return s
}

func finishKeyMaintenance(ctx context.Context, finish func() error, cause error) error {
	if errors.Is(cause, ownedfile.ErrRetirement) || errors.Is(cause, vault.ErrMasterKeyRetirement) {
		if scope := keyScope(ctx); scope != nil {
			scope.debt = errors.Join(scope.debt, cause)
		}
	}
	return errors.Join(cause, finish(), ctx.Err())
}
func (s *keyMaintenanceScope) release() error {
	if s.permit != (resourcebudget.Permit{}) {
		return s.permit.Release()
	}
	return nil
}
func (s *keyMaintenanceScope) captureSource(cfg config.Config) error {
	if s.parentInfo != nil {
		return s.checkParent()
	}
	live, err := filepath.Abs(cfg.MetadataPath())
	if err != nil || len(live) > 4096 {
		return errors.Join(err, ownedfile.ErrReplaced)
	}
	parent := filepath.Dir(live)
	p, err := os.Lstat(parent)
	if err != nil || !p.IsDir() {
		return errors.Join(err, ownedfile.ErrReplaced)
	}
	target, err := os.Lstat(live)
	if err != nil || !target.Mode().IsRegular() {
		return errors.Join(err, ownedfile.ErrReplaced)
	}
	s.parent, s.parentInfo, s.live, s.target = parent, p, live, target
	return nil
}
func (s *keyMaintenanceScope) checkParent() error {
	current, err := os.Lstat(s.parent)
	if err != nil || !os.SameFile(current, s.parentInfo) {
		return errors.Join(err, ownedfile.ErrReplaced)
	}
	return nil
}
func (s *keyMaintenanceScope) trackCloser(c keyMaintenanceCloser) error {
	for i := range s.closers {
		if s.closers[i] == nil {
			s.closers[i] = c
			return nil
		}
	}
	return errors.New("key maintenance exceeded fixed physical handle slots")
}
func (s *keyMaintenanceScope) trackStage(path string, owner *ownedfile.Owner, err error) error {
	if errors.Is(err, ownedfile.ErrRetirement) || errors.Is(err, boltstore.ErrMetadataFileRetirement) {
		s.debt = errors.Join(s.debt, err)
	}
	if owner == nil {
		return err
	}
	for i := range s.stages {
		if s.stages[i].path == "" {
			s.stages[i] = keyMaintenanceStage{path: path, owner: owner}
			return err
		}
	}
	s.debt = errors.Join(s.debt, errKeyMaintenanceRetirement, errors.New("key maintenance exceeded fixed staging receipts"))
	return errors.Join(err, s.debt)
}
func (s *keyMaintenanceScope) expected(path string) os.FileInfo {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	if absolute == s.live {
		return s.target
	}
	for _, stage := range s.stages {
		if stage.path == path && !stage.published {
			return stage.owner.Identity()
		}
	}
	return nil
}
func (s *keyMaintenanceScope) finish() (resultErr error) {
	// Actual Store/Audit Close joins all readers and terminal writes. Repeated
	// Store Close retains the original retirement result; no name is reopened.
	for _, c := range s.closers {
		if c != nil {
			if err := c.Close(); err != nil {
				s.debt = errors.Join(s.debt, errKeyMaintenanceRetirement, err)
			}
		}
	}
	resultErr = s.debt
	if resultErr == nil {
		for _, stage := range s.stages {
			if stage.owner == nil || stage.published {
				continue
			}
			if err := s.checkParent(); err != nil {
				resultErr = errors.Join(resultErr, errKeyMaintenanceRetirement, err)
				break
			}
			if err := stage.owner.Retire(); err != nil {
				resultErr = errors.Join(resultErr, errKeyMaintenanceRetirement, err)
				continue
			}
			if err := durable.SyncDirectory(s.parent); err != nil {
				resultErr = errors.Join(resultErr, errKeyMaintenanceRetirement, err)
			}
		}
	}
	if resultErr != nil {
		// Retain the actual guard with the surviving handles and fees. A failed
		// Close is not permission for another offline writer to enter the tree.
		return &keyMaintenanceFailure{scope: s, cause: resultErr}
	}
	if s.lock != nil {
		if err := s.lock.Close(); err != nil {
			resultErr = errors.Join(resultErr, errKeyMaintenanceRetirement, err)
		}
	}
	if resultErr == nil {
		resultErr = s.release()
	}
	if resultErr != nil {
		return &keyMaintenanceFailure{scope: s, cause: resultErr}
	}
	return resultErr
}
func holdKeyMaintenanceLock(ctx context.Context, c keyMaintenanceCloser) error {
	s := keyScope(ctx)
	if s == nil || s.lock != nil {
		return errors.New("key maintenance requires exactly one original offline directory lock")
	}
	s.lock = c
	return nil
}
func publishKeyMetadata(ctx context.Context, stagePath, livePath string) error {
	s := keyScope(ctx)
	if s == nil {
		return errors.New("key Metadata publication requires original maintenance scope")
	}
	live, err := filepath.Abs(livePath)
	if err != nil || live != s.live || filepath.Dir(stagePath) != s.parent {
		return errors.Join(err, ownedfile.ErrReplaced)
	}
	if s.debt != nil {
		return s.debt
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.checkParent(); err != nil {
		return err
	}
	for i := range s.stages {
		stage := &s.stages[i]
		if stage.path != stagePath || stage.owner == nil || stage.published {
			continue
		}
		// All handles have been explicitly closed by the command before this point.
		// A second Close joins/observes their original outcome; it never closes a
		// replacement. Every failure retains the whole workspace and all outputs.
		for _, c := range s.closers {
			if c != nil {
				if err := c.Close(); err != nil {
					s.debt = errors.Join(errKeyMaintenanceRetirement, err)
					return s.debt
				}
			}
		}
		if err := stage.owner.ReplaceCaptured(live, s.target); err != nil {
			return err
		}
		stage.published = true // Rename already happened, even if directory Sync fails.
		s.target = stage.owner.Identity()
		if err := durable.SyncDirectory(s.parent); err != nil {
			s.debt = errors.Join(errKeyMaintenanceRetirement, err)
			return s.debt
		}
		return nil
	}
	return errors.New("key Metadata publication has no exclusive staging creation receipt")
}
