package metadatastate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/akz142857/Halro/internal/authenticatedstate"
	"github.com/akz142857/Halro/internal/durable"
	"github.com/akz142857/Halro/internal/ownedfile"
	"github.com/akz142857/Halro/internal/resourcebudget"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"github.com/akz142857/Halro/internal/store/lock"
	bbolt "go.etcd.io/bbolt"
)

// MetadataImage is a closed, fresh whole-file candidate. Its receipt describes
// authenticated bucket/value content, including sequences and empty buckets;
// the physical digest also detects mutation after construction. It grants no
// Runtime role, native history deletion, key rotation or business readiness.
type MetadataImage struct {
	mu            sync.Mutex
	path          string
	owner         *ownedfile.Owner
	reservation   resourcebudget.Permit
	budget        *resourcebudget.Manager
	maxBytes      int64
	reference     AuthenticatedMetadataSealReference
	digest        [32]byte
	size          int64
	info          boltstore.MetadataInfo
	published     bool
	removed       bool
	transferred   bool
	retirementErr error
	workDebt      resourcebudget.Permit
	ioDebt        resourcebudget.Permit
}

// decodeRowKey accepts only the canonical encoding emitted by RowKey. No path
// separator, zero escape, marker or depth is guessed when restoring a bucket.
func decodeRowKey(raw []byte) (path [][]byte, key []byte, bucket bool, err error) {
	if len(raw) < 5 || raw[0] != 1 || len(raw) > authenticatedstate.MaxKeyBytes {
		return nil, nil, false, ErrSource
	}
	kind := raw[len(raw)-1]
	if kind > 1 {
		return nil, nil, false, ErrSource
	}
	var components [][]byte
	var part []byte
	for offset := 1; offset < len(raw)-1; {
		c := raw[offset]
		offset++
		if c != 0 {
			part = append(part, c)
			continue
		}
		if offset >= len(raw)-1 {
			return nil, nil, false, ErrSource
		}
		next := raw[offset]
		offset++
		switch next {
		case 255:
			part = append(part, 0)
		case 0:
			if len(part) == 0 || len(components) >= maxBaselineDepth+1 {
				return nil, nil, false, ErrSource
			}
			components = append(components, part)
			part = nil
		default:
			return nil, nil, false, ErrSource
		}
	}
	if len(part) != 0 || len(components) == 0 {
		return nil, nil, false, ErrSource
	}
	bucket = kind == 0
	if bucket {
		path = components
	} else {
		if len(components) < 2 {
			return nil, nil, false, ErrSource
		}
		path, key = components[:len(components)-1], components[len(components)-1]
	}
	canonical, err := RowKey(path, key, bucket)
	if err != nil || !bytes.Equal(canonical, raw) {
		return nil, nil, false, errors.Join(ErrSource, err)
	}
	return path, key, bucket, nil
}

func putMetadataImageRow(tx *bbolt.Tx, row authenticatedstate.Row) error {
	path, key, marker, err := decodeRowKey(row.Key)
	if err != nil {
		return err
	}
	names := make([]string, len(path))
	for i := range path {
		names[i] = string(path[i])
	}
	if _, err := boltstore.MetadataBaselineClass(names, key); err != nil {
		return err
	}
	var bucket *bbolt.Bucket
	for i, name := range path {
		var child *bbolt.Bucket
		if i == 0 {
			child = tx.Bucket(name)
		} else {
			child = bucket.Bucket(name)
		}
		if i == len(path)-1 && marker {
			if child != nil || len(row.Value) != 8 {
				return ErrSource
			}
			if i == 0 {
				child, err = tx.CreateBucket(name)
			} else {
				child, err = bucket.CreateBucket(name)
			}
			if err != nil {
				return err
			}
			return child.SetSequence(binary.BigEndian.Uint64(row.Value))
		}
		if child == nil {
			return ErrSource
		} // parent marker must have preceded the row
		bucket = child
	}
	if bucket.Get(key) != nil || bucket.Bucket(key) != nil {
		return ErrSource
	}
	return bucket.Put(key, row.Value)
}

