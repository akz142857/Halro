package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	"github.com/akz142857/Halro/internal/usage"
	"github.com/akz142857/Halro/internal/vault"
)

// VerifyUsageAfter checks a caller-selected (after, archive H] interval. The
// explicit VerifyUsageAfter selects exactly this interval. VerifyUsage may select
// a signed current date policy, independently authenticating its native prefix.
// Missing files or an unauthenticated root/checkpoint never choose a floor.
func VerifyUsageAfter(ctx context.Context, cfg config.Config, after uint64) (report usage.ReconciliationReport, err error) {
	return verifyUsageAfter(ctx, cfg, after, nil)
}

// The seam injects a source/member fault after archive decoding, before final
// authority checks. It supplies no production authority or mutable ownership.
func verifyUsageAfter(ctx context.Context, cfg config.Config, after uint64, beforeRevalidate func() error) (usage.ReconciliationReport, error) {
	return verifyUsageScope(ctx, cfg, after, nil, beforeRevalidate)
}

// VerifyUsageRetainedDates authenticates a fresh role-allowed complete native
// reference before comparing the explicit UTC date scope. Neither existing
// Usage/checkpoint state nor root metadata supplies the policy or authority.
func VerifyUsageRetainedDates(ctx context.Context, cfg config.Config, retainFrom time.Time) (usage.ReconciliationReport, error) {
	if retainFrom.IsZero() || retainFrom.UTC().Year() < 1 || retainFrom.UTC().Year() > 9999 {
		return usage.ReconciliationReport{}, usage.ErrDiskUsageLimit
	}
	return verifyUsageScope(ctx, cfg, 0, &retainFrom, nil)
}

func verifyUsageScope(ctx context.Context, cfg config.Config, after uint64, retainFrom *time.Time, beforeRevalidate func() error) (usage.ReconciliationReport, error) {
	return verifyUsageScopePolicy(ctx, cfg, after, retainFrom, beforeRevalidate, false)
}

func verifyUsageScopePolicy(ctx context.Context, cfg config.Config, after uint64, retainFrom *time.Time, beforeRevalidate func() error, selectPolicy bool) (usage.ReconciliationReport, error) {
	var verified usage.ArchiveVerification
	var dates usage.ArchiveDateVerification
	scope, err := withUsageDiskReference(ctx, cfg, func(ctx context.Context, disk *usage.DiskProjection, exporter *usage.Exporter, scope usage.ReconciliationReport) error {
		var err error
		if selectPolicy {
			date, policyErr := exporter.ReadAuthenticatedDateRetention(ctx, disk)
			if policyErr != nil {
				if manifestAbsent(cfg) && scope.NativeHead.Sequence == 0 {
					return errors.New("usage holds no manifest: expected on a directory that has not exported usage yet, and indistinguishable from one whose usage tree was erased")
				}
				return fmt.Errorf("authenticate Usage retention policy: %w", policyErr)
			}
			if !date.IsZero() {
				retainFrom = &date
			}
		}
		if retainFrom == nil {
			verified, err = exporter.VerifyDiskArchiveAfter(ctx, disk, after, usage.DefaultArchiveVerifyLimits())
		} else {
			dates, err = exporter.VerifyDiskArchiveRetainedDates(ctx, disk, *retainFrom, usage.DefaultArchiveVerifyLimits())
			verified = dates.ArchiveVerification
		}
		if err != nil {
			if manifestAbsent(cfg) && scope.NativeHead.Sequence == 0 {
				return errors.New("usage holds no manifest: expected on a directory that has not exported usage yet, and indistinguishable from one whose usage tree was erased")
			}
			return fmt.Errorf("verify usage: %w", err)
		}
		return nil
	}, beforeRevalidate)
	if err != nil {
		return usage.ReconciliationReport{}, err
	}
	scope.LedgerRecords, scope.ParquetRecords = int64(verified.SourceRows), int64(verified.ArchiveRows)
	scope.AfterSequence, scope.ThroughSequence = verified.AfterSequence, verified.ThroughSequence
	if retainFrom != nil {
		scope.RetainFromDate = dates.RetainFromDate
		scope.RequiredRows, scope.MatchedRows, scope.ExpiredOmittedRows = int64(dates.RequiredRows), int64(dates.MatchedRows), int64(dates.ExpiredOmittedRows)
	}
	return scope, nil
}

