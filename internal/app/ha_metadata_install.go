package app

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/akz142857/Halro/internal/diskinventory"
	"github.com/akz142857/Halro/internal/replication"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/metadatajournal"
	"github.com/akz142857/Halro/internal/metadatastate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

// HAMetadataInstallation owns only the files it exclusively created. It grants
// no business readiness, ordering confirmation, promotion or old-source repair.
// Published files retain their persistent Disk obligation after Close.
type HAMetadataInstallation struct {
	mu                                         sync.Mutex
	Path                                       string
	stage, root                                string
	stageInfo, rootInfo                        os.FileInfo
	files                                      [5]*ownedfile.Owner
	fileInfo                                   [5]os.FileInfo
	rootDisk, journalDisk, carrierDisk, work   resourcebudget.Permit
	baseline                                   *metadatastate.Baseline
	inventory                                  *diskinventory.Baseline
	published, publicationUncertain, discarded bool
	failure                                    error
	phase                                      func(string) error
}

func (p *HAMetadataInstallation) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeLocked()
}

func (p *HAMetadataInstallation) closeLocked() error {
	if p.inventory != nil {
		p.failure = errors.Join(p.failure, p.inventory.Close())
	}
	if p.baseline != nil {
		p.failure = errors.Join(p.failure, p.baseline.Close())
	}
	if errors.Is(p.failure, ownedfile.ErrRetirement) {
		return p.failure
	}
	if p.work != (resourcebudget.Permit{}) {
		if err := p.work.Release(); err != nil {
			p.failure = errors.Join(p.failure, ownedfile.ErrRetirement, err)
			return p.failure
		}
		p.work = resourcebudget.Permit{}
	}
	return p.failure
}

func (p *HAMetadataInstallation) DiskReservations() (resourcebudget.Permit, resourcebudget.Permit, error) {
	if p == nil {
		return resourcebudget.Permit{}, resourcebudget.Permit{}, metadatastate.ErrSource
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.published || p.failure != nil || p.discarded {
		return resourcebudget.Permit{}, resourcebudget.Permit{}, errors.Join(metadatastate.ErrSource, p.failure)
	}
	r, err := p.rootDisk.Retain()
	if err != nil {
		return r, resourcebudget.Permit{}, err
	}
	j, err := p.journalDisk.Retain()
	if err != nil {
		return resourcebudget.Permit{}, j, errors.Join(err, r.Release())
	}
	return r, j, nil
}

// Discard is only for this process's unpublished, completely joined staging.
// A published namespace may already have role borrowers and cannot be retired
// by this installer. Unknown actual Close or publication retains every fee.
func (p *HAMetadataInstallation) Discard() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.discarded {
		return nil
	}
	if p.published || p.publicationUncertain {
		return metadatastate.ErrSource
	}
	if err := p.closeLocked(); errors.Is(err, ownedfile.ErrRetirement) {
		return err
	}
	for _, dir := range []struct {
		path string
		info os.FileInfo
	}{{p.stage, p.stageInfo}, {p.root, p.rootInfo}} {
		if dir.info == nil {
			continue
		}
		actual, err := os.Lstat(dir.path)
		if err != nil || !actual.IsDir() || !os.SameFile(actual, dir.info) {
			p.failure = errors.Join(p.failure, ownedfile.ErrRetirement, ownedfile.ErrReplaced, err)
			return p.failure
		}
	}
	for _, owner := range p.files {
		if owner != nil {
			if err := owner.Retire(); err != nil {
				p.failure = errors.Join(p.failure, ownedfile.ErrRetirement, err)
				return p.failure
			}
		}
	}
	if p.rootInfo != nil {
		if err := durable.SyncDirectory(p.root); err != nil {
			p.failure = errors.Join(p.failure, ownedfile.ErrRetirement, err)
			return p.failure
		}
		if err := os.Remove(p.root); err != nil {
			p.failure = errors.Join(p.failure, ownedfile.ErrRetirement, err)
			return p.failure
		}
	}
	if p.stageInfo != nil {
		if err := durable.SyncDirectory(p.stage); err != nil {
			p.failure = errors.Join(p.failure, ownedfile.ErrRetirement, err)
			return p.failure
		}
		if err := os.Remove(p.stage); err != nil {
			p.failure = errors.Join(p.failure, ownedfile.ErrRetirement, err)
			return p.failure
		}
		if err := durable.SyncDirectory(filepath.Dir(p.stage)); err != nil {
			p.failure = errors.Join(p.failure, ownedfile.ErrRetirement, err)
			return p.failure
		}
	}
	for _, fee := range []resourcebudget.Permit{p.rootDisk, p.journalDisk, p.carrierDisk} {
		if fee != (resourcebudget.Permit{}) {
			if err := fee.Release(); err != nil {
				p.failure = errors.Join(p.failure, ownedfile.ErrRetirement, err)
				return p.failure
			}
		}
	}
	p.rootDisk, p.journalDisk, p.carrierDisk = resourcebudget.Permit{}, resourcebudget.Permit{}, resourcebudget.Permit{}
	p.discarded = true
	return nil
}