// materializePhase is a test-only hook; it is nil in production.
var materializePhase func(string)

// MaterializeMetadata reconstructs an exclusive bbolt file from the exact
// selected root, using finite row/byte batches. It never copies a live ordinary
// DB. The owner lock freezes delta/trim and Close cancels and joins the complete
// construction. This conservative profile charges the complete target mapping
// plus fixed workspace; it refuses a ceiling that cannot fit the shared budget.
func (b *Baseline) MaterializeMetadata(ctx context.Context, output string, selected AuthenticatedMetadataSealReference, maxBytes int64, reservation resourcebudget.Permit) (result *MetadataImage, resultErr error) {
	if b == nil || !filepath.IsAbs(output) || maxBytes <= 0 || maxBytes > int64(^uint(0)>>1)-BaselineSourceRAMBytes {
		return nil, ErrSource
	}
	parent, err := os.Lstat(filepath.Dir(output))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return nil, errors.Join(ErrSource, err)
	}
	actual, err := b.SealReference(ctx)
	if err != nil || actual != selected {
		return nil, errors.Join(ErrSource, err)
	}
	if !b.mu.TryLock() {
		return nil, authenticatedstate.ErrBusy
	}
	defer b.mu.Unlock()
	if !b.ready.Load() {
		return nil, authenticatedstate.ErrClosed
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.ctx, cancel)
	defer func() { stop(); cancel() }()
	root, err := b.store.SealRootReference(ctx, selected.Schema)
	if err != nil || root != selected.Root || metadataSealPosition(b.position) != selected.Position {
		return nil, errors.Join(ErrSource, err)
	}
	workspace, err := b.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: maxBytes + BaselineSourceRAMBytes})
	if err != nil {
		return nil, err
	}
	credit, err := reservation.RetainCovering(b.budget, resourcebudget.Cost{Disk: maxBytes})
	if err != nil {
		return nil, errors.Join(err, workspace.Release())
	}
	image := &MetadataImage{path: output, reservation: credit, budget: b.budget, maxBytes: maxBytes, reference: selected}
	defer func() {
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			image.workDebt = workspace
		} else if err := workspace.Release(); err != nil {
			image.workDebt = workspace
			image.retirementErr = errors.Join(image.retirementErr, err)
			result, resultErr = image, errors.Join(resultErr, err)
		}
	}()
	var db *bbolt.DB
	defer func() {
		if db != nil {
			if closeErr := db.Close(); closeErr != nil {
				image.retirementErr = errors.Join(image.retirementErr, ownedfile.ErrRetirement, closeErr)
				resultErr = errors.Join(resultErr, image.retirementErr)
			}
		}
		if errors.Is(resultErr, ownedfile.ErrRetirement) {
			image.retirementErr = errors.Join(image.retirementErr, resultErr)
		}
		if resultErr != nil {
			result = nil
		}
		if result == nil {
			if cleanupErr := image.Discard(); cleanupErr != nil {
				result, resultErr = image, errors.Join(resultErr, cleanupErr)
			}
		}
	}()
	handedToEngine := false
	db, err = bbolt.Open(output, 0o600, &bbolt.Options{FreelistType: bbolt.FreelistMapType, MaxSize: int(maxBytes), OpenFile: func(path string, flags int, mode os.FileMode) (*os.File, error) {
		file, owner, err := ownedfile.Create(path, flags, mode)
		if err == nil {
			image.owner = owner
			handedToEngine = true
		}
		return file, err
	}})
	if err != nil {
		if handedToEngine {
			err = errors.Join(ownedfile.ErrRetirement, err)
		}
		return nil, err
	}
	if materializePhase != nil {
		materializePhase("opened")
	}
	pin, err := b.store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, pin.Close()) }()
	var rows uint64
	cursor := ""
	for {
		page, err := pin.Range(ctx, authenticatedstate.RangeOptions{Lo: []byte{1}, Hi: []byte{2}, Cursor: cursor, MaxRows: 64, MaxBytes: 8 << 20, MaxPages: 4096})
		if err != nil {
			return nil, err
		}
		if err := db.Update(func(tx *bbolt.Tx) error {
			for _, row := range page.Rows {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := putMetadataImageRow(tx, row); err != nil {
					return err
				}
			}
			return ctx.Err()
		}); err != nil {
			return nil, err
		}
		rows += uint64(len(page.Rows))
		if page.Done {
			break
		}
		cursor = page.Cursor
	}
	// The one provenance row is not a bbolt row. Its presence and the complete
	// root count exclude any unhandled or silently skipped namespace.
	if _, found, err := pin.Get(ctx, baselineEvidenceKey); err != nil || !found || rows+1 != selected.Root.Rows {
		return nil, errors.Join(ErrSource, err)
	}
	if err := db.View(func(tx *bbolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		if meta == nil || string(meta.Get([]byte("instance_id"))) != b.evidence.InstanceID {
			return ErrSource
		}
		for _, item := range []struct {
			name  string
			value uint64
		}{{"schema_version", boltstore.CurrentSchemaVersion()}, {"metadata_journal_epoch", selected.Position.Epoch}, {"applied_journal_sequence", selected.Position.Sequence}} {
			raw := meta.Get([]byte(item.name))
			if len(raw) != 8 || binary.BigEndian.Uint64(raw) != item.value {
				return ErrSource
			}
		}
		image.info = boltstore.MetadataInfo{SchemaVersion: boltstore.CurrentSchemaVersion(), TxID: uint64(tx.ID()), MetadataJournalEpoch: selected.Position.Epoch, MetadataJournalSequence: selected.Position.Sequence}
		if epoch := meta.Get([]byte("ledger_feature_epoch")); len(epoch) == 1 {
			image.info.LedgerFeatureEpoch = epoch[0]
		}
		reader := meta.Get([]byte("minimum_ledger_reader_version"))
		if len(reader) > 128 {
			return ErrSource
		}
		image.info.MinimumLedgerReaderVersion = string(reader)
		return ctx.Err()
	}); err != nil {
		return nil, err
	}
	if err := db.Close(); err != nil {
		db = nil
		return nil, errors.Join(ownedfile.ErrRetirement, err)
	}
	db = nil
	image.digest, image.size, err = image.verifyFile(ctx, [32]byte{}, 0)
	if err != nil {
		return nil, err
	}
	if err := durable.SyncDirectory(filepath.Dir(output)); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return image, nil
}

