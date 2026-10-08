package governance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/governancelimits"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type startupCacheFixture struct {
	native      *Log
	path        string
	key         []byte
	options     Options
	selected    SelectedSealOpenOptions
	cache       *StartupCache
	first       Record
	expectDebt  bool
	persistDebt bool
}

func nativeStartupBudget(t *testing.T) *resourcebudget.Manager {
	t.Helper()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 32 << 20, Disk: 16 << 20}, RecoveryReserve: resourcebudget.Cost{RAM: 8 << 20}, MaxClaims: 64, RecoveryClaims: 4, MaxOwners: 16, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: 64 * resourcebudget.ClaimRAMBytes}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{RAM: (8 << 20) - 4*resourcebudget.ClaimRAMBytes}
	budget, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := budget.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	return budget
}

func newStartupCacheFixture(t *testing.T, records int) *startupCacheFixture {
	return newStartupCacheFixtureWithBudget(t, records, nativeStartupBudget(t))
}
func newStartupCacheFixtureWithBudget(t *testing.T, records int, budget *resourcebudget.Manager) *startupCacheFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "native.log")
	key := bytes.Repeat([]byte{0x32}, 32)
	options := Options{CacheBudget: budget, CacheRAMBytes: governancelimits.MinCacheRAMBytes}
	native, err := OpenWithOptions(path, key, options)
	if err != nil {
		t.Fatal(err)
	}
	var first Record
	for from := 1; from <= records; from += 128 {
		var batch []Event
		for i := from; i <= records && i < from+128; i++ {
			batch = append(batch, projectionEvent(i))
		}
		rows, err := native.AppendBatch(ctx, batch)
		if err != nil {
			t.Fatal(err)
		}
		if from == 1 {
			first = rows[0]
		}
	}
	identity := [32]byte{9}
	cache, err := native.CaptureStartupCache(ctx, budget, identity)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := native.SealReference(ctx, budget, native.Summary())
	if err != nil {
		t.Fatal(err)
	}
	guard, err := lock.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	fee, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	seal := authenticatedstate.CandidateSeal{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: [32]byte{1}, Incarnation: [16]byte{1}, Generation: 1, Revision: 1, Role: authenticatedstate.SealStandalone, Coverage: authenticatedstate.SealCoverageAuthenticated, MaxPageBytes: 4 << 20, MaxValueBytes: 3 << 20, MaxTreeHeight: 32, PositionCount: 5, RootCount: 1}
	seal.Positions[0] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentLedger, Generation: 1}
	seal.Positions[1] = authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentMetadata, Epoch: 1, Revision: 1, MetadataBaseline: [32]byte{2}}
	seal.Positions[2] = ref.Position
	seal.Positions[3].Component = authenticatedstate.SealComponentAudit
	seal.Positions[4].Component = authenticatedstate.SealComponentUsage
	seal.Roots[0] = ref.Root
	hard := authenticatedstate.SealHardLimits{MaxPageBytes: seal.MaxPageBytes, MaxValueBytes: seal.MaxValueBytes, MaxTreeHeight: seal.MaxTreeHeight, RequiredRoots: []authenticatedstate.SealRootSchema{ref.Schema}}
	pub, err := authenticatedstate.OpenSealCandidatePublisher(dir, guard, key, hard, budget, fee)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pub.Publish(ctx, seal); err != nil {
		t.Fatal(err)
	}
	authority := authenticatedstate.SealEligibility{SchemaVersion: 1, ApplyVersion: 1, IndexVersion: 1, KeyDerivation: 1, Instance: seal.Instance, Incarnation: seal.Incarnation, Role: seal.Role, Coverage: seal.Coverage}
	copy(authority.Positions[:], seal.Positions[:])
	f := &startupCacheFixture{native: native, path: path, key: key, options: options, cache: cache, first: first, selected: SelectedSealOpenOptions{Publisher: pub, Guard: guard, Authority: authority, Cache: cache, Identity: identity}}
	t.Cleanup(func() {
		if err := f.native.Close(); err != nil && !(f.expectDebt && errors.Is(err, ownedfile.ErrRetirement)) {
			t.Error(err)
		}
		if err := f.cache.Close(); err != nil && !(f.expectDebt && errors.Is(err, ownedfile.ErrRetirement)) {
			t.Error(err)
		}
		if err := pub.Close(); err != nil {
			t.Error(err)
		}
		if err := pub.Retire(); err != nil {
			t.Error(err)
		}
		if err := fee.Release(); err != nil {
			t.Error(err)
		}
		if err := guard.Close(); err != nil {
			t.Error(err)
		}
		if got := budget.Snapshot().Used; (!f.expectDebt && !f.persistDebt && got != (resourcebudget.Cost{})) || (f.expectDebt && got.RAM < governancelimits.MinCacheRAMBytes+startupCacheRAMBytes) {
			t.Error("native startup resource lifetime", got)
		}
	})
	return f
}