func InstallHAMetadataBaseline(ctx context.Context, cfg config.Config, keys HAMetadataKeys, sourceDirectory string, guard *lock.Lock, manager *resourcebudget.Manager) (*HAMetadataInstallation, error) {
	return installHAMetadataBaseline(ctx, cfg, keys, sourceDirectory, guard, manager, nil)
}

func installHAMetadataBaseline(ctx context.Context, cfg config.Config, keys HAMetadataKeys, sourceDirectory string, guard *lock.Lock, manager *resourcebudget.Manager, phase func(string) error) (result *HAMetadataInstallation, resultErr error) {
	return installHAMetadataBaselineWithOptions(ctx, cfg, keys, sourceDirectory, guard, manager, HAMetadataInstallOptions{}, phase)
}

// Restart preserves interrupted files in four opaque slots and requalifies a
// published candidate from the independently supplied source carrier. It never
// grants startup readiness or deletes cross-process files.
type HAMetadataInstallOptions struct{ Restart bool }

func InstallHAMetadataBaselineWithOptions(ctx context.Context, cfg config.Config, keys HAMetadataKeys, sourceDirectory string, guard *lock.Lock, manager *resourcebudget.Manager, options HAMetadataInstallOptions) (*HAMetadataInstallation, error) {
	return installHAMetadataBaselineWithOptions(ctx, cfg, keys, sourceDirectory, guard, manager, options, nil)
}