// SnapshotInfo authenticates the still-owned, closed materialization before an
// archive consumes it. Its physical TxID belongs to this fresh image, while the
// independently selected reference retains the original source position.
func (i *MetadataImage) SnapshotInfo(ctx context.Context) (boltstore.MetadataInfo, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.removed || i.retirementErr != nil || i.digest == ([32]byte{}) {
		return boltstore.MetadataInfo{}, errors.Join(ErrSource, i.retirementErr)
	}
	fee, err := i.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 64 << 10})
	if err != nil {
		return boltstore.MetadataInfo{}, err
	}
	_, _, err = i.verifyFile(ctx, i.digest, i.size)
	if !errors.Is(err, ownedfile.ErrRetirement) {
		if releaseErr := fee.Release(); releaseErr != nil {
			i.ioDebt = fee
			i.retirementErr = errors.Join(i.retirementErr, releaseErr)
			err = errors.Join(err, releaseErr)
		}
	} else {
		i.ioDebt = fee
	}
	if err != nil {
		return boltstore.MetadataInfo{}, err
	}
	return i.info, nil
}

// DiskReservation transfers the published file's existing charge to its new
// enclosing lifetime. It grants no unlink permission and must not be refunded
// merely because publication or a Runtime handle has finished.
func (i *MetadataImage) DiskReservation() (resourcebudget.Permit, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.published || i.transferred || i.removed {
		return resourcebudget.Permit{}, ErrSource
	}
	credit, err := i.reservation.Transfer()
	if err != nil {
		return resourcebudget.Permit{}, err
	}
	i.reservation = resourcebudget.Permit{}
	i.transferred = true
	return credit, nil
}

