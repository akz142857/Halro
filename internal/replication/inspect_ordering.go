package replication

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/ownedfile"
	"io"
	"os"
	"syscall"
)

// OrderingInspection separates the role-selected prefix from the physical
// journal head. Header includes authenticated index-zero store heads: a seeded
// baseline is not genesis. Neither these cursors nor frame digests authenticate
// the native store bodies; callers must verify those independently.
type OrderingInspection struct {
	Header  OrderingHeader
	Allowed OrderingPrefix
	Durable OrderingPrefix
	Head    OrderingPrefix
}

var ErrOrderingInspectionRetirement = errors.New("ordering inspection physical close is uncertain")

// InspectOrderingJournal authenticates an existing journal without creating,
// repairing, truncating or syncing it. The caller owns the data-directory lock
// and supplies the authenticated member state's durable index/MAC and a role-
// allowed through index no greater than that durable index. Every physical
// record is checked, including the tail beyond either index. Only fixed-size
// prefix summaries are retained; all failures discard them.
//
// A physical journal ahead of member state is permitted, just as on restart,
// but a torn final record is refused here and must be repaired by the runtime.
// Cancellation is checked per record; this is not a wall-time bound on file IO.
func InspectOrderingJournal(ctx context.Context, path string, key []byte, clusterID, incarnation string, expectedIndex uint64, expectedHead [sha256.Size]byte, through uint64) (report OrderingInspection, err error) {
	return inspectOrderingJournal(ctx, path, key, clusterID, incarnation, expectedIndex, expectedHead, through, nil, nil, 0)
}

// InspectOrderingJournalWithLimit refuses an oversized named/actual regular
// file before decoding, while retaining the complete read-only prefix contract.
func InspectOrderingJournalWithLimit(ctx context.Context, path string, key []byte, clusterID, incarnation string, expectedIndex uint64, expectedHead [sha256.Size]byte, through uint64, maxDiskBytes int64) (OrderingInspection, error) {
	return inspectOrderingJournal(ctx, path, key, clusterID, incarnation, expectedIndex, expectedHead, through, nil, nil, maxDiskBytes)
}

// Hooks only authenticate read-only native evidence. They must not retain a
// record or publish tentative evidence before the entire scan succeeds.
func inspectOrderingJournal(ctx context.Context, path string, key []byte, clusterID, incarnation string, expectedIndex uint64, expectedHead [sha256.Size]byte, through uint64, checkHeader func(OrderingHeader) error, checkRecord func(OrderingHeader, OrderingRecord) error, maxDiskBytes int64) (report OrderingInspection, err error) {
	defer func() {
		if err != nil {
			report = OrderingInspection{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if len(key) != sha256.Size {
		return report, errors.New("ordering journal key must be 32 bytes")
	}
	if maxDiskBytes < 0 {
		return report, ErrOrderingJournalDiskLimit
	}
	if (expectedIndex == 0) != (expectedHead == ([sha256.Size]byte{})) {
		return report, errors.New("ordering journal expected index and head MAC are inconsistent")
	}
	if through > expectedIndex {
		return report, errors.New("ordering inspection prefix exceeds authenticated member durable index")
	}
	named, err := os.Lstat(path)
	if err != nil {
		return report, fmt.Errorf("inspect existing ordering journal: %w", err)
	}
	if !named.Mode().IsRegular() {
		return report, errors.New("ordering inspection requires a regular existing file")
	}
	if maxDiskBytes > 0 && named.Size() > maxDiskBytes {
		return report, ErrOrderingJournalDiskLimit
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return report, fmt.Errorf("open ordering journal for inspection: %w", err)
	}
	defer func() {
		if closeErr := ownedfile.CloseFile(file); closeErr != nil {
			err = errors.Join(err, ErrOrderingInspectionRetirement, ownedfile.CloseError(file, closeErr))
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return report, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(named, info) || named.Size() != info.Size() || !named.ModTime().Equal(info.ModTime()) {
		return report, errors.New("ordering journal changed while opening inspection")
	}
	if maxDiskBytes > 0 && info.Size() > maxDiskBytes {
		return report, ErrOrderingJournalDiskLimit
	}
	reader := io.NewSectionReader(file, 0, info.Size())
	encoded, err := ReadLengthDelimited(reader, orderingHeaderFixedBytes+2*MaxIdentityBytes)
	if err != nil {
		return report, fmt.Errorf("read ordering inspection header: %w", err)
	}
	header, err := UnmarshalOrderingHeader(encoded, key)
	if err != nil {
		return report, fmt.Errorf("authenticate ordering inspection header: %w", err)
	}
	if header.ClusterID != clusterID || header.Incarnation != incarnation {
		return report, errors.New("ordering journal identity does not match authenticated member state")
	}
	if checkHeader != nil {
		if err := checkHeader(header); err != nil {
			return report, err
		}
	}
	report.Header = header
	prefix := OrderingPrefix{ClusterID: header.ClusterID, Incarnation: header.Incarnation, Stores: header.StoreCursors}
	if through == 0 {
		report.Allowed = prefix
	}
	if expectedIndex == 0 {
		report.Durable = prefix
	}
	remaining := info.Size() - int64(len(encoded))
	if remaining%orderingRecordBytes != 0 {
		return report, errors.New("ordering journal has a partial tail; start Halro to repair it before inspection")
	}
	var recordBytes [orderingRecordBytes]byte
	var expectedAtState [sha256.Size]byte
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if _, err := io.ReadFull(reader, recordBytes[:]); err != nil {
			return report, fmt.Errorf("read ordering inspection record: %w", err)
		}
		record, err := UnmarshalOrderingRecord(recordBytes[:], key, header.ClusterID, header.Incarnation, prefix.MAC)
		if err != nil {
			return report, fmt.Errorf("authenticate ordering inspection record after index %d: %w", prefix.Index, err)
		}
		if prefix.Index == ^uint64(0) || record.Index != prefix.Index+1 {
			return report, errors.New("ordering journal index does not continue its authenticated prefix")
		}
		if record.Term < prefix.Term || record.Term > prefix.Term && record.Kind != KindLeadershipEstablished || record.Term == prefix.Term && record.Kind == KindLeadershipEstablished {
			return report, errors.New("ordering journal has an invalid leadership term transition")
		}
		prefix.Stores, err = advanceOrderingCursors(prefix.Stores, record)
		if err != nil {
			return report, fmt.Errorf("ordering inspection native cursor: %w", err)
		}
		if checkRecord != nil {
			if err := checkRecord(header, record); err != nil {
				return report, err
			}
		}
		prefix.Index, prefix.Term, prefix.MAC = record.Index, record.Term, record.MAC
		if record.Index == expectedIndex {
			expectedAtState = record.MAC
			report.Durable = prefix
		}
		if record.Index == through {
			report.Allowed = prefix
		}
		remaining -= orderingRecordBytes
	}
	if prefix.Index < expectedIndex {
		return report, errors.New("ordering journal is missing an authenticated suffix required by member state")
	}
	if expectedAtState != expectedHead {
		return report, errors.New("ordering journal prefix does not match the head MAC in member state")
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	current, err := file.Stat()
	if err != nil {
		return report, err
	}
	named, err = os.Lstat(path)
	if err != nil {
		return report, err
	}
	if !named.Mode().IsRegular() || !os.SameFile(info, current) || !os.SameFile(info, named) || info.Size() != current.Size() || info.Size() != named.Size() || !info.ModTime().Equal(current.ModTime()) || !info.ModTime().Equal(named.ModTime()) {
		return report, errors.New("ordering journal changed during inspection")
	}
	report.Head = prefix
	return report, nil
}