func installHAMetadataBaselineWithOptions(ctx context.Context, cfg config.Config, keys HAMetadataKeys, sourceDirectory string, guard *lock.Lock, manager *resourcebudget.Manager, options HAMetadataInstallOptions, phase func(string) error) (result *HAMetadataInstallation, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Replication == nil || cfg.ResourceBudget == nil || manager == nil || guard == nil || !guard.CoversDataDirectory(cfg.Storage.DataDir) || !filepath.IsAbs(sourceDirectory) {
		return nil, metadatastate.ErrSource
	}
	if err := keys.validate(); err != nil {
		return nil, err
	}
	limits := cfg.ResourceBudget
	if limits.MetadataStoreDiskBytes <= 4096 || limits.MetadataJournalDiskBytes < 4096 {
		return nil, metadatastate.ErrSource
	}
	p := &HAMetadataInstallation{Path: filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory), phase: phase}
	p.stage = p.Path + ".preparing"
	p.root = filepath.Join(p.stage, "metadata-root")
	if !options.Restart {
		for _, path := range []string{p.Path, p.stage} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				return nil, errors.Join(os.ErrExist, err)
			}
		}
	}
	member, clusterKey, err := readMemberState(cfg, keys.Master)
	if err != nil {
		return nil, err
	}
	defer clear(clusterKey[:])
	p.work, err = manager.TryAcquireRecovery(resourcebudget.Cost{RAM: 2 << 20})
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			p.failure = errors.Join(p.failure, resultErr)
			if err := p.Discard(); err != nil {
				result, resultErr = p, errors.Join(resultErr, err)
			}
		}
	}()
	carrier, authority, err := readHAMetadataCarrier(ctx, filepath.Join(sourceDirectory, "carrier.json"), clusterKey[:], member)
	if err != nil {
		return nil, err
	}
	claims := authority.Claims()
	if claims.New.MaxFileBytes != limits.MetadataJournalDiskBytes || carrier.Preparation.Head.Sequence != 0 {
		return nil, metadatastate.ErrSource
	}
	resumePublished := false
	if options.Restart {
		if limits.PersistentInventoryDiskBytes <= 0 {
			return nil, metadatastate.ErrSource
		}
		p.inventory, err = diskinventory.Reconcile(ctx, diskinventory.Options{DataDir: cfg.Storage.DataDir, Guard: guard, Budget: manager, Ceiling: limits.PersistentInventoryDiskBytes, Deadline: time.Minute})
		if err != nil {
			return p, errors.Join(err, ownedfile.ErrRetirement)
		}
		if err = p.inventory.Publish(); err != nil {
			return p, err
		}
		info, statErr := os.Lstat(p.Path)
		if statErr == nil {
			if _, _, err := migrationDirectoryIdentity(info); err != nil {
				return nil, err
			}
			if _, err := os.Lstat(p.stage); !errors.Is(err, os.ErrNotExist) {
				return nil, errors.Join(os.ErrExist, err)
			}
			resumePublished = true
			p.published = true
			p.stage, p.stageInfo = p.Path, info
			p.root = filepath.Join(p.Path, "metadata-root")
			p.rootInfo, err = os.Lstat(p.root)
			if err != nil || !p.rootInfo.IsDir() || p.rootInfo.Mode().Perm()&0077 != 0 {
				return p, errors.Join(ownedfile.ErrReplaced, err)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		} else {
			if err := preserveHAMetadataInstallStage(ctx, cfg, p.stage, guard, p.at); err != nil {
				return nil, err
			}
		}
	}
	p.rootDisk, err = manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: limits.MetadataStoreDiskBytes})
	if err != nil {
		return nil, err
	}
	p.journalDisk, err = manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: limits.MetadataJournalDiskBytes})
	if err != nil {
		return nil, err
	}
	p.carrierDisk, err = manager.TryAcquireContext(ctx, resourcebudget.Background, resourcebudget.Cost{Disk: maxHAMetadataCarrierBytes})
	if err != nil {
		return nil, err
	}
	if resumePublished {
		for i := 0; i < len(p.fileInfo); i++ {
			info, err := os.Lstat(haMetadataInstalledFile(p.Path, i))
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
				return p, errors.Join(ownedfile.ErrReplaced, err)
			}
			p.fileInfo[i] = info
		}
		if err := p.qualify(ctx, cfg, keys, guard, manager, member, clusterKey[:], carrier, claims, p.Path); err != nil {
			return p, err
		}
		if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
			return p, err
		}
		if err := p.Close(); err != nil {
			return p, err
		}
		return p, nil
	}
	if err := os.Mkdir(p.stage, 0700); err != nil {
		return nil, err
	}
	p.stageInfo, err = os.Lstat(p.stage)
	if err != nil {
		return p, errors.Join(ownedfile.ErrRetirement, err)
	}
	if err := os.Mkdir(p.root, 0700); err != nil {
		return nil, err
	}
	p.rootInfo, err = os.Lstat(p.root)
	if err != nil {
		return p, errors.Join(ownedfile.ErrRetirement, err)
	}
	if err := p.at("install-stage-created"); err != nil {
		return nil, err
	}
	buffer := make([]byte, 64<<10)
	for i, file := range []struct {
		name  string
		limit int64
	}{
		{"carrier.json", maxHAMetadataCarrierBytes},
		{"metadata.journal", limits.MetadataJournalDiskBytes},
		{"metadata-root/pages", limits.MetadataStoreDiskBytes},
		{"metadata-root/active.root", 4096},
		{"metadata-root/.halro.lock", 4096},
	} {
		if err := p.copyFile(ctx, filepath.Join(sourceDirectory, file.name), filepath.Join(p.stage, file.name), file.limit, buffer, i); err != nil {
			return nil, err
		}
		if err := p.at(fmt.Sprintf("install-file-%d-synced", i)); err != nil {
			return nil, err
		}
	}
	if err := errors.Join(durable.SyncDirectory(p.root), durable.SyncDirectory(p.stage)); err != nil {
		return nil, err
	}
	if err := p.qualify(ctx, cfg, keys, guard, manager, member, clusterKey[:], carrier, claims, p.stage); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := renameMigrationWithoutReplace(p.stage, p.Path); err != nil {
		return nil, err
	}
	p.publicationUncertain = true
	if err := p.at("install-renamed"); err != nil {
		return p, err
	}
	if err := durable.SyncDirectory(cfg.Storage.DataDir); err != nil {
		return p, err
	}
	p.published, p.publicationUncertain = true, false
	if err := p.at("install-published"); err != nil {
		return p, err
	}
	if err := p.Close(); err != nil {
		return p, err
	}
	return p, nil
}