func (i *MetadataImage) verifyFile(ctx context.Context, expected [32]byte, size int64) (digest [32]byte, written int64, resultErr error) {
	return i.verifyFileWithOpen(ctx, expected, size, os.Open)
}

func (i *MetadataImage) verifyFileWithOpen(ctx context.Context, expected [32]byte, size int64, open func(string) (*os.File, error)) (digest [32]byte, written int64, resultErr error) {
	file, err := open(i.path)
	if err != nil {
		return digest, 0, err
	}
	defer func() {
		if closeErr := ownedfile.CloseFile(file); closeErr != nil {
			i.retirementErr = errors.Join(i.retirementErr, ownedfile.ErrRetirement, closeErr)
			resultErr = errors.Join(resultErr, i.retirementErr)
		}
	}()
	if err := i.owner.VerifyFile(file); err != nil {
		return digest, 0, err
	}
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > i.maxBytes {
		return digest, 0, errors.Join(ErrSource, err)
	}
	hash := sha256.New()
	var buffer [64 << 10]byte
	for {
		if err := ctx.Err(); err != nil {
			return digest, written, err
		}
		n, err := file.Read(buffer[:])
		hash.Write(buffer[:n])
		written += int64(n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return digest, written, err
		}
	}
	copy(digest[:], hash.Sum(nil))
	final, err := file.Stat()
	if err != nil || final.Size() != info.Size() || !final.ModTime().Equal(info.ModTime()) || written != info.Size() || expected != ([32]byte{}) && (expected != digest || written != size) {
		return digest, written, errors.Join(ErrSource, err)
	}
	return digest, written, i.owner.VerifyFile(file)
}

// Publish replaces precisely the independently captured destination, after the
// caller has retired every old Metadata reader/writer under the directory lock.
// Selection is checked again by the caller's current common-cut receipt. Rename
// plus directory Sync is the whole-file commit barrier; Sync failure is an
// uncertain publication and keeps the new inode and its disk credit available.
func (i *MetadataImage) Publish(ctx context.Context, destination string, previous os.FileInfo, guard *lock.Lock, selected AuthenticatedMetadataSealReference) (resultErr error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.retirementErr != nil {
		return i.retirementErr
	}
	if i.removed || i.published || guard == nil || !guard.CoversDataDirectory(filepath.Dir(destination)) || filepath.Dir(destination) != filepath.Dir(i.path) || selected != i.reference {
		return ErrSource
	}
	permit, err := i.budget.TryAcquireContext(ctx, resourcebudget.IO, resourcebudget.Cost{RAM: 64 << 10})
	if err != nil {
		return err
	}
	defer func() {
		if !errors.Is(resultErr, ownedfile.ErrRetirement) {
			resultErr = errors.Join(resultErr, permit.Release())
		}
	}()
	if _, _, err := i.verifyFile(ctx, i.digest, i.size); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := i.owner.ReplaceCaptured(destination, previous); err != nil {
		return err
	}
	i.path = destination
	i.published = true
	return durable.SyncDirectory(filepath.Dir(destination))
}

// Discard removes only an unpublished owned candidate. A published Metadata
// file is authoritative; this handle has no permission to unlink or refund it.
func (i *MetadataImage) Discard() error {
	if i == nil {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.retirementErr != nil {
		return i.retirementErr
	}
	if i.published {
		return ErrSource
	}
	if i.removed {
		return nil
	}
	if err := i.owner.Remove(); err != nil {
		return err
	}
	if err := i.reservation.Release(); err != nil {
		return err
	}
	i.removed = true
	return nil
}
