package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/statecut"
	"github.com/akz142857/Halro/internal/vault"
)

// One finite local whole-group command owns the original stopped foundations,
// guards, intents and actual file creators. Failure keeps it reachable; it is
// never converted into permission to discard roots or historical native facts.
type haGroupFileRotation struct {
	intent         *haGroupRotationIntent
	intentDigest   [32]byte
	slot           int
	members        [3]*haSelectedRuntimeFoundation
	files          [3][6]*haGroupRotationFile
	checkpoints    [3]*HASelectedCheckpoint
	switched       [3]bool
	debt           error
	work           resourcebudget.Permit
	intentNames    [3]os.FileInfo
	opaque         [3][4][haGroupRotationPublishAttempts]resourcebudget.Permit
	phase          func(string) error
	readIntent     func(string) ([]byte, os.FileInfo, error)
	kms            *kmsRotationOptions
	prepaid        [3][6]resourcebudget.Permit
	retainedIntent [3]resourcebudget.Permit
	historyFees    [3][haGroupRotationIntentSlots]resourcebudget.Permit
}
type haGroupFileRotationFailure struct {
	Owner *haGroupFileRotation
	Cause error
}

func (e *haGroupFileRotationFailure) Error() string { return e.Cause.Error() }
func (e *haGroupFileRotationFailure) Unwrap() error { return e.Cause }

// Preparation captures independent authority before a candidate exists. The
// complete configured group and every actual target namespace are checked
// before its first intent is written. Existing finite slots are preserved;
// no familiar pathname grants an old-file unlink capability.
func prepareHAGroupFileRotation(ctx context.Context, foundations []*haSelectedRuntimeFoundation, old, next []byte, operation string) (r *haGroupFileRotation, resultErr error) {
	return prepareHAGroupRotationWithKMS(ctx, foundations, old, next, operation, nil)
}