func (f *startupCacheFixture) persistedCache(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	budget := f.options.CacheBudget
	credit, err := budget.TryAcquire(resourcebudget.Background, resourcebudget.Cost{Disk: MaxStartupCacheBytes})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "startup.cache")
	file, owner, err := ownedfile.Create(path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.cache.WriteTo(ctx, file); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.cache.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := reader.Stat()
	if err != nil {
		t.Fatal(err)
	}
	cache, err := ReadStartupCache(ctx, reader, info.Size(), f.key, f.selected.Identity, budget)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	f.cache = cache
	f.selected.Cache = cache
	t.Cleanup(func() {
		if err := owner.Retire(); err != nil {
			t.Error(err)
		}
		if err := credit.Release(); err != nil {
			t.Error(err)
		}
	})
}

func TestSelectedNativeCachePersistsRingAndReadsOnlyLaterFrames(t *testing.T) {
	f := newStartupCacheFixture(t, 6)
	ctx := context.Background()
	f.persistedCache(t)
	if _, err := f.native.Append(ctx, projectionEvent(7)); err != nil {
		t.Fatal(err)
	}
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	n, receipt, err := OpenWithSelectedSeal(ctx, f.path, f.key, f.options, f.selected)
	if err != nil {
		t.Fatal(err)
	}
	f.native = n
	if receipt.DecodedRecords != 1 || receipt.Head.Records != 7 || receipt.Saved.Records != 6 || n.SelectedStartupReady() {
		t.Fatal(receipt)
	}
	if _, err := n.Append(ctx, projectionEvent(1)); !errors.Is(err, ErrStartupAuditPending) {
		t.Fatal(err)
	}
	if err := n.AuditSelectedStartup(ctx, f.options.CacheBudget); err != nil {
		t.Fatal(err)
	}
	row, err := n.Append(ctx, projectionEvent(1))
	if err != nil || row.Sequence != f.first.Sequence || row.Hash != f.first.Hash || n.Summary().Records != 7 {
		t.Fatal("saved retry became new fact", row, err, n.Summary())
	}
	if !n.SelectedStartupReady() {
		t.Fatal("successful audit did not open native gate")
	}
}

func TestSelectedNativeCacheBoundedFullWindowKeepsEvictionOrder(t *testing.T) {
	f := newStartupCacheFixture(t, maxTrackedEventIDs+7)
	f.persistedCache(t)
	ctx := context.Background()
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	n, r, err := OpenWithSelectedSeal(ctx, f.path, f.key, f.options, f.selected)
	if err != nil {
		t.Fatal(err)
	}
	f.native = n
	if r.DecodedRecords != 0 || n.trackedCount != maxTrackedEventIDs || f.cache.Length() > MaxStartupCacheBytes {
		t.Fatal(r, n.trackedCount, f.cache.Length())
	}
	if err := n.AuditSelectedStartup(ctx, f.options.CacheBudget); err != nil {
		t.Fatal(err)
	}
	old := projectionEvent(8)
	cached, err := n.Append(ctx, old)
	if err != nil || cached.Sequence != 8 {
		t.Fatal(cached, err)
	}
	if _, err := n.Append(ctx, projectionEvent(maxTrackedEventIDs+8)); err != nil {
		t.Fatal(err)
	}
	if _, retained := n.recordsByEventID[shaIdentity(old.EventID)]; retained {
		t.Fatal("restored ring changed eviction order")
	}
}

