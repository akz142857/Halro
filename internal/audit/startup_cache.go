package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"

	"github.com/akz142857/Halro/internal/auditlimits"
	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

const startupCacheHeaderBytes = 96
const startupCacheSlotBytes = 112
const MaxStartupCacheBytes = startupCacheHeaderBytes + maxTrackedEventIDs*startupCacheSlotBytes + sha256.Size
const startupCacheRAMBytes int64 = auditlimits.StartupCacheReadRAMBytes

var ErrStartupCache = errors.New("audit native startup cache is not eligible")
var ErrStartupAuditPending = errors.New("audit selected startup requires historical audit before writes or readiness")

// StartupCache owns the bounded authenticated retry ring and its RAM charge.
// It supplements the selected native head; it is not a seal or startup authority.
// Its persistence/reader is caller-owned and needs independent disk/IO ownership.
type StartupCache struct {
	mu             sync.Mutex
	ctx            context.Context
	cancel         context.CancelFunc
	budget         *resourcebudget.Manager
	permit         resourcebudget.Permit
	raw            []byte
	encodedDigest  [32]byte
	identity       [32]byte
	head           Summary
	closed         bool
	err            error
	retainedSource *Log
	capturedSource *Log
}

func startupCacheMAC(key, raw []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("halro:audit:startup-retry-cache:v1\x00"))
	mac.Write(raw)
	return mac.Sum(nil)
}

func newStartupCache(budget *resourcebudget.Manager, permit resourcebudget.Permit, raw []byte, identity [32]byte, head Summary) *StartupCache {
	ctx, cancel := context.WithCancel(context.Background())
	return &StartupCache{ctx: ctx, cancel: cancel, budget: budget, permit: permit, raw: raw, encodedDigest: sha256.Sum256(raw), identity: identity, head: head}
}

// CaptureStartupCache freezes actual standalone native mutation and fully
// authenticates its bytes before/after copying the exact owned cache ring.
// The returned object keeps its encoded-byte fee until Close, including WriteTo.
func (l *Log) CaptureStartupCache(ctx context.Context, budget *resourcebudget.Manager, identity [32]byte) (*StartupCache, error) {
	return l.captureStartupCache(ctx, budget, identity, l.WithFrozenStandaloneHead)
}

func (l *Log) captureStartupCache(ctx context.Context, budget *resourcebudget.Manager, identity [32]byte, freeze func(context.Context, *resourcebudget.Manager, func(context.Context, Summary) error) error) (result *StartupCache, resultErr error) {
	if l == nil || budget == nil || identity == ([32]byte{}) || l.cacheBudget != budget {
		return nil, ErrStartupCache
	}
	credit, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: startupCacheRAMBytes})
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			resultErr = errors.Join(resultErr, credit.Release())
		}
	}()
	resultErr = freeze(ctx, budget, func(ctx context.Context, head Summary) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.startupAuditPending && (l.startupAudit == nil || !l.startupAudit.Snapshot().Complete || l.startupAudit.Snapshot().Err != nil) || len(l.recordsByEventID) != l.trackedCount || l.trackedCount < 0 || l.trackedCount > maxTrackedEventIDs {
			return ErrStartupCache
		}
		raw := make([]byte, startupCacheHeaderBytes+l.trackedCount*startupCacheSlotBytes+sha256.Size)
		copy(raw[:8], "HAUCAC01")
		copy(raw[8:40], identity[:])
		binary.BigEndian.PutUint64(raw[40:48], head.Records)
		binary.BigEndian.PutUint64(raw[48:56], uint64(head.Bytes))
		copy(raw[56:88], head.LastHash[:])
		binary.BigEndian.PutUint32(raw[88:92], uint32(l.trackedCount))
		start := 0
		if l.trackedCount == maxTrackedEventIDs {
			start = l.trackedNext
		}
		for i := 0; i < l.trackedCount; i++ {
			id := l.trackedOrder[(start+i)%maxTrackedEventIDs]
			record, found := l.recordsByEventID[id]
			if !found {
				return ErrStartupCache
			}
			slot := raw[startupCacheHeaderBytes+i*startupCacheSlotBytes:]
			copy(slot[:32], id[:])
			copy(slot[32:64], record.PayloadHash[:])
			binary.BigEndian.PutUint64(slot[64:72], record.Sequence)
			binary.BigEndian.PutUint64(slot[72:80], uint64(record.Offset))
			copy(slot[80:112], record.Hash[:])
		}
		copy(raw[len(raw)-sha256.Size:], startupCacheMAC(l.key, raw[:len(raw)-sha256.Size]))
		result = newStartupCache(budget, credit, raw, identity, head)
		result.capturedSource = l
		return ctx.Err()
	})
	if resultErr != nil {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			if result == nil {
				result = newStartupCache(budget, credit, nil, identity, Summary{})
			}
			result.closed = true
			result.err = resultErr
			result.retainedSource = l
			result.cancel()
			transferred = true
			return result, resultErr
		}
		if result != nil {
			clear(result.raw)
			result.cancel()
			result = nil
		}
		return nil, resultErr
	}
	transferred = true
	return result, nil
}

