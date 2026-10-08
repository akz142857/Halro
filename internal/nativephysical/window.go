// Package nativephysical owns finite, authenticated allocation receipts for an
// already qualified native descriptor. It does not select a root or confer an
// HA role, source position, confirmation, or append permission.
package nativephysical

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// Four finite opaque preservation slots cover interrupted first creation. The
// unknown files are moved without interpreting or acquiring deletion rights.
const ControlDiskBytes int64 = 8192 + 4*(8192+4096)
const MaxReservations = 32

var ErrOwnership = errors.New("native physical owner is not qualified")
var ErrBusy = errors.New("native physical encoder is occupied")

// Position must come from authentication of the actual native descriptor. A
// receipt position is compared with independently authenticated bytes on restart.
type Position struct {
	Sequence uint64
	Hash     [32]byte
	EOF      int64
}

type Reference struct {
	Native, Parent resourcebudget.PhysicalIdentity
	Head           Position
	End            int64
}

type Options struct {
	Manager                             *resourcebudget.Manager
	NativeDisk, ControlDisk             resourcebudget.Permit
	MaxDiskBytes, AppendBytes, RAMBytes int64
	WindowBytes                         int64
	Identity                            [32]byte
	Restart                             bool
	// Authenticate reads only an earlier receipt prefix on this actual file.
	// The native owner keeps its append mutex throughout preparation.
	Authenticate func(context.Context, Position) error
	Phase        func(string) error
}

type receipt struct {
	Version        int
	Serial         uint64
	Identity       [32]byte
	Native, Parent resourcebudget.PhysicalIdentity
	Banks          [2]resourcebudget.PhysicalIdentity
	Head           Position
	End            int64
	MAC            []byte
}

// Window is always retained by its actual native parent, including a partial
// preparation or uncertain close. Its fixed RAM fee covers one encoder; no
// parallel caller can grow another payload graph before acquiring that encoder.
type Window struct {
	mu            sync.Mutex
	encoder       chan struct{}
	ctx           context.Context
	cancel        context.CancelFunc
	encoders      int
	file          *os.File
	owner         *ownedfile.Owner
	path          string
	parent        *os.File
	controls      [2]*os.File
	controlOwners [2]*ownedfile.Owner
	key           [32]byte
	opts          Options
	receipt       receipt
	fees          [3]resourcebudget.Permit
	reserved      int
	reservedBytes int64
	closed        bool
	failure       error
}

func mac(r receipt, key []byte) ([]byte, error) {
	r.MAC = nil
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte("halro:native:physical-window:v1\x00"))
	h.Write(raw)
	return h.Sum(nil), nil
}