func TestSelectedNativeCacheRejectsMACIdentityAndWrongAuthorityBeforeRepair(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	ctx := context.Background()
	var encoded bytes.Buffer
	if err := f.cache.WriteTo(ctx, &encoded); err != nil {
		t.Fatal(err)
	}
	before := f.options.CacheBudget.Snapshot().Used
	raw := bytes.Clone(encoded.Bytes())
	raw[len(raw)-1] ^= 1
	if c, err := ReadStartupCache(ctx, bytes.NewReader(raw), int64(len(raw)), f.key, f.selected.Identity, f.options.CacheBudget); c != nil || !errors.Is(err, ErrStartupCache) {
		t.Fatal(c, err)
	}
	if c, err := ReadStartupCache(ctx, bytes.NewReader(encoded.Bytes()), int64(encoded.Len()), f.key, [32]byte{8}, f.options.CacheBudget); c != nil || !errors.Is(err, ErrStartupCache) {
		t.Fatal(c, err)
	}
	if got := f.options.CacheBudget.Snapshot().Used; got != before {
		t.Fatal("failed capsule retained workspace", before, got)
	}
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(f.path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("torn")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	o := f.selected
	o.Authority.Positions[2].Sequence++
	if n, _, err := OpenWithSelectedSeal(ctx, f.path, f.key, f.options, o); n != nil || !errors.Is(err, authenticatedstate.ErrSealIneligible) {
		t.Fatal(n, err)
	}
	got, err := os.Stat(f.path)
	if err != nil || got.Size() != want.Size() {
		t.Fatal("unselected cache repaired source", got, err)
	}
}

func TestSelectedNativeCacheOldPrefixTamperWithdrawsReadiness(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	ctx := context.Background()
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	raw[frameHeaderSize] ^= 1
	if err := os.WriteFile(f.path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	n, receipt, err := OpenWithSelectedSeal(ctx, f.path, f.key, f.options, f.selected)
	if err != nil {
		t.Fatal(err)
	}
	f.native = n
	if receipt.DecodedRecords != 0 || n.SelectedStartupReady() {
		t.Fatal(receipt)
	}
	if err := n.AuditSelectedStartup(ctx, f.options.CacheBudget); err == nil {
		t.Fatal("changed skipped prefix was accepted")
	}
	if n.SelectedStartupReady() {
		t.Fatal("corrupt native history became ready")
	}
}

type startupBlockingWriter struct{ entered, release chan struct{} }

func (w startupBlockingWriter) Write(raw []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(raw), nil
}

func TestStartupCacheCloseJoinsWriterBeforeClearingBytesAndCharge(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	ctx := context.Background()
	before := f.options.CacheBudget.Snapshot().Used
	w := startupBlockingWriter{make(chan struct{}), make(chan struct{})}
	written := make(chan error, 1)
	closed := make(chan error, 1)
	go func() { written <- f.cache.WriteTo(ctx, w) }()
	<-w.entered
	go func() { closed <- f.cache.Close() }()
	<-f.cache.ctx.Done()
	select {
	case err := <-closed:
		t.Fatal("close escaped actual writer", err)
	default:
	}
	if got := f.options.CacheBudget.Snapshot().Used; got != before {
		t.Fatal("blocked writer lost charge", before, got)
	}
	close(w.release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if f.cache.Length() != 0 || f.options.CacheBudget.Snapshot().Used.RAM >= before.RAM {
		t.Fatal("known writer exit did not release capsule")
	}
}

func shaIdentity(value string) [32]byte { return sha256.Sum256([]byte(value)) }

func TestStartupCacheFrozenOwnerRefusesForgedIdentityAndCopiedHead(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	ctx := context.Background()
	budget := f.options.CacheBudget
	if err := f.cache.ValidateFrozenOwner(ctx, f.native, budget, f.selected.Identity, f.native.Summary()); !errors.Is(err, ErrStartupCache) {
		t.Fatal("unfrozen owner qualified", err)
	}
	if err := f.native.WithFrozenStandaloneHead(ctx, budget, func(ctx context.Context, head Summary) error {
		if err := f.cache.ValidateFrozenOwner(ctx, f.native, budget, f.selected.Identity, head); err != nil {
			return err
		}
		if err := f.cache.ValidateFrozenOwner(ctx, f.native, budget, [32]byte{8}, head); !errors.Is(err, ErrStartupCache) {
			t.Fatal("identity relabel qualified", err)
		}
		copyHead := head
		copyHead.Records++
		if err := f.cache.ValidateFrozenOwner(ctx, f.native, budget, f.selected.Identity, copyHead); !errors.Is(err, ErrStartupCache) {
			t.Fatal("copied head authorized wrong cut", err)
		}
		// Alter the encoded identity while preserving the original MAC and exact
		// native head. Validation may not manufacture a new signature for it.
		f.cache.raw[8] ^= 1
		err := f.cache.ValidateFrozenOwner(ctx, f.native, budget, f.selected.Identity, head)
		f.cache.raw[8] ^= 1
		if !errors.Is(err, ErrStartupCache) {
			t.Fatal("caller cache content was re-signed", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "copied.native")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	foreign, err := OpenWithOptions(path, f.key, f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if err := foreign.WithFrozenStandaloneHead(ctx, budget, func(ctx context.Context, head Summary) error {
		if head != f.cache.Head() {
			t.Fatal("fixture did not copy actual head")
		}
		if err := f.cache.ValidateFrozenOwner(ctx, foreign, budget, f.selected.Identity, head); !errors.Is(err, ErrStartupCache) {
			t.Fatal("captured pointer provenance was laundered", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStartupCacheReopenedContentBindsActualFrozenKeyAndBudget(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	f.persistedCache(t)
	ctx := context.Background()
	budget := f.options.CacheBudget
	if f.cache.capturedSource != nil {
		t.Fatal("reader open invented pointer provenance")
	}
	if err := f.native.WithFrozenStandaloneHead(ctx, budget, func(ctx context.Context, head Summary) error {
		if err := f.cache.ValidateFrozenOwner(ctx, f.native, budget, f.selected.Identity, head); err != nil {
			return err
		}
		if err := f.cache.ValidateFrozenOwner(ctx, f.native, nativeStartupBudget(t), f.selected.Identity, head); !errors.Is(err, ErrStartupCache) {
			t.Fatal("foreign budget qualified", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedNativeCacheActualConstructorCloseFailureKeepsCleanupOwnerAndFees(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	ctx := context.Background()
	if err := f.native.Close(); err != nil {
		t.Fatal(err)
	}
	options := f.options
	options.WrapDurability = func(file *os.File) DurabilityWriter {
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	n, _, err := OpenWithSelectedSeal(ctx, f.path, f.key, options, f.selected)
	if n == nil || !errors.Is(err, ownedfile.ErrRetirement) || n.SelectedStartupReady() {
		t.Fatal("actual descriptor debt lacked cleanup owner", n, err)
	}
	f.native = n
	f.expectDebt = true
	before := f.options.CacheBudget.Snapshot().Used
	if !errors.Is(n.Close(), ownedfile.ErrRetirement) || !errors.Is(n.Close(), ownedfile.ErrRetirement) {
		t.Fatal("unknown exit was reclassified")
	}
	if got := f.options.CacheBudget.Snapshot().Used; got != before {
		t.Fatal("unknown exit returned job/lifetime fees", before, got)
	}
}

func TestStartupCacheActualSourceCloseFailureKeepsWholeCaptureJob(t *testing.T) {
	f := newStartupCacheFixture(t, 3)
	ctx := context.Background()
	if err := f.cache.Close(); err != nil {
		t.Fatal(err)
	}
	f.native.closeReaderFile = func(file *os.File) error {
		if err := file.Close(); err != nil {
			return err
		}
		return file.Close()
	}
	c, err := f.native.CaptureStartupCache(ctx, f.options.CacheBudget, f.selected.Identity)
	if c == nil || !errors.Is(err, ownedfile.ErrRetirement) || c.retainedSource != f.native {
		t.Fatal("source IO debt lost whole capture owner", c, err)
	}
	f.cache = c
	f.expectDebt = true
	before := f.options.CacheBudget.Snapshot().Used
	if !errors.Is(c.Close(), ownedfile.ErrRetirement) || !errors.Is(f.native.Close(), ownedfile.ErrRetirement) {
		t.Fatal("actual source exit became known")
	}
	if got := f.options.CacheBudget.Snapshot().Used; got != before {
		t.Fatal("capture/source debt refunded job", before, got)
	}
}