// ReadStartupCache reads one bounded capsule from a caller-owned reader.
// length must come from that reader's independently pinned extent. No borrowed
// descriptor is closed here. A failed capsule never authorizes tail repair.
func ReadStartupCache(ctx context.Context, reader io.Reader, length int64, key []byte, identity [32]byte, budget *resourcebudget.Manager) (result *StartupCache, resultErr error) {
	if reader == nil || budget == nil || len(key) != 32 || identity == ([32]byte{}) || length < startupCacheHeaderBytes+sha256.Size || length > MaxStartupCacheBytes {
		return nil, ErrStartupCache
	}
	credit, err := budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: startupCacheRAMBytes})
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			resultErr = errors.Join(resultErr, credit.Release())
		}
	}()
	raw := make([]byte, int(length))
	if _, err := io.ReadFull(startupContextReader{ctx, reader}, raw); err != nil {
		clear(raw)
		return nil, err
	}
	count := uint64(binary.BigEndian.Uint32(raw[88:92]))
	n := binary.BigEndian.Uint64(raw[40:48])
	offset := binary.BigEndian.Uint64(raw[48:56])
	var hash [32]byte
	copy(hash[:], raw[56:88])
	if string(raw[:8]) != "HAUCAC01" || string(raw[8:40]) != string(identity[:]) || count > maxTrackedEventIDs || count > n || offset > math.MaxInt64 || int64(startupCacheHeaderBytes+count*startupCacheSlotBytes+sha256.Size) != length || binary.BigEndian.Uint32(raw[92:96]) != 0 || (n == 0 && (offset != 0 || hash != ([32]byte{}) || count != 0)) || (n != 0 && (offset == 0 || hash == ([32]byte{}))) || !hmac.Equal(raw[len(raw)-sha256.Size:], startupCacheMAC(key, raw[:len(raw)-sha256.Size])) {
		clear(raw)
		return nil, ErrStartupCache
	}
	result = newStartupCache(budget, credit, raw, identity, Summary{Records: n, Bytes: int64(offset), LastHash: hash})
	transferred = true
	return result, nil
}

func (c *StartupCache) Head() Summary { c.mu.Lock(); defer c.mu.Unlock(); return c.head }
func (c *StartupCache) Length() int64 { c.mu.Lock(); defer c.mu.Unlock(); return int64(len(c.raw)) }

// WriteTo lends no byte slice. Close first cancels the job and joins an actual
// writer call before clearing bytes and releasing the fixed capsule charge.
func (c *StartupCache) WriteTo(ctx context.Context, w io.Writer) error {
	if c == nil || w == nil {
		return ErrStartupCache
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.Join(ErrStartupCache, c.err)
	}
	jobCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	return writeFull(startupContextWriter{jobCtx, w}, c.raw)
}

func (c *StartupCache) Close() error {
	if c == nil {
		return nil
	}
	c.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.err
	}
	c.closed = true
	clear(c.raw)
	c.raw = nil
	c.err = c.permit.Release()
	return c.err
}

func (c *StartupCache) restore(l *Log) error {
	count := int(binary.BigEndian.Uint32(c.raw[88:92]))
	for i := 0; i < count; i++ {
		slot := c.raw[startupCacheHeaderBytes+i*startupCacheSlotBytes:]
		var id [32]byte
		copy(id[:], slot[:32])
		var r cachedRecord
		copy(r.PayloadHash[:], slot[32:64])
		r.Sequence = binary.BigEndian.Uint64(slot[64:72])
		offset := binary.BigEndian.Uint64(slot[72:80])
		copy(r.Hash[:], slot[80:112])
		if _, duplicate := l.recordsByEventID[id]; duplicate || r.Sequence == 0 || r.Sequence > c.head.Records || offset == 0 || offset > uint64(c.head.Bytes) || r.Hash == ([32]byte{}) {
			return ErrStartupCache
		}
		r.Offset = int64(offset)
		l.recordsByEventID[id] = r
		l.trackedOrder[i] = id
	}
	l.trackedCount = count
	l.trackedNext = count % maxTrackedEventIDs
	return nil
}