// Every offline disk operation owns the same authenticated reference and final
// authority/cleanup barrier. The callback's scope is tentative: neither its
// result nor a success report can escape if final revalidation or cleanup fails.
func withUsageDiskReference(ctx context.Context, cfg config.Config, work func(context.Context, *usage.DiskProjection, *usage.Exporter, usage.ReconciliationReport) error, beforeRevalidate func() error) (report usage.ReconciliationReport, err error) {
	return withUsageDiskReferencePublication(ctx, cfg, work, nil, beforeRevalidate)
}

// A writable offline publication runs only after independent authority checks
// and actual readonly Metadata close, while retaining the exclusive directory
// lock, fresh projections and keys. It may change derivatives, never authority.
func withUsageDiskReferencePublication(ctx context.Context, cfg config.Config, work func(context.Context, *usage.DiskProjection, *usage.Exporter, usage.ReconciliationReport) error, publish func(context.Context, *usage.DiskProjection, *boltstore.Store, usage.ReconciliationReport) error, beforeRevalidate func() error) (report usage.ReconciliationReport, err error) {
	return withUsageDiskReferenceActions(ctx, cfg, work, publish, nil, beforeRevalidate)
}

// Archive coverage publication needs no writable Metadata handle. It runs only
// after independent authority checks and actual readonly Metadata Close, with
// the same data-directory exclusion/fresh references held until final cleanup.
func withUsageDiskReferenceArchivePublication(ctx context.Context, cfg config.Config, work func(context.Context, *usage.DiskProjection, *usage.Exporter, usage.ReconciliationReport) error, publish func(context.Context, *usage.DiskProjection, *usage.Exporter, usage.ReconciliationReport) error, beforeRevalidate func() error) (usage.ReconciliationReport, error) {
	return withUsageDiskReferenceActions(ctx, cfg, work, nil, publish, beforeRevalidate)
}

