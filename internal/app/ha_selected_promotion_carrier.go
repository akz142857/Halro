package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func haSelectedPromotionPath(cfg config.Config, fulfilled bool) string {
	name := "selected-promotion.prepare"
	if fulfilled {
		name = "selected-promotion.carrier"
	}
	return filepath.Join(cfg.ClusterDirectoryPath(), name)
}

type haSelectedPromotionCarrierOwner struct {
	file      *os.File
	creator   *ownedfile.Owner
	ram, disk resourcebudget.Permit
	raw       []byte
	cause     error
}

func (o *haSelectedPromotionCarrierOwner) Close() error {
	if o == nil {
		return nil
	}
	if o.cause != nil {
		return o.cause
	}
	if o.file != nil {
		if err := ownedfile.CloseFile(o.file); err != nil {
			o.cause = errors.Join(ownedfile.ErrRetirement, err)
			return o.cause
		}
		o.file = nil
	}
	clear(o.raw)
	o.raw = nil
	if o.ram != (resourcebudget.Permit{}) {
		if err := o.ram.Release(); err != nil {
			o.cause = errors.Join(ownedfile.ErrRetirement, err)
			return o.cause
		}
		o.ram = resourcebudget.Permit{}
	}
	// Authenticated or opaque persistent bytes still occupy their original Disk.
	return nil
}

type haSelectedPromotionCarrierFailure struct {
	Owner *haSelectedPromotionCarrierOwner
	Cause error
}

func (e *haSelectedPromotionCarrierFailure) Error() string { return e.Cause.Error() }
func (e *haSelectedPromotionCarrierFailure) Unwrap() error { return e.Cause }

func readHASelectedPromotionCarrier(ctx context.Context, path string, key []byte, manager *resourcebudget.Manager) (authority *replication.HAPromotionAuthority, resultErr error) {
	if manager == nil || len(key) != 32 {
		return nil, replication.ErrMemberAuthority
	}
	ram, err := manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4 * replication.MaxHAPromotionCarrierBytes})
	if err != nil {
		return nil, err
	}
	o := &haSelectedPromotionCarrierOwner{ram: ram}
	defer func() {
		if err := o.Close(); err != nil {
			authority = nil
			resultErr = &haSelectedPromotionCarrierFailure{Owner: o, Cause: errors.Join(resultErr, err)}
		}
	}()
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 33 || info.Size() > replication.MaxHAPromotionCarrierBytes {
		return nil, ownedfile.ErrReplaced
	}
	o.file, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	actual, err := o.file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	o.raw, err = io.ReadAll(io.LimitReader(o.file, replication.MaxHAPromotionCarrierBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := o.file.Stat()
	named, nameErr := os.Lstat(path)
	if err != nil || nameErr != nil || !os.SameFile(info, after) || !os.SameFile(info, named) || after.Size() != info.Size() || named.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) || !named.ModTime().Equal(info.ModTime()) {
		return nil, errors.Join(ownedfile.ErrReplaced, err, nameErr)
	}
	if err := ownedfile.CloseFile(o.file); err != nil {
		o.cause = errors.Join(ownedfile.ErrRetirement, err)
		return nil, o.cause
	}
	o.file = nil
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return replication.QualifyHAPromotionCarrier(o.raw, key)
}

// persistHASelectedPromotionCarrier uses two fixed exclusive names. A restart
// may reuse only exactly the authenticated intended carrier, never a partial
// creator guess. Unknown IO keeps the actual owner, bytes and fees reachable.
func persistHASelectedPromotionCarrier(ctx context.Context, cfg config.Config, claims replication.HAPromotionClaims, key []byte, manager *resourcebudget.Manager) (authority *replication.HAPromotionAuthority, resultErr error) {
	raw, err := replication.SignHAPromotionCarrier(claims, key)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	path := haSelectedPromotionPath(cfg, claims.Fulfilled)
	old, err := readHASelectedPromotionCarrier(ctx, path, key, manager)
	if err == nil {
		intended, err := replication.QualifyHAPromotionCarrier(raw, key)
		if err != nil || old.Digest() != intended.Digest() {
			return nil, errors.Join(replication.ErrMemberAuthority, err)
		}
		return old, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	o := &haSelectedPromotionCarrierOwner{}
	o.ram, err = manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{RAM: 4 * replication.MaxHAPromotionCarrierBytes})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := o.Close(); err != nil {
			authority = nil
			resultErr = &haSelectedPromotionCarrierFailure{Owner: o, Cause: errors.Join(resultErr, err)}
		}
	}()
	o.disk, err = manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: replication.MaxHAPromotionCarrierBytes})
	if err != nil {
		return nil, err
	}
	o.file, o.creator, err = ownedfile.Create(path, os.O_WRONLY, 0600)
	if err != nil {
		resultErr = errors.Join(err, o.disk.Release())
		o.disk = resourcebudget.Permit{}
		return nil, resultErr
	}
	// Once an exclusive name exists, even a known partial write remains opaque;
	// no transition can be acknowledged from it and no fee is refunded.
	if err = ctx.Err(); err == nil {
		var n int
		n, err = o.file.Write(raw)
		if err == nil && n != len(raw) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = o.file.Sync()
	}
	if err == nil {
		err = o.creator.VerifyFile(o.file)
	}
	if err != nil {
		return nil, &haSelectedPromotionCarrierFailure{Owner: o, Cause: err}
	}
	if err = ownedfile.CloseFile(o.file); err != nil {
		o.cause = errors.Join(ownedfile.ErrRetirement, err)
		return nil, o.cause
	}
	o.file = nil
	if err = durable.SyncDirectory(filepath.Dir(path)); err != nil {
		o.cause = errors.Join(ownedfile.ErrRetirement, err)
		return nil, o.cause
	}
	authority, err = replication.QualifyHAPromotionCarrier(raw, key)
	if err != nil {
		return nil, err
	}
	actual, err := readHASelectedPromotionCarrier(ctx, path, key, manager)
	if err != nil || actual.Digest() != authority.Digest() {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	return authority, nil
}

func sameHAPromotionRequest(c replication.HAPromotionClaims, o PromoteMemberOptions) bool {
	p := c.Proposal
	return p.ExpectedTerm == o.ExpectedTerm && p.ExpectedAppliedIndex == o.ExpectedAppliedIndex && p.OldPrimaryNodeID == o.OldPrimaryNodeID && p.FencedBy == o.FencedBy && p.Self == o.Self && p.PlannedStepdown == o.PlannedStepdown && p.ActorID == o.Username && c.NoPeerPromise == o.NoPeerPromise
}