// Prepare uses only the original native descriptor. The caller must already
// prove independent selected construction, role, full native MAC head and data
// directory exclusion. No reopened file or caller-supplied header substitutes.
// The returned owner must remain reachable on EVERY non-nil result.
func Prepare(ctx context.Context, path string, file *os.File, key []byte, head Position, o Options) (w *Window, resultErr error) {
	if ctx == nil || file == nil || len(key) != 32 || o.Manager == nil || o.Identity == ([32]byte{}) || o.Authenticate == nil || o.AppendBytes <= 0 || o.RAMBytes < ControlDiskBytes || o.MaxDiskBytes < head.EOF || head.EOF < 0 || o.AppendBytes > o.MaxDiskBytes-head.EOF {
		return nil, ErrOwnership
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if o.WindowBytes == 0 {
		o.WindowBytes = o.AppendBytes
	}
	if o.WindowBytes < o.AppendBytes || o.WindowBytes > o.MaxDiskBytes-head.EOF {
		return nil, ErrOwnership
	}
	x := &Window{file: file, path: abs, opts: o, encoder: make(chan struct{}, 1)}
	x.ctx, x.cancel = context.WithCancel(context.Background())
	x.encoder <- struct{}{}
	copy(x.key[:], key)
	x.fees[0], err = o.NativeDisk.RetainCovering(o.Manager, resourcebudget.Cost{Disk: o.MaxDiskBytes})
	if err != nil {
		return nil, err
	}
	x.fees[1], err = o.ControlDisk.RetainCovering(o.Manager, resourcebudget.Cost{Disk: ControlDiskBytes})
	if err != nil {
		return nil, errors.Join(err, x.fees[0].Release())
	}
	x.fees[2], err = o.Manager.TryAcquire(resourcebudget.IO, resourcebudget.Cost{RAM: o.RAMBytes})
	if err != nil {
		return nil, errors.Join(err, x.fees[1].Release(), x.fees[0].Release())
	}
	w = x
	defer func() {
		if resultErr != nil {
			x.failure = resultErr
		}
	}()
	if err := ctx.Err(); err != nil {
		return x, err
	}
	info, err := file.Stat()
	if err != nil {
		return x, err
	}
	parentInfo, err := os.Lstat(filepath.Dir(abs))
	if err != nil {
		return x, err
	}
	x.owner, err = ownedfile.RestoreAuthenticated(abs, file, info, parentInfo)
	if err != nil {
		return x, err
	}
	if !info.Mode().IsRegular() || info.Size() != head.EOF {
		return x, ErrOwnership
	}
	x.parent, err = os.Open(filepath.Dir(abs))
	if err != nil {
		return x, err
	}
	nativeID, err := resourcebudget.CapturePhysicalIdentity(file)
	if err != nil {
		return x, err
	}
	parentID, err := resourcebudget.CapturePhysicalIdentity(x.parent)
	if err != nil {
		return x, err
	}
	x.receipt = receipt{Version: 1, Identity: o.Identity, Native: nativeID, Parent: parentID, Head: head, End: head.EOF}
	if o.Restart {
		validReceipt, err := hasAuthenticatedReceipt(abs, x.key[:])
		if err != nil {
			return x, err
		}
		if !validReceipt {
			if err := resourcebudget.CheckPhysicalTail(file, head.EOF, head.EOF); err != nil {
				return x, err
			}
			if err := preserveUnqualifiedControls(ctx, abs); err != nil {
				return x, err
			}
			o.Restart = false
		}
	}
	if o.Restart {
		var records [2]receipt
		var valid [2]bool
		for i := range x.controls {
			x.controls[i], err = os.OpenFile(x.controlPath(i), os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return x, err
			}
			bi, e := x.controls[i].Stat()
			if e != nil || !bi.Mode().IsRegular() || (bi.Size() != 4096 && bi.Size() != 0) {
				return x, errors.Join(ErrOwnership, e)
			}
			x.controlOwners[i], err = ownedfile.RestoreAuthenticated(x.controlPath(i), x.controls[i], bi, parentInfo)
			if err != nil {
				return x, err
			}
			var raw [4096]byte
			if bi.Size() == 0 {
				continue
			}
			if _, err = io.ReadFull(io.NewSectionReader(x.controls[i], 0, 4096), raw[:]); err != nil {
				return x, err
			}
			r := &records[i]
			if json.Unmarshal(bytes.TrimRight(raw[:], "\x00"), r) == nil {
				m, e := mac(*r, x.key[:])
				valid[i] = e == nil && hmac.Equal(m, r.MAC)
			}
		}
		chosen := -1
		for i := range valid {
			if valid[i] && (chosen < 0 || records[i].Serial > records[chosen].Serial) {
				chosen = i
			}
		}
		if chosen < 0 {
			return x, ErrOwnership
		}
		r := records[chosen]
		if valid[0] && valid[1] && records[0].Serial == records[1].Serial && !sameReceipt(records[0], records[1]) {
			return x, ErrOwnership
		}
		if r.Version != 1 || r.Identity != o.Identity || r.Native != nativeID || r.Parent != parentID || r.Head.EOF < 0 || r.Head.EOF > head.EOF || r.Head.Sequence > head.Sequence || r.End < head.EOF || r.End > o.MaxDiskBytes {
			return x, ErrOwnership
		}
		for i, f := range x.controls {
			id, e := resourcebudget.CapturePhysicalIdentity(f)
			if e != nil || id != r.Banks[i] {
				return x, errors.Join(ErrOwnership, e)
			}
		}
		if err := o.Authenticate(ctx, r.Head); err != nil {
			return x, err
		}
		if err := resourcebudget.CheckPhysicalTail(file, head.EOF, r.End); err != nil {
			return x, err
		}
		x.receipt = r
		for i, f := range x.controls {
			if err := resourcebudget.CheckPhysicalRange(f, 0, 4096); err != nil {
				return x, err
			}
			info, err := f.Stat()
			if err != nil {
				return x, err
			}
			if info.Size() == 0 {
				if err := x.writeBankLocked(i, r); err != nil {
					return x, err
				}
			}
		}
	} else {
		if err := resourcebudget.CheckPhysicalTail(file, head.EOF, head.EOF); err != nil {
			return x, err
		}
		for i := range x.controls {
			x.controls[i], x.controlOwners[i], err = ownedfile.Create(x.controlPath(i), os.O_RDWR, 0600)
			if err != nil {
				return x, err
			}
			if o.Phase != nil {
				if err := o.Phase(fmt.Sprintf("bank-%d-created", i)); err != nil {
					return x, err
				}
			}
			if err := resourcebudget.AllocatePhysicalRange(x.controls[i], 0, 4096); err != nil {
				return x, err
			}
			x.receipt.Banks[i], err = resourcebudget.CapturePhysicalIdentity(x.controls[i])
			if err != nil {
				return x, err
			}
		}
		// Both fixed names exist durably before the first allocation permission.
		if err := x.parent.Sync(); err != nil {
			return x, err
		}
		initial := x.receipt
		initial.Serial = 1
		for i := range x.controls {
			if err := x.writeBankLocked(i, initial); err != nil {
				return x, err
			}
		}
		x.receipt = initial
	}
	if err := x.prepareLocked(ctx, head); err != nil {
		return x, err
	}
	return x, nil
}

func sameReceipt(a, b receipt) bool {
	a.MAC = nil
	b.MAC = nil
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func (w *Window) controlPath(i int) string { return fmt.Sprintf("%s.physical-window-%d", w.path, i) }
func (w *Window) verifyLocked(head Position) error {
	if w.closed || w.failure != nil {
		return errors.Join(ErrOwnership, w.failure)
	}
	if err := w.owner.VerifyFile(w.file); err != nil {
		return err
	}
	info, err := w.file.Stat()
	if err != nil || info.Size() != head.EOF {
		return errors.Join(ErrOwnership, err)
	}
	native, e := resourcebudget.CapturePhysicalIdentity(w.file)
	if e != nil || native != w.receipt.Native {
		return errors.Join(ErrOwnership, e)
	}
	parent, e := resourcebudget.CapturePhysicalIdentity(w.parent)
	if e != nil || parent != w.receipt.Parent {
		return errors.Join(ErrOwnership, e)
	}
	for i, f := range w.controls {
		if err := w.controlOwners[i].VerifyFile(f); err != nil {
			return err
		}
	}
	return resourcebudget.CheckPhysicalTail(w.file, head.EOF, w.receipt.End)
}
func (w *Window) writeReceiptLocked(r receipt) error {
	if err := w.writeBankLocked(int(r.Serial%2), r); err != nil {
		return err
	}
	w.receipt = r
	return nil
}
func (w *Window) writeBankLocked(i int, r receipt) error {
	m, err := mac(r, w.key[:])
	if err != nil {
		return err
	}
	r.MAC = m
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > 4096 {
		return errors.Join(ErrOwnership, err)
	}
	var block [4096]byte
	copy(block[:], raw)
	if err := w.controlOwners[i].VerifyFile(w.controls[i]); err != nil {
		return err
	}
	n, err := w.controls[i].WriteAt(block[:], 0)
	if err != nil || n != len(block) {
		return errors.Join(io.ErrShortWrite, err)
	}
	if err := w.controls[i].Sync(); err != nil {
		return err
	}
	return nil
}
func (w *Window) prepareLocked(ctx context.Context, head Position) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	end := head.EOF + w.opts.WindowBytes
	if end > w.opts.MaxDiskBytes {
		return resourcebudget.ErrPhysicalAllocation
	}
	if end < w.receipt.End {
		end = w.receipt.End
	}
	r := w.receipt
	r.Serial++
	r.Head = head
	r.End = end
	if r.Serial == 0 {
		return ErrOwnership
	}
	if err := w.writeReceiptLocked(r); err != nil {
		return err
	}
	if w.opts.Phase != nil {
		if err := w.opts.Phase("receipt-synced"); err != nil {
			return err
		}
	}
	if err := resourcebudget.AllocatePhysicalRange(w.file, head.EOF, end-head.EOF); err != nil {
		return err
	}
	if w.opts.Phase != nil {
		if err := w.opts.Phase("native-allocated"); err != nil {
			return err
		}
	}
	return errors.Join(resourcebudget.CheckPhysicalRange(w.file, head.EOF, end), w.verifyLocked(head))
}

// Replenish is pre-admission work under the actual native owner mutex and a
// freshly qualified common cut. Every prior accepted reservation must join.
func (w *Window) Replenish(ctx context.Context, head Position) (err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reserved != 0 {
		return ErrBusy
	}
	if err = w.verifyLocked(head); err != nil {
		return err
	}
	err = w.prepareLocked(ctx, head)
	if err != nil {
		w.failure = err
	}
	return err
}

// Checkpoint records the authenticated current native prefix in an existing
// bank, retaining the original upper range. It allocates no future capacity and
// grants no permission to call Replenish or publish a derived/common root.
func (w *Window) Checkpoint(ctx context.Context, head Position) (err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reserved != 0 {
		return ErrBusy
	}
	if err = w.verifyLocked(head); err != nil {
		return err
	}
	if err = w.opts.Authenticate(ctx, head); err != nil {
		return err
	}
	r := w.receipt
	r.Serial++
	r.Head = head
	if r.Serial == 0 {
		return ErrOwnership
	}
	err = w.writeReceiptLocked(r)
	if err != nil {
		w.failure = err
	}
	return err
}

// Reservation escrows finite actual tail capacity before an accepted producer
// starts. Only the originating Window may consume it under the native mutex.
// Duplicate receives must bypass reservation and verify their existing frames.
type Reservation struct {
	owner          *Window
	bytes          int64
	used, released bool
}

func (w *Window) Reserve(head Position, bytes int64) (*Reservation, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if bytes <= 0 || bytes > w.opts.AppendBytes || w.reserved >= MaxReservations || bytes > w.receipt.End-head.EOF-w.reservedBytes {
		return nil, resourcebudget.ErrPhysicalAllocation
	}
	if err := w.verifyLocked(head); err != nil {
		w.failure = errors.Join(w.failure, err)
		return nil, err
	}
	w.reserved++
	w.reservedBytes += bytes
	return &Reservation{owner: w, bytes: bytes}, nil
}
func (r *Reservation) Release() error {
	if r == nil || r.owner == nil {
		return ErrOwnership
	}
	w := r.owner
	w.mu.Lock()
	defer w.mu.Unlock()
	if r.released {
		return nil
	}
	if r.used {
		return ErrOwnership
	}
	r.released = true
	w.reserved--
	w.reservedBytes -= r.bytes
	return nil
}
func (r *Reservation) BeforeWrite(head Position, size int64) error {
	if r == nil || r.owner == nil {
		return ErrOwnership
	}
	w := r.owner
	w.mu.Lock()
	defer w.mu.Unlock()
	if r.used || r.released || size <= 0 || size > r.bytes || head.EOF > w.receipt.End-size {
		return ErrOwnership
	}
	if err := w.verifyLocked(head); err != nil {
		w.failure = err
		return err
	}
	if err := resourcebudget.CheckPhysicalRange(w.file, head.EOF, head.EOF+size); err != nil {
		return err
	}
	r.used = true
	return nil
}

// Durable joins the consumed reservation after the native fsync. An uncertain
// write keeps the reservation and all fees instead of returning future space.
func (r *Reservation) Durable() error {
	if r == nil || r.owner == nil {
		return ErrOwnership
	}
	w := r.owner
	w.mu.Lock()
	defer w.mu.Unlock()
	if !r.used || r.released {
		return ErrOwnership
	}
	r.released = true
	w.reserved--
	w.reservedBytes -= r.bytes
	return nil
}

// WithEncoder covers payload encoding before the native append mutex. A finite
// number of callers may wait with their original request fee/context, but only
// one payload graph exists. Withdrawal wakes waiting and active borrowers.
func (w *Window) WithEncoder(ctx context.Context, visit func(context.Context) error) error {
	if w == nil || visit == nil {
		return ErrOwnership
	}
	w.mu.Lock()
	bad := w.closed || w.failure != nil || w.ctx.Err() != nil
	failure := w.failure
	if !bad && w.encoders >= MaxReservations {
		w.mu.Unlock()
		return ErrBusy
	}
	if !bad {
		w.encoders++
	}
	w.mu.Unlock()
	if bad {
		return errors.Join(ErrOwnership, failure)
	}
	defer func() { w.mu.Lock(); w.encoders--; w.mu.Unlock() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.ctx.Done():
		return w.ctx.Err()
	case <-w.encoder:
	}
	defer func() { w.encoder <- struct{}{} }()
	job, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(w.ctx, cancel)
	defer func() { stop(); cancel() }()
	if w.ctx.Err() != nil {
		cancel()
	}
	if err := job.Err(); err != nil {
		return err
	}
	return visit(job)
}

// Withdraw does not prove exit or release fees. The native parent calls this
// before joining its real encoder/writer lifetime and actual descriptor Close.
func (w *Window) Withdraw() {
	if w != nil {
		w.cancel()
	}
}

func (w *Window) Hold(cause error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failure = errors.Join(w.failure, cause)
}

// Reference is only physical comparison evidence for a separately qualified
// derived owner. It is captured on the original native descriptor under that
// parent's mutex and cannot supply source/role/root selection authority.
func (w *Window) Reference(head Position) (Reference, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.verifyLocked(head); err != nil {
		return Reference{}, err
	}
	return Reference{Native: w.receipt.Native, Parent: w.receipt.Parent, Head: head, End: w.receipt.End}, nil
}
func (w *Window) VerifyReference(ctx context.Context, head Position, prior Reference) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.verifyLocked(head); err != nil {
		return err
	}
	if prior.Native != w.receipt.Native || prior.Parent != w.receipt.Parent || prior.Head.EOF < 0 || prior.Head.EOF > head.EOF || prior.Head.Sequence > head.Sequence || prior.End < head.EOF || prior.End > w.receipt.End {
		return ErrOwnership
	}
	if err := w.opts.Authenticate(ctx, prior.Head); err != nil {
		return err
	}
	return resourcebudget.CheckPhysicalTail(w.file, head.EOF, prior.End)
}