// ValidateFrozenOwner binds a capsule to the concrete native owner inside its
// actual registered freeze. A freshly captured capsule cannot move to another
// Log pointer. Reopened content has no pointer provenance, so actual key/MAC,
// identity, head, cache Manager and the active native freeze establish the bind.
// It never signs caller content or grants a publication/readiness permission.
func (c *StartupCache) ValidateFrozenOwner(ctx context.Context, native *Log, budget *resourcebudget.Manager, identity [32]byte, head Summary) error {
	if c == nil || native == nil || budget == nil || identity == ([32]byte{}) || !c.mu.TryLock() {
		return ErrStartupCache
	}
	defer c.mu.Unlock()
	if err := errors.Join(ctx.Err(), c.ctx.Err(), c.err); err != nil {
		return err
	}
	if c.closed || c.budget != budget || c.identity != identity || c.head != head || c.capturedSource != nil && c.capturedSource != native {
		return ErrStartupCache
	}
	native.mu.Lock()
	defer native.mu.Unlock()
	if native.freezeCancel == nil || native.closing || native.file == nil || native.terminalErr != nil || native.cacheBudget != budget || !native.cacheOwned || (Summary{Records: native.sequence, Bytes: native.offset, LastHash: native.lastHash}) != head || len(native.key) != 32 {
		return ErrStartupCache
	}
	if !hmac.Equal(c.raw[len(c.raw)-sha256.Size:], startupCacheMAC(native.key, c.raw[:len(c.raw)-sha256.Size])) {
		return ErrStartupCache
	}
	return ctx.Err()
}

type SelectedSealOpenOptions struct {
	Publisher *authenticatedstate.SealCandidatePublisher
	Guard     *lock.Lock
	Authority authenticatedstate.SealEligibility
	Cache     *StartupCache
	Identity  [32]byte
}
type NativeStartupReceipt struct {
	Saved, Head, Allowed   Summary
	DecodedRecords         uint64
	HistoricalAuditPending bool
}
type selectedNativeStartup struct {
	ctx         context.Context
	cache       *StartupCache
	decoded     uint64
	retainedLog *Log
}

// OpenWithSelectedSeal restores the exact authenticated native retry ring and
// scans only subsequent frames. It requires the actual selected seal publisher,
// explicit standalone authority and the same shared cache Manager. Pending old
// byte audit rejects semantic writes and SelectedStartupReady remains false.
func OpenWithSelectedSeal(ctx context.Context, path string, key []byte, options Options, o SelectedSealOpenOptions) (result *Log, receipt NativeStartupReceipt, resultErr error) {
	return openWithSelectedSeal(ctx, path, key, options, o, nil)
}

