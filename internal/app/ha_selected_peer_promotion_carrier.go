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

func haSelectedPeerPromotionPath(cfg config.Config, promised bool) string {
	name := "selected-peer-promotion.prepare"
	if promised {
		name = "selected-peer-promotion.promise"
	}
	return filepath.Join(cfg.ClusterDirectoryPath(), name)
}
func readHASelectedPeerPromotionCarrier(ctx context.Context, path string, key []byte, manager *resourcebudget.Manager) (authority *replication.HAPromotionPeerAuthority, resultErr error) {
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
	return replication.QualifyHAPromotionPeerCarrier(o.raw, key)
}

func persistHASelectedPeerPromotionCarrier(ctx context.Context, cfg config.Config, claims replication.HAPromotionPeerClaims, key []byte, manager *resourcebudget.Manager) (authority *replication.HAPromotionPeerAuthority, resultErr error) {
	if manager == nil || len(key) != 32 {
		return nil, replication.ErrMemberAuthority
	}
	o := &haSelectedPromotionCarrierOwner{}
	var err error
	o.ram, err = manager.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 4 * replication.MaxHAPromotionCarrierBytes})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := o.Close(); err != nil {
			authority = nil
			resultErr = &haSelectedPromotionCarrierFailure{Owner: o, Cause: errors.Join(resultErr, err)}
		}
	}()
	o.raw, err = replication.SignHAPromotionPeerCarrier(claims, key)
	if err != nil {
		return nil, err
	}
	path := haSelectedPeerPromotionPath(cfg, claims.Promise != nil)
	old, err := readHASelectedPeerPromotionCarrier(ctx, path, key, manager)
	if err == nil {
		intended, err := replication.QualifyHAPromotionPeerCarrier(o.raw, key)
		if err != nil || intended.Digest() != old.Digest() {
			return nil, errors.Join(replication.ErrMemberAuthority, err)
		}
		return old, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	o.disk, err = manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: replication.MaxHAPromotionCarrierBytes})
	if err != nil {
		return nil, err
	}
	o.file, o.creator, err = ownedfile.Create(path, os.O_WRONLY, 0600)
	if err != nil {
		if releaseErr := o.disk.Release(); releaseErr != nil {
			o.cause = errors.Join(ownedfile.ErrRetirement, err, releaseErr)
			return nil, o.cause
		}
		o.disk = resourcebudget.Permit{}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, &haSelectedPromotionCarrierFailure{Owner: o, Cause: err}
	}
	n, err := o.file.Write(o.raw)
	if err != nil || n != len(o.raw) {
		o.cause = errors.Join(ownedfile.ErrRetirement, err, io.ErrShortWrite)
		return nil, o.cause
	}
	if err := o.file.Sync(); err != nil {
		o.cause = errors.Join(ownedfile.ErrRetirement, err)
		return nil, o.cause
	}
	if err := o.creator.VerifyFile(o.file); err != nil {
		o.cause = errors.Join(ownedfile.ErrRetirement, err)
		return nil, o.cause
	}
	if err := ownedfile.CloseFile(o.file); err != nil {
		o.cause = ownedfile.CloseError(o.file, err)
		return nil, o.cause
	}
	o.file = nil
	if err := durable.SyncDirectory(filepath.Dir(path)); err != nil {
		o.cause = errors.Join(ownedfile.ErrRetirement, err)
		return nil, o.cause
	}
	authority, err = replication.QualifyHAPromotionPeerCarrier(o.raw, key)
	if err != nil {
		return nil, err
	}
	actual, err := readHASelectedPeerPromotionCarrier(ctx, path, key, manager)
	if err != nil || actual.Digest() != authority.Digest() {
		return nil, errors.Join(ownedfile.ErrReplaced, err)
	}
	return authority, nil
}