// CloseAfterNative joins fixed control descriptors only AFTER actual native IO
// has joined. The parent passes its actual result; uncertainty retains keys and
// all original fees. Persisted native extents and receipt names are preserved.
func (w *Window) CloseAfterNative(nativeErr error) error {
	if w == nil {
		return nil
	}
	w.Withdraw()
	<-w.encoder
	defer func() { w.encoder <- struct{}{} }()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.failure
	}
	w.closed = true
	var joined error
	for _, f := range w.controls {
		if f != nil {
			joined = errors.Join(joined, ownedfile.CloseFile(f))
		}
	}
	if w.parent != nil {
		joined = errors.Join(joined, ownedfile.CloseFile(w.parent))
	}
	if joined != nil {
		w.failure = errors.Join(w.failure, ownedfile.ErrRetirement, joined)
		return w.failure
	}
	if nativeErr != nil || w.reserved != 0 || errors.Is(w.failure, ownedfile.ErrRetirement) || errors.Is(w.failure, ownedfile.ErrReplaced) || errors.Is(w.failure, resourcebudget.ErrPhysicalAllocationUncertain) {
		w.failure = errors.Join(w.failure, ownedfile.ErrRetirement, nativeErr)
		return w.failure
	}
	// Native terminal failures retain independently authenticated allocation
	// receipts on disk, but an actually joined descriptor no longer owns RAM.
	for _, p := range w.fees {
		if p != (resourcebudget.Permit{}) {
			if err := p.Release(); err != nil {
				w.failure = errors.Join(w.failure, err)
				return w.failure
			}
		}
	}
	clear(w.key[:])
	w.controls = [2]*os.File{}
	w.parent = nil
	return nil
}