func withUsageDiskReferenceActions(ctx context.Context, cfg config.Config, work func(context.Context, *usage.DiskProjection, *usage.Exporter, usage.ReconciliationReport) error, publish func(context.Context, *usage.DiskProjection, *boltstore.Store, usage.ReconciliationReport) error, publishArchive func(context.Context, *usage.DiskProjection, *usage.Exporter, usage.ReconciliationReport) error, beforeRevalidate func() error) (report usage.ReconciliationReport, err error) {
	ctx, pressure, err := commandBudgetContext(ctx, cfg)
	if err != nil {
		return report, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	defer func() {
		// Physical Close/join may outlive the last source check. Observe caller
		// cancellation only after every owned dependency has actually closed,
		// before publishing the returned scope.
		err = errors.Join(err, ctx.Err())
		if err != nil {
			report = usage.ReconciliationReport{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if err := replication.RequireMemberRuntime(cfg.Storage.DataDir, cfg.Replication != nil, "verify usage"); err != nil {
		return report, err
	}
	dataLock, err := lock.AcquireExistingReadOnly(cfg.Storage.DataDir)
	if err != nil {
		return report, err
	}
	ctx = withCommandDataLock(ctx, dataLock)
	defer func() { err = errors.Join(err, dataLock.Close()) }()
	metadataIdentity, err := os.Lstat(cfg.MetadataPath())
	if err != nil {
		return report, err
	}
	if !metadataIdentity.Mode().IsRegular() {
		return report, errors.New("usage verification requires a regular Metadata authority")
	}
	store, err := openCommandMetadataReadOnly(ctx, cfg, cfg.MetadataPath())
	if err != nil {
		if errors.Is(err, boltstore.ErrSchemaVersionMismatch) {
			return report, fmt.Errorf("%w; usage commands do not migrate metadata. Take a backup with the old binary before running `halro start` to upgrade, or use the binary that wrote this directory", err)
		}
		return report, fmt.Errorf("open metadata for ledger authentication: %w", err)
	}
	defer func() { err = errors.Join(err, store.Close()) }()
	masterKey, err := unlockMasterKey(ctx, cfg, store)
	if err != nil {
		return report, err
	}
	defer clear(masterKey)
	secretVault, err := vault.New(masterKey)
	if err != nil {
		return report, err
	}
	defer secretVault.Close()
	if err := verifyVaultKeyCheck(store, secretVault); err != nil {
		return report, err
	}
	key, err := loadLedgerHMACKey(store, secretVault, masterKey)
	if err != nil {
		return report, err
	}
	defer clear(key)
	if _, _, err := verifySelectedCommandLedgerPrefix(ctx, cfg, key); err != nil {
		return report, err
	}
	checkpoint, err := store.LedgerChainCheckpoint()
	if err != nil {
		return report, err
	}
	metadata, err := store.ReadMetadataAppliedPosition(ctx)
	if err != nil {
		return report, err
	}
	_, native, err := ledger.InspectProjectionCursor(ctx, cfg.LedgerPath(), key, 0, 0)
	if err != nil {
		return report, err
	}
	checkAnchor := func() error {
		current, err := store.LedgerChainCheckpoint()
		if err != nil {
			return err
		}
		if current != checkpoint {
			return errors.New("usage verification trusted checkpoint changed")
		}
		if err := verifyLedgerCheckpoint(native, checkpoint); err != nil {
			return err
		}
		// A checkpoint behind the head must still match its actual historical
		// position/hash; an equally long independent valid chain is not enough.
		if checkpoint.Sequence != 0 {
			prefix, actual, err := ledger.InspectProjectionCursor(ctx, cfg.LedgerPath(), key, checkpoint.Generation, checkpoint.Sequence)
			if err != nil || actual != native || prefix.Hash != checkpoint.Hash || prefix.Head.Offset != checkpoint.Offset {
				return errors.Join(err, errors.New("ledger chain does not match its trusted checkpoint"))
			}
		}
		return nil
	}
	if err := checkAnchor(); err != nil {
		return report, err
	}
	allowed := ledger.ProjectionPrefix{Head: native.Head, Hash: native.ChainHash}
	role := "standalone"
	var member replication.MemberState
	var clusterKey [32]byte
	defer clear(clusterKey[:])
	var ordering replication.OrderingInspection
	if cfg.Replication != nil {
		if err := refuseReseedRequiredMember(cfg); err != nil {
			return report, err
		}
		member, clusterKey, err = readMemberState(cfg, masterKey)
		if err != nil {
			return report, err
		}
		role = string(member.Role)
		through := member.DurableIndex
		switch member.Role {
		case replication.RolePrimary:
		case replication.RoleReplica:
			through = member.AppliedIndex
			if member.Projection.Index != through || metadata.Epoch != member.Projection.MetadataEpoch || metadata.Sequence != member.Projection.MetadataSequence {
				return report, errors.New("Replica Metadata does not match authenticated applied cut")
			}
		default:
			return report, errors.New("usage verification has no authorized member role")
		}
		ordering, err = replication.InspectOrderingLedger(ctx, cfg.OrderingJournalPath(), clusterKey[:], member.ClusterID, member.Incarnation, member.DurableIndex, member.OrderingHeadMAC, through, cfg.LedgerPath(), key)
		if err != nil {
			return report, err
		}
		if ordering.Durable.Index > 0 && ordering.Durable.Term != member.Term {
			return report, errors.New("usage verification ordering term differs from authenticated member term")
		}
		cursor := ordering.Allowed.Stores[replication.StoreLedger-replication.StoreLedger]
		var actual ledger.ChainReport
		allowed, actual, err = ledger.InspectProjectionCursor(ctx, cfg.LedgerPath(), key, cursor.Generation, cursor.Sequence)
		if err != nil || actual != native {
			return report, errors.Join(err, errors.New("usage verification native source changed"))
		}
		if member.Role == replication.RoleReplica {
			cursor := ordering.Allowed.Stores[replication.StoreMetadata-replication.StoreLedger]
			if cursor.Generation != metadata.Epoch || cursor.Sequence != metadata.Sequence {
				return report, errors.New("Replica Metadata differs from applied ordering cursor")
			}
		}
	}
	// Only after complete source/key/checkpoint/member authentication may any
	// derived file be created. Temp files are private and outside native data.
	parent, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return report, err
	}
	dataDir, err := filepath.EvalSymlinks(cfg.Storage.DataDir)
	if err != nil {
		return report, err
	}
	rel, err := filepath.Rel(dataDir, parent)
	if err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return report, errors.Join(err, errors.New("usage verification scratch parent must be outside the native data directory"))
	}
	referenceBudget, err := acquireUsageReferenceBudget(ctx, cfg, pressure, resourcebudget.Recovery)
	if err != nil {
		return report, err
	}
	var cleanupErr error
	defer func() { err = errors.Join(err, referenceBudget.retire(withRetirementDebt(cleanupErr, err))) }()
	directory, err := os.MkdirTemp(parent, "halro-usage-verify-")
	if err != nil {
		return report, err
	}
	identity, err := os.Lstat(directory)
	if err != nil {
		cleanupErr = err
		return report, err
	}
	defer func() {
		named, statErr := os.Lstat(directory)
		if statErr != nil || !os.SameFile(identity, named) {
			cleanupErr = errors.Join(cleanupErr, statErr, errors.New("usage verification scratch identity changed"))
			err = errors.Join(err, cleanupErr)
			return
		}
		cleanupErr = errors.Join(cleanupErr, os.Remove(directory))
		err = errors.Join(err, cleanupErr)
	}()
	var p *ledger.LedgerProjection
	if referenceBudget.ledgerBytes > 0 {
		p, err = ledger.BuildLedgerProjectionPrefixWithLimit(ctx, cfg.LedgerPath(), key, filepath.Join(directory, "ledger.db"), native, allowed, referenceBudget.ledgerBytes)
	} else {
		p, err = ledger.BuildLedgerProjectionPrefix(ctx, cfg.LedgerPath(), key, filepath.Join(directory, "ledger.db"), native, allowed)
	}
	if err != nil {
		return report, err
	}
	defer func() { cleanupErr = errors.Join(cleanupErr, p.Discard()); err = errors.Join(err, cleanupErr) }()
	derivative, disk, err := usage.NewDiskLedgerDerivativeWithLimit(p, filepath.Join(directory, "usage.db"), referenceBudget.usageBytes)
	if err != nil {
		return report, err
	}
	defer func() {
		derivative.Close()
		cleanupErr = errors.Join(cleanupErr, disk.Discard())
		err = errors.Join(err, cleanupErr)
	}()
	actual, partial, err := ledger.InspectReplayAuthenticatedContext(ctx, cfg.LedgerPath(), key, func(r ledger.Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		position := ledger.Watermark{Generation: r.Generation, Sequence: r.Sequence, Offset: r.Offset}
		if position.After(allowed.Head) {
			return nil
		}
		return derivative.ApplyContext(ctx, r)
	})
	if err != nil || partial || actual != native {
		return report, errors.Join(err, errors.New("usage verification source does not match complete authenticated history"))
	}
	stats, err := disk.ReadStats(ctx)
	if err != nil || stats.Watermark.Sequence != allowed.Head.Sequence || stats.Watermark.After(allowed.Head) {
		return report, errors.Join(err, errors.New("usage projection does not reach the allowed Ledger prefix"))
	}
	// Policy authorization is independent of every archive root/checkpoint. The
	// resolver authenticates the complete frozen native source, including bytes
	// beyond a role-permitted prefix, and checks the original physical hash.
	policyPrefix := func(ctx context.Context, head ledger.Watermark) (ledger.ProjectionPrefix, error) {
		if head.After(allowed.Head) {
			return ledger.ProjectionPrefix{}, usage.ErrArchiveDateProof
		}
		prefix, actual, err := ledger.InspectProjectionCursor(ctx, cfg.LedgerPath(), key, head.Generation, head.Sequence)
		if err != nil || actual != native || prefix.Head != head {
			return ledger.ProjectionPrefix{}, errors.Join(err, usage.ErrArchiveDateProof)
		}
		return prefix, nil
	}
	archiveBudget, releaseArchive, err := newOneShotUsageArchiveDiskBudget(ctx, cfg)
	if err != nil {
		return report, err
	}
	var reserveArchiveDisk func(context.Context, int64) (func() error, error)
	if archiveBudget != nil {
		reserveArchiveDisk = archiveBudget.reserve
	}
	exporter, err := usage.NewExporterWithContext(ctx, cfg.UsagePath(), usage.Options{Format: cfg.Usage.ExportFormat, ArchiveDiskBytes: usageArchiveDiskCeiling(cfg), ReserveArchiveDisk: reserveArchiveDisk, ReserveArchiveRAM: archiveBudget.readerReservation(cfg), ReserveArchiveWorkRAM: archiveBudget.workReservation(), DatePolicyKey: key, DatePolicyPrefix: policyPrefix})
	if err != nil {
		_ = releaseArchive()
		return report, err
	}
	defer func() {
		closeErr := exporter.Close()
		err = errors.Join(err, closeErr)
		if closeErr == nil {
			err = errors.Join(err, releaseArchive())
		}
		if err != nil {
			report = usage.ReconciliationReport{}
		}
	}()
	scope := usage.ReconciliationReport{SourceHead: stats.Watermark, AllowedLedgerHead: allowed.Head, NativeHead: native.Head, Role: role, OrderingIndex: ordering.Allowed.Index}
	if err := work(ctx, disk, exporter, scope); err != nil {
		return report, err
	}
	// Revalidate independent authority, never trust a freshly built DB to
	// certify a later source or role. Any failure also withdraws the JSON report.
	if beforeRevalidate != nil {
		if err := beforeRevalidate(); err != nil {
			return report, err
		}
	}
	_, finalNative, err := ledger.InspectProjectionCursor(ctx, cfg.LedgerPath(), key, 0, 0)
	if err != nil || finalNative != native {
		return report, errors.Join(err, errors.New("usage verification native head changed"))
	}
	if err := checkAnchor(); err != nil {
		return report, err
	}
	finalMetadata, err := store.ReadMetadataAppliedPosition(ctx)
	if err != nil || finalMetadata != metadata {
		return report, errors.Join(err, errors.New("usage verification Metadata changed"))
	}
	if cfg.Replication != nil {
		if err := refuseReseedRequiredMember(cfg); err != nil {
			return report, err
		}
		next, nextKey, err := readMemberState(cfg, masterKey)
		defer clear(nextKey[:])
		if err != nil || nextKey != clusterKey || !sameReplicaOwnerBuildCut(member, next) {
			return report, errors.Join(err, errors.New("usage verification member identity or prefix changed"))
		}
		final, err := replication.InspectOrderingLedger(ctx, cfg.OrderingJournalPath(), clusterKey[:], member.ClusterID, member.Incarnation, member.DurableIndex, member.OrderingHeadMAC, ordering.Allowed.Index, cfg.LedgerPath(), key)
		if err != nil || final != ordering {
			return report, errors.Join(err, errors.New("usage verification ordering evidence changed"))
		}
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if err := replication.RequireMemberRuntime(cfg.Storage.DataDir, cfg.Replication != nil, "verify usage"); err != nil {
		return report, err
	}
	namedMetadata, err := os.Lstat(cfg.MetadataPath())
	if err != nil || !namedMetadata.Mode().IsRegular() || !os.SameFile(metadataIdentity, namedMetadata) || metadataIdentity.Size() != namedMetadata.Size() || !metadataIdentity.ModTime().Equal(namedMetadata.ModTime()) {
		return report, errors.Join(err, errors.New("usage verification Metadata file identity changed"))
	}
	if publishArchive != nil {
		if cfg.Replication != nil {
			return report, errors.New("Usage archive activation requires offline Standalone")
		}
		if err := store.Close(); err != nil {
			return report, err
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if err := publishArchive(ctx, disk, exporter, scope); err != nil {
			return report, err
		}
		// An already-published logical root is not rolled back on a late failure;
		// its persistent obligation remains. Returned authority is withdrawn.
		var afterMetadata os.FileInfo
		afterMetadata, err = os.Lstat(cfg.MetadataPath())
		if err != nil || !afterMetadata.Mode().IsRegular() || !os.SameFile(metadataIdentity, afterMetadata) || afterMetadata.Size() != metadataIdentity.Size() || !afterMetadata.ModTime().Equal(metadataIdentity.ModTime()) {
			return report, errors.Join(err, errors.New("Usage archive activation Metadata identity changed"))
		}
		var finalStore *boltstore.Store
		finalStore, err = openCommandMetadataReadOnly(ctx, cfg, cfg.MetadataPath())
		if err != nil {
			return report, err
		}
		defer func() { err = errors.Join(err, finalStore.Close()) }()
		finalCheckpoint, anchorErr := finalStore.LedgerChainCheckpoint()
		finalPosition, positionErr := finalStore.ReadMetadataAppliedPosition(ctx)
		if anchorErr != nil || positionErr != nil || finalCheckpoint != checkpoint || finalPosition != metadata {
			return report, errors.Join(anchorErr, positionErr, errors.New("Usage archive activation Metadata authority changed"))
		}
		if err := finalStore.Close(); err != nil {
			return report, err
		}
		_, afterNative, err := ledger.InspectProjectionCursor(ctx, cfg.LedgerPath(), key, 0, 0)
		if err != nil || afterNative != native {
			return report, errors.Join(err, errors.New("Usage archive activation native source changed"))
		}
		if err := replication.GuardUnavailableRuntime(cfg.Storage.DataDir, false, "activate Usage date retention"); err != nil {
			return report, err
		}
	}
	if publish != nil {
		if cfg.Replication != nil {
			return report, errors.New("Usage derivative publication requires offline Standalone")
		}
		if err := store.Close(); err != nil {
			return report, err
		}
		var writable *boltstore.Store
		writable, err = openCommandMetadataDerivedMaintenance(ctx, cfg, cfg.MetadataPath(), metadataIdentity)
		if err != nil {
			return report, err
		}
		defer func() { err = errors.Join(err, writable.Close()) }()
		actualMetadata, err := writable.ReadMetadataAppliedPosition(ctx)
		actualCheckpoint, anchorErr := writable.LedgerChainCheckpoint()
		if err != nil || anchorErr != nil || actualMetadata != metadata || actualCheckpoint != checkpoint {
			return report, errors.Join(err, anchorErr, errors.New("Usage publication Metadata authority changed"))
		}
		if err := publish(ctx, disk, writable, scope); err != nil {
			return report, err
		}
		finalCheckpoint, err := writable.LedgerChainCheckpoint()
		if err != nil || finalCheckpoint != checkpoint {
			return report, errors.Join(err, errors.New("Usage publication changed Ledger authority"))
		}
		finalPosition, positionErr := writable.ReadMetadataAppliedPosition(ctx)
		if positionErr != nil || finalPosition.Epoch != metadata.Epoch || finalPosition.Sequence != metadata.Sequence {
			return report, errors.Join(positionErr, errors.New("Usage publication changed Metadata authority"))
		}
		if err := writable.Close(); err != nil {
			return report, err
		}
		publishedMetadata, identityErr := os.Lstat(cfg.MetadataPath())
		if identityErr != nil || !publishedMetadata.Mode().IsRegular() || !os.SameFile(metadataIdentity, publishedMetadata) {
			return report, errors.Join(identityErr, errors.New("Usage publication Metadata file was replaced"))
		}
		_, afterPublication, err := ledger.InspectProjectionCursor(ctx, cfg.LedgerPath(), key, 0, 0)
		if err != nil || afterPublication != native {
			return report, errors.Join(err, errors.New("Usage publication native source changed"))
		}
		if err := replication.RequireMemberRuntime(cfg.Storage.DataDir, false, "publish Usage derivatives"); err != nil {
			return report, err
		}
	}
	return scope, nil
}