func (p *HAMetadataInstallation) at(name string) error {
	if p.phase != nil {
		return p.phase(name)
	}
	return nil
}

func (p *HAMetadataInstallation) copyFile(ctx context.Context, source, destination string, ceiling int64, buffer []byte, slot int) (resultErr error) {
	parent, err := os.Lstat(filepath.Dir(source))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	in, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, ownedfile.CloseFile(in)) }()
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 0 || info.Size() > ceiling {
		return errors.Join(metadatastate.ErrSource, err)
	}
	check := func() error {
		after, err := in.Stat()
		named, nameErr := os.Lstat(source)
		currentParent, parentErr := os.Lstat(filepath.Dir(source))
		if err != nil || nameErr != nil || parentErr != nil || !named.Mode().IsRegular() || !os.SameFile(info, after) || !os.SameFile(info, named) || !os.SameFile(parent, currentParent) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
			return errors.Join(ownedfile.ErrReplaced, err, nameErr, parentErr)
		}
		return ctx.Err()
	}
	if err := check(); err != nil {
		return err
	}
	out, owner, err := ownedfile.Create(destination, os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	p.files[slot] = owner
	defer func() { resultErr = errors.Join(resultErr, ownedfile.CloseFile(out)) }()
	var copied int64
	for {
		if err := check(); err != nil {
			return err
		}
		n, readErr := in.Read(buffer)
		if n > 0 {
			if int64(n) > info.Size()-copied {
				return metadatastate.ErrSource
			}
			written, writeErr := out.Write(buffer[:n])
			if writeErr != nil || written != n {
				return errors.Join(io.ErrShortWrite, writeErr)
			}
			copied += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	if copied != info.Size() {
		return metadatastate.ErrSource
	}
	if err := check(); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := owner.VerifyFile(out); err != nil {
		return err
	}
	p.fileInfo[slot], err = out.Stat()
	return err
}

func haMetadataInstalledFile(directory string, index int) string {
	return filepath.Join(directory, [5]string{"carrier.json", "metadata.journal", "metadata-root/pages", "metadata-root/active.root", "metadata-root/.halro.lock"}[index])
}

func (p *HAMetadataInstallation) qualify(ctx context.Context, cfg config.Config, keys HAMetadataKeys, guard *lock.Lock, manager *resourcebudget.Manager, member replication.MemberState, clusterKey []byte, carrier haMetadataCarrier, claims replication.MetadataBaselineClaims, directory string) error {
	limits := cfg.ResourceBudget
	// Qualify the actual copied carrier again: a source replaced between its
	// first read and copy cannot transfer the first read's permission.
	copied, copiedAuthority, err := readHAMetadataCarrier(ctx, filepath.Join(directory, "carrier.json"), clusterKey, member)
	if err != nil || !reflect.DeepEqual(copied, carrier) || copiedAuthority == nil || !reflect.DeepEqual(copiedAuthority.Claims(), claims) {
		return errors.Join(metadatastate.ErrSource, err)
	}
	newKey, err := hkdf.Key(sha256.New, keys.Ledger, claims.New.Baseline.Scope[:], "halro:metadata:bound-journal:hmac:v1", 32)
	if err != nil {
		return err
	}
	defer clear(newKey)
	head, err := metadatajournal.BoundReplicationPrefixHead(ctx, filepath.Join(directory, "metadata.journal"), newKey, claims.New, claims.New.Baseline.Epoch, 0)
	if err != nil || head != carrier.Preparation.Head {
		return errors.Join(metadatastate.ErrSource, err)
	}
	generation, sequence, err := metadatajournal.BoundReplicationCursor(ctx, filepath.Join(directory, "metadata.journal"), newKey, claims.New)
	if err != nil || generation != head.Epoch || sequence != 0 {
		return errors.Join(metadatastate.ErrSource, err)
	}
	r := carrier.Preparation
	o := metadatastate.OpenOptions{BaselineOptions: metadatastate.BaselineOptions{Identity: r.Evidence.Identity, InstanceID: r.Evidence.InstanceID, KeyGeneration: r.Evidence.KeyGeneration, Budget: manager, MaxSourceBytes: limits.MetadataStoreDiskBytes, MaxDiskBytes: limits.MetadataStoreDiskBytes, MaxAppendBytes: min(limits.MetadataStoreDiskBytes/2, 8<<20)}, Evidence: r.Evidence, SavedRoot: r.Root, SelectedSealRoot: &r.Reference.Root, SavedHead: r.Head, NativeHead: head, PermittedHead: head, RootDiskReservation: p.rootDisk, JournalDiskReservation: p.journalDisk, MaxJournalBytes: limits.MetadataJournalDiskBytes}
	p.baseline, _, err = metadatastate.OpenFromRoot(ctx, keys.Ledger, p.root, filepath.Join(directory, "metadata.journal"), o)
	if err != nil {
		return err
	}
	if err := p.baseline.VerifyHAMaintenanceRoot(ctx); err != nil {
		return err
	}
	if err := p.baseline.Close(); err != nil {
		return err
	}
	if err := p.at("install-qualified-closed"); err != nil {
		return err
	}
	actualMember, actualClusterKey, err := readMemberState(cfg, keys.Master)
	clear(actualClusterKey[:])
	if err != nil || !reflect.DeepEqual(member, actualMember) || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return errors.Join(metadatastate.ErrSource, err)
	}
	for _, dir := range []struct {
		path string
		info os.FileInfo
	}{{p.stage, p.stageInfo}, {p.root, p.rootInfo}} {
		if dir.info == nil {
			continue
		}
		info, err := os.Lstat(dir.path)
		if err != nil || !info.IsDir() || !os.SameFile(info, dir.info) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
	}
	for i, owner := range p.files {
		info, err := os.Lstat(haMetadataInstalledFile(directory, i))
		previous := p.fileInfo[i]
		if (!p.published && owner == nil) || previous == nil || err != nil || !info.Mode().IsRegular() || !os.SameFile(previous, info) || info.Size() != previous.Size() || !info.ModTime().Equal(previous.ModTime()) {
			return errors.Join(ownedfile.ErrReplaced, err)
		}
	}

	return ctx.Err()
}

func preserveHAMetadataInstallStage(ctx context.Context, cfg config.Config, stage string, guard *lock.Lock, phase func(string) error) error {
	captured, err := os.Lstat(stage)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, _, err := migrationDirectoryIdentity(captured); err != nil {
		return err
	}
	destination := ""
	for i := 0; i < 4; i++ {
		path := fmt.Sprintf("%s.unqualified-%d", filepath.Join(cfg.Storage.DataDir, HAMetadataBaselineDirectory), i)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			destination = path
			break
		} else if err != nil {
			return err
		}
	}
	if destination == "" {
		return errors.New("HA Metadata interrupted installation slots exhausted; retained candidates require explicit archival")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	named, err := os.Lstat(stage)
	if err != nil || !os.SameFile(captured, named) || !guard.CoversDataDirectory(cfg.Storage.DataDir) {
		return errors.Join(ownedfile.ErrReplaced, err)
	}
	if err := renameMigrationWithoutReplace(stage, destination); err != nil {
		return err
	}
	if err := phase("install-stage-preserved"); err != nil {
		return err
	}
	return durable.SyncDirectory(cfg.Storage.DataDir)
}