func openWithSelectedSeal(ctx context.Context, path string, key []byte, options Options, o SelectedSealOpenOptions, ha *haSelectedStartupProvenance) (result *Log, receipt NativeStartupReceipt, resultErr error) {
	if o.Publisher == nil || o.Guard == nil || o.Cache == nil || o.Identity == ([32]byte{}) || o.Identity != o.Cache.identity || options.CacheBudget == nil || options.CacheBudget != o.Cache.budget || len(key) != 32 || !selectedStartupModeValid(ctx, options, o.Authority, ha) || o.Authority.Coverage != authenticatedstate.SealCoverageAuthenticated || !o.Guard.CoversNativePath(path) {
		return nil, receipt, ErrStartupCache
	}
	if err := o.Publisher.ValidateOwner(o.Guard, options.CacheBudget); err != nil {
		return nil, receipt, err
	}
	seal, found, err := o.Publisher.Select(o.Authority)
	if err != nil || !found {
		return nil, receipt, errors.Join(authenticatedstate.ErrSealIneligible, err)
	}
	if !o.Cache.mu.TryLock() {
		return nil, receipt, ErrNativeFrozen
	}
	defer o.Cache.mu.Unlock()
	c := o.Cache
	if c.closed || c.ctx.Err() != nil || !hmac.Equal(c.raw[len(c.raw)-sha256.Size:], startupCacheMAC(key, c.raw[:len(c.raw)-sha256.Size])) {
		return nil, receipt, ErrStartupCache
	}
	var position authenticatedstate.SealPosition
	var root authenticatedstate.SealRoot
	for _, p := range seal.Positions[:seal.PositionCount] {
		if p.Component == authenticatedstate.SealComponentAudit {
			position = p
		}
	}
	for _, r := range seal.Roots[:seal.RootCount] {
		if r.Kind == authenticatedstate.SealRootNative && r.Component == authenticatedstate.SealComponentAudit && r.IndexID == AuthenticatedAuditSealIndexID && r.Codec == AuthenticatedAuditSealCodec {
			root = r
		}
	}
	want := authenticatedstate.SealPosition{Component: authenticatedstate.SealComponentAudit, Sequence: c.head.Records, Offset: uint64(c.head.Bytes), Hash: c.head.LastHash}
	if position != want || root.Component != want.Component || root.Rows != c.head.Records || (c.head.Records > 0 && (root.Digest != c.head.LastHash || root.Offset+uint64(root.Length) != uint64(c.head.Bytes))) {
		return nil, receipt, ErrStartupCache
	}
	jobCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	start := &selectedNativeStartup{ctx: jobCtx, cache: c}
	work, err := options.CacheBudget.TryAcquireContext(jobCtx, resourcebudget.IO, resourcebudget.Cost{RAM: startupCacheRAMBytes})
	if err != nil {
		return nil, receipt, err
	}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			if result == nil {
				result = start.retainedLog
			}
			if result != nil {
				result.startupDebt = work
			}
			return
		}
		if err := work.Release(); err != nil {
			resultErr = errors.Join(resultErr, err)
			if result != nil {
				result.startupDebt = work
				result.terminalErr = errors.Join(result.terminalErr, err)
			}
		}
	}()
	options.selectedStartup = start
	options.RequireExisting = true
	result, err = OpenWithOptions(path, key, options)
	if err != nil {
		return start.retainedLog, receipt, err
	}
	current, found, err := o.Publisher.Select(o.Authority)
	if err != nil || !found || current != seal || jobCtx.Err() != nil {
		resultErr = errors.Join(ErrStartupCache, err, jobCtx.Err(), result.Close())
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			result = nil
		}
		return result, receipt, resultErr
	}
	result.startupSaved = c.head
	result.startupProvenance = &selectedAuditProvenance{publisher: o.Publisher, guard: o.Guard, seal: seal, identity: o.Identity, ha: ha}
	receipt = NativeStartupReceipt{Saved: c.head, Head: result.Summary(), DecodedRecords: start.decoded, HistoricalAuditPending: true}
	return result, receipt, nil
}

func (l *Log) SelectedStartupReady() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return (!l.startupAuditPending || l.startupAudit != nil && l.startupAudit.Snapshot().Ready) && !l.closing && l.file != nil && l.terminalErr == nil && l.freezeCancel == nil
}
func (l *Log) AuditSelectedStartup(ctx context.Context, budget *resourcebudget.Manager) error {
	if l == nil || budget == nil || l.cacheBudget != budget {
		return ErrStartupCache
	}
	l.mu.Lock()
	pending := l.startupAuditPending
	active := l.startupAudit
	l.mu.Unlock()
	if active != nil {
		return active.Wait(ctx)
	}
	if !pending {
		return nil
	}
	if err := l.WithFrozenStandaloneHead(ctx, budget, func(ctx context.Context, _ Summary) error { return ctx.Err() }); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing || l.file == nil || l.terminalErr != nil || ctx.Err() != nil {
		return errors.Join(ErrStartupCache, l.terminalErr, ctx.Err())
	}
	l.startupAuditPending = false
	return nil
}

type startupContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r startupContextReader) Read(raw []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(raw)
}

type startupContextSeeker struct {
	ctx    context.Context
	reader io.ReadSeeker
}

func (r startupContextSeeker) Read(raw []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(raw)
}
func (r startupContextSeeker) Seek(offset int64, whence int) (int64, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Seek(offset, whence)
}

type startupContextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w startupContextWriter) Write(raw []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(raw)
}