func prepareHAGroupRotationWithKMS(ctx context.Context, foundations []*haSelectedRuntimeFoundation, old, next []byte, operation string, kms *kmsRotationOptions, phases ...func(string) error) (r *haGroupFileRotation, resultErr error) {
	if len(foundations) < 2 || len(foundations) > 3 || foundations[0] == nil || foundations[0].owners.Components.Budget == nil {
		return nil, authenticatedstate.ErrSealIneligible
	}
	r = &haGroupFileRotation{slot: -1, kms: kms}
	if len(phases) > 0 {
		r.phase = phases[0]
	}
	for n, f := range foundations {
		r.members[n] = f
	}
	var err error
	r.work, err = haGroupRotationIntentWorkspace(ctx, foundations[0].owners.Components.Budget)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				r.debt = errors.Join(r.debt, resultErr)
			}
			resultErr = &haGroupFileRotationFailure{Owner: r, Cause: resultErr}
		}
	}()
	i, err := captureHAGroupRotationIntent(ctx, foundations, old, next, operation)
	if err != nil {
		return r, fmt.Errorf("complete original group capture: %w", err)
	}
	r.intent = i
	if kms != nil {
		for n := 1; n < int(i.Count); n++ {
			if !reflect.DeepEqual(foundations[0].cfg.Storage.MasterKey, foundations[n].cfg.Storage.MasterKey) {
				return r, errors.New("KMS group rotation requires a shared trusted primary/recovery slot policy")
			}
		}
	}
	for slot := range haGroupRotationIntentSlots {
		free := true
		for n := 0; n < int(i.Count); n++ {
			entry := i.Members[n]
			for _, path := range []string{haGroupRotationIntentPath(entry.DataDir, slot), haGroupRotationIntentPath(entry.DataDir, slot) + ".complete", haGroupRotationStagePath(entry.DataDir, slot, "bootstrap"), haGroupRotationStagePath(entry.DataDir, slot, "cluster"), haGroupRotationStagePath(entry.DataDir, slot, "history"), haGroupRotationKeyStage(entry, slot)} {
				_, err := os.Lstat(path)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return r, err
				}
				free = false
			}
		}
		if free {
			r.slot = slot
			break
		}
	}
	if r.slot < 0 {
		return r, authenticatedstate.ErrLimit
	}
	var dataInfo, keyInfo [3]os.FileInfo
	for n := 0; n < int(i.Count); n++ {
		e := i.Members[n]
		f := r.members[n]
		if f.cfg.ResourceBudget.PhysicalTerminalReserve {
			return r, errors.New("group DEK rotation requires original physical vectors to finish and retire before key preparation")
		}
		dataInfo[n], err = os.Lstat(e.DataDir)
		if err != nil || !dataInfo[n].IsDir() || dataInfo[n].Mode()&os.ModeSymlink != 0 {
			return r, errors.Join(ownedGroupNamespaceError(), err)
		}
		actual, info, err := readHAGroupOriginalKey(ctx, f, old, e.KeyFile)
		if err != nil {
			return r, fmt.Errorf("member %s original key artifact: %w", e.Source.Member.NodeID, err)
		}
		same := bytes.Equal(actual, old)
		clear(actual)
		if !same {
			return r, authenticatedstate.ErrSealIneligible
		}
		keyInfo[n] = info
		for prior := 0; prior < n; prior++ {
			if os.SameFile(dataInfo[n], dataInfo[prior]) || os.SameFile(keyInfo[n], keyInfo[prior]) {
				return r, errors.New("group key/data namespace aliases an earlier actual member")
			}
		}
		// All old-key secrets validate before the first durable intent. The
		// facade's independent permission/hook constructor remains unchanged.
		store := f.metadataStore
		if store == nil {
			store, err = f.reopened.OpenHAMetadataStore(ctx)
			if err != nil {
				return r, err
			}
			f.metadataStore = store
		}
		ov, err := vault.New(old)
		if err != nil {
			return r, err
		}
		nv, err := vault.New(next)
		if err != nil {
			ov.Close()
			return r, err
		}
		options, err := selectedVaultRewrite(ctx, ov, nv, e.VaultVersion, i.NewFingerprint, e.Bootstrap.AuditKey, e.Bootstrap.LedgerKey, e.Bootstrap.LegacyMetadataKey)
		if err == nil {
			options.Keyring.PreviousFingerprint = i.OldFingerprint
			options.Keyring.RotationOperationID = i.OperationID
			options.Keyring.RecoveryEnvelope, err = encryptRotationBridge(ov, next)
		}
		if err == nil {
			err = store.PreflightJournaledVaultRewrite(options)
		}
		ov.Close()
		nv.Close()
		if err != nil {
			return r, err
		}
	}
	if err := r.captureCompletedHistory(ctx); err != nil {
		return r, err
	}
	if err := r.preflightPublication(ctx); err != nil {
		return r, err
	}
	if err := r.prepareKMSDescriptor(ctx); err != nil {
		return r, fmt.Errorf("target KMS descriptor: %w", err)
	}
	raw, err := encodeHAGroupRotationIntent(i)
	if err != nil {
		return r, fmt.Errorf("complete group intent encoding: %w", err)
	}
	defer clear(raw)
	r.intentDigest = sha256.Sum256(raw)
	for n := 0; n < int(i.Count); n++ {
		r.files[n][0], err = r.createFile(ctx, n, 0, haGroupRotationIntentPath(i.Members[n].DataDir, r.slot), raw)
		if err != nil {
			return r, err
		}
		r.intentNames[n], err = os.Lstat(haGroupRotationIntentPath(i.Members[n].DataDir, r.slot))
		if err != nil {
			return r, err
		}
		if err := r.callPhase(ctx, fmt.Sprintf("member_%d_intent", n)); err != nil {
			return r, err
		}
	}
	return r, nil
}

func ownedGroupNamespaceError() error { return authenticatedstate.ErrSealIneligible }
func haGroupRotationStagePath(dataDir string, slot int, kind string) string {
	return filepath.Join(dataDir, fmt.Sprintf("ha-group-rotation-%d.%s.pending", slot+1, kind))
}
func haGroupRotationKeyStage(e haGroupRotationMemberIntent, slot int) string {
	node := sha256.Sum256([]byte(e.Source.Member.NodeID))
	return filepath.Join(filepath.Dir(e.KeyFile), fmt.Sprintf(".halro-group-%x-%d.key", node[:8], slot+1))
}

// execute runs only these actual stopped/qualified members. Real original TLS
// transport carries native Metadata facts to the original Replica receiver and
// applier. Provider/admin HTTP producers are never served by this command.
func (r *haGroupFileRotation) execute(ctx context.Context, logger *slog.Logger) (resultErr error) {
	if r == nil || r.intent == nil || logger == nil {
		return authenticatedstate.ErrSealIneligible
	}
	defer func() {
		if resultErr != nil {
			if errors.Is(resultErr, ownedfile.ErrRetirement) {
				r.debt = errors.Join(r.debt, resultErr)
			}
			resultErr = &haGroupFileRotationFailure{Owner: r, Cause: resultErr}
		}
	}()
	i := r.intent
	primary := -1
	for n := 0; n < int(i.Count); n++ {
		f := r.members[n]
		if f.metadataStore == nil || r.checkIntent(ctx, n) != nil {
			return authenticatedstate.ErrSealIneligible
		}
		if f.replication.role == replication.RolePrimary {
			primary = n
		}
		// No accounting/Gateway lane is installed. Metadata already has its
		// original HAFacade and bound Primary/Replica hooks from preparation.
		c := f.reopened.Owners.Components
		if err := c.Usage.ActivateHASelectedTypedLive(ctx, c.NativeLedger, c.Budget); err != nil {
			return err
		}
		if err := c.Ledger.EnableHASelectedBusinessReads(ctx, c.NativeLedger, c.Lease, c.Budget); err != nil {
			return err
		}
		if err := f.owners.Components.Lease.Close(); err != nil {
			return err
		}
		if err := f.prepareSelectedTransport(logger); err != nil {
			return err
		}
	}
	if primary < 0 {
		return replication.ErrMemberAuthority
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 3)
	started := 0
	joined := false
	join := func() error {
		stop()
		var result error
		for range started {
			e := <-done
			if e != nil && !errors.Is(e, context.Canceled) {
				result = errors.Join(result, e)
			}
		}
		joined = true
		return result
	}
	defer func() {
		if !joined {
			resultErr = errors.Join(resultErr, join())
		}
	}()
	for n := 0; n < int(i.Count); n++ {
		f := r.members[n]
		started++
		go func() { done <- f.serveSelectedTransport(runCtx) }()
	}
	for {
		ready := true
		primaryConnections := r.members[primary].replication.manager.PeerConnections()
		primaryNode := i.Members[primary].Source.Member.NodeID
		for n := 0; n < int(i.Count); n++ {
			if n == primary {
				continue
			}
			// Selected transport registers authenticated data sessions only
			// between the actual Primary and its Replicas. Replica-to-Replica
			// control sessions do not grant replication or confirmation.
			if !primaryConnections[i.Members[n].Source.Member.NodeID] || !r.members[n].replication.manager.PeerConnections()[primaryNode] {
				ready = false
			}
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("group Primary-to-Replica transport readiness: %w", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	ov, err := vault.New(i.OldKey)
	if err != nil {
		return err
	}
	defer ov.Close()
	nv, err := vault.New(i.NewKey)
	if err != nil {
		return err
	}
	defer nv.Close()
	e := i.Members[primary]
	options, err := selectedVaultRewrite(ctx, ov, nv, e.VaultVersion, i.NewFingerprint, e.Bootstrap.AuditKey, e.Bootstrap.LedgerKey, e.Bootstrap.LegacyMetadataKey)
	if err != nil {
		return err
	}
	options.Keyring.PreviousFingerprint = i.OldFingerprint
	options.Keyring.RotationOperationID = i.OperationID
	options.Keyring.RecoveryEnvelope, err = encryptRotationBridge(ov, i.NewKey)
	if err != nil {
		return err
	}
	if err := r.bindKMSRewrite(&options, r.members[primary].cfg); err != nil {
		return err
	}
	for {
		progress, err := r.members[primary].metadataStore.RewriteVaultMaterialJournaledStep(options)
		if err != nil {
			return err
		}
		if err := r.callPhase(ctx, "metadata_durable"); err != nil {
			return err
		}
		if progress.Stage == 6 {
			break
		}
	}
	through := r.members[primary].replication.publisher.Snapshot().DurableIndex
	if err := r.members[primary].replication.coordinator.WaitConfirmed(ctx, through); err != nil {
		return err
	}
	for {
		applied := true
		for n := 0; n < int(i.Count); n++ {
			s := r.members[n].replication.publisher.Snapshot()
			if s.DurableIndex != through || s.ConfirmedIndex != through || s.AppliedIndex != through {
				applied = false
			}
		}
		if applied {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := r.callPhase(ctx, "group_confirmed"); err != nil {
		return err
	}
	if err := join(); err != nil {
		return err
	}
	for n := 0; n < int(i.Count); n++ {
		f := r.members[n]
		r.checkpoints[n], err = checkpointHAGroupRotationMember(ctx, f)
		if err != nil {
			return fmt.Errorf("group member %s seven-root publication: %w", i.Members[n].Source.Member.NodeID, err)
		}
		if err := r.callPhase(ctx, fmt.Sprintf("member_%d_common", n)); err != nil {
			return err
		}
	}
	for n := 0; n < int(i.Count); n++ {
		if err := r.checkpoints[n].Close(); err != nil {
			return err
		}
	}
	for n := 0; n < int(i.Count); n++ {
		if err := r.switchMember(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

func (r *haGroupFileRotation) switchMember(ctx context.Context, n int) error {
	i := r.intent
	e := i.Members[n]
	cfg := r.members[n].cfg
	plain, err := json.Marshal(e.Bootstrap)
	if err != nil {
		return err
	}
	defer clear(plain)
	v, err := vault.New(i.NewKey)
	if err != nil {
		return err
	}
	defer v.Close()
	bootstrap, err := v.EncryptCredential(haMemoryBootstrapCredential, "system", haMemoryBootstrapDomain, plain)
	if err != nil {
		return err
	}
	defer clear(bootstrap)
	cluster, err := encodeHAGroupClusterKeyCandidate(cfg, i.ClusterKey, i.NewKey)
	if err != nil {
		return err
	}
	defer clear(cluster)
	keyArtifact := i.NewKey
	if i.Mode == "key_slots" {
		keyArtifact, err = encodeHAGroupTargetUnlock(cfg, i, n)
		if err != nil {
			return err
		}
		defer clear(keyArtifact)
	}
	history, err := encodeHAGroupHistory(cfg, i.NewKey, e.History)
	if err != nil {
		return err
	}
	defer clear(history)
	for _, stage := range []struct {
		kind              int
		path, destination string
		raw               []byte
	}{
		{0, haGroupRotationStagePath(e.DataDir, r.slot, "bootstrap"), haMemoryStateBootstrapPath(cfg), bootstrap},
		{1, haGroupRotationStagePath(e.DataDir, r.slot, "cluster"), haGroupClusterKeyPath(cfg), cluster},
		{3, haGroupRotationStagePath(e.DataDir, r.slot, "history"), haGroupHistoryPath(cfg), history},
		{2, haGroupRotationKeyStage(e, r.slot), e.KeyFile, keyArtifact},
	} {
		if err := r.publishMemberArtifact(ctx, n, stage.kind, stage.path, stage.destination, stage.raw); err != nil {
			return err
		}
		if err := r.callPhase(ctx, fmt.Sprintf("member_%d_file_%d", n, stage.kind)); err != nil {
			return err
		}
	}
	r.switched[n] = true
	return nil
}

// Completion is minted only after ALL actual new-key members were reopened
// through the ordinary independent factories, fully inspected at their target
// seven-root common selection, and checked against the original native MAC
// keys. It clears a pending gate; it supplies no role/readiness/source grant.
func (r *haGroupFileRotation) completeVerified(ctx context.Context, fresh []*haSelectedRuntimeFoundation) (resultErr error) {
	defer func() {
		if r != nil && errors.Is(resultErr, ownedfile.ErrRetirement) {
			r.debt = errors.Join(r.debt, resultErr)
		}
	}()

	if r == nil || r.intent == nil || len(fresh) != int(r.intent.Count) {
		return authenticatedstate.ErrSealIneligible
	}
	for n, f := range fresh {
		if f == nil || !r.switched[n] || f.cfg.Storage.DataDir != r.intent.Members[n].DataDir || f.owners.Components.Guard != r.members[n].owners.Components.Guard || f.bootstrap.Binding != r.intent.Members[n].Bootstrap.Binding || f.reopened == nil {
			return authenticatedstate.ErrSealIneligible
		}
		source, err := statecut.CaptureHAKeyRotationSource(ctx, f.reopened.Owners)
		if err != nil {
			return err
		}
		if source.Selected != r.checkpoints[n].Publication.Seal || source.Member.DurableIndex != source.Member.ConfirmedIndex || source.Member.AppliedIndex != source.Member.DurableIndex || source.Member.Role != r.intent.Members[n].Source.Member.Role || source.Member.Term != r.intent.Members[n].Source.Member.Term {
			return authenticatedstate.ErrSealIneligible
		}
		if err := verifyHAGroupNewMaterial(ctx, f, r.intent, n); err != nil {
			return err
		}
	}
	// Randomized envelope creation must never replace the persisted intent.
	// Read its original fully joined bytes and compare the creation namespace.
	for n := 0; n < int(r.intent.Count); n++ {
		if err := r.checkIntent(ctx, n); err != nil {
			return err
		}
		actual, _, err := r.readActualIntent(haGroupRotationIntentPath(r.intent.Members[n].DataDir, r.slot))
		if err != nil {
			return err
		}
		if sha256.Sum256(actual) != r.intentDigest {
			clear(actual)
			return authenticatedstate.ErrSealIneligible
		}
		done := haGroupRotationDoneBytesFor(r.intent, actual, r.intent.Members[n].Source.Member.NodeID)
		clear(actual)
		donePath := haGroupRotationIntentPath(r.intent.Members[n].DataDir, r.slot) + ".complete"
		existing, _, readErr := readHAGroupRotationBytes(donePath)
		if readErr == nil {
			same := bytes.Equal(existing, done)
			clear(existing)
			clear(done)
			if !same {
				return authenticatedstate.ErrSealIneligible
			}
			continue
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			clear(existing)
			clear(done)
			return readErr
		}
		r.files[n][5], err = r.createFile(ctx, n, 5, donePath, done)
		clear(done)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *haGroupFileRotation) checkIntent(ctx context.Context, n int) (resultErr error) {
	defer func() {
		if r != nil && errors.Is(resultErr, ownedfile.ErrRetirement) {
			r.debt = errors.Join(r.debt, resultErr)
		}
	}()

	if r == nil || r.intent == nil || n < 0 || n >= int(r.intent.Count) || r.intentNames[n] == nil || r.work == (resourcebudget.Permit{}) {
		return authenticatedstate.ErrSealIneligible
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, info, err := r.readActualIntent(haGroupRotationIntentPath(r.intent.Members[n].DataDir, r.slot))
	defer clear(raw)
	if err != nil {
		return err
	}
	if !sameHAGroupReadFile(r.intentNames[n], info) || sha256.Sum256(raw) != r.intentDigest {
		return authenticatedstate.ErrSealIneligible
	}
	return nil
}

// Close joins original producers and actual FD owners before clearing sensitive
// authorization bytes or returning their workspace. Persistent bytes stay paid.
func (r *haGroupFileRotation) Close() error {
	if r == nil {
		return nil
	}
	for _, c := range r.checkpoints {
		if c != nil {
			r.debt = errors.Join(r.debt, c.Close())
		}
	}
	for _, f := range r.members {
		if f != nil {
			r.debt = errors.Join(r.debt, f.Close())
		}
	}
	for _, row := range r.files {
		for _, f := range row {
			if f != nil {
				r.debt = errors.Join(r.debt, f.Close())
			}
		}
	}
	if r.debt != nil {
		return r.debt
	}
	for n := range r.prepaid {
		for k := range r.prepaid[n] {
			if r.prepaid[n][k] != (resourcebudget.Permit{}) {
				if err := r.prepaid[n][k].Release(); err != nil {
					r.debt = errors.Join(r.debt, err)
				} else {
					r.prepaid[n][k] = resourcebudget.Permit{}
				}
			}
		}
	}
	if r.debt != nil {
		return r.debt
	}
	if r.intent != nil {
		r.intent.clearKeys()
	}
	if r.work != (resourcebudget.Permit{}) {
		r.debt = r.work.Release()
		if r.debt == nil {
			r.work = resourcebudget.Permit{}
		}
	}
	return r.debt
}

func (r *haGroupFileRotation) callPhase(ctx context.Context, phase string) error {
	if r.phase != nil {
		return r.phase(phase)
	}
	return ctx.Err()
}

func (r *haGroupFileRotation) readActualIntent(path string) ([]byte, os.FileInfo, error) {
	if r.readIntent != nil {
		return r.readIntent(path)
	}
	return readHAGroupRotationBytes(path)
}
