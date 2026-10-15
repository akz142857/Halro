// Package metadatajournal is the write-ahead log that makes halro.db a
// projection rather than an authority.
//
// bbolt has no log of its own and cannot be replicated, so every authoritative
// metadata write is recorded here as the operations it performed, in durable
// order, before the bbolt transaction it belongs to commits. HA Phase 0a
// (docs/todo/halro-ha-architecture.zh-CN.md §5.2, §6.1) is what this exists
// for, but the path is enabled in Standalone too: a write path that only exists
// under HA never gets Standalone's daily coverage.
//
// The frame format deliberately mirrors internal/audit's — magic, version,
// sequence, previous-frame hash, payload, HMAC — because the two answer the
// same question about their file and there is no reason for a reader to learn
// two shapes. What is added is an epoch, which says which projection of
// halro.db these operations belong to.
//
// # Epochs
//
// Some things replace halro.db as a whole file rather than writing into it:
// the File-mode Master Key rotation bridge, `backup restore`, and
// CompactSnapshot. After such a publish the operations recorded before it are
// no longer a description of the file on disk. So a publish starts a new epoch:
// the published halro.db is the new epoch's starting projection, the applied
// sequence returns to zero, and the previous epoch's frames are deleted.
//
// That rule also closes a path that would otherwise exist: a holder of an old
// Master Key plus the journal could otherwise replay their way to the new key's
// envelope. They cannot, because the old epoch does not exist.
//
// # What is not here
//
// Frames are not encrypted, and that is the same classification bbolt itself
// has: Credentials are AEAD ciphertext, passwords are Argon2id hashes, and no
// caller body — no prompt, no response, no tool argument — is metadata. The
// journal is not a fourth place caller content is stored.
package metadatajournal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	frameMagic   = "HMJL"
	frameVersion = 1
	// frameHeaderSize is magic(4) version(1) kind(1) reserved(2) epoch(8)
	// sequence(8) payload length(4) previous hash(32).
	frameHeaderSize = 60
	frameMACSize    = sha256.Size
	// KeySize is the HMAC key length, matching the Audit log's. The key is
	// derived from the Master Key under its own domain so that holding one
	// subsystem's key grants nothing in another.
	KeySize = 32
	// MaxPayloadSize bounds one transaction's recorded operations.
	//
	// It is generous rather than tight because one transaction legitimately
	// carries a whole price timeline or a batch of coalesced price pins, and a
	// refusal here would refuse a write Halro accepts today. It is bounded at
	// all because a frame is read into memory to authenticate it, and a length
	// field nobody checks is how a corrupt header turns into an allocation.
	MaxPayloadSize = 8 << 20
)

// ErrCorrupt is returned when the file is not what this writer would have
// produced. It is deliberately not recoverable by the reader: the projection
// this journal describes cannot be trusted past the first frame that fails.
var ErrCorrupt = errors.New("metadata journal is corrupt")

// Kind says what a frame carries. Every frame is authenticated the same way;
// the kind decides how its payload reads and where it is allowed to appear.
type Kind uint8

const (
	// KindOperations is one bbolt transaction's recorded writes.
	KindOperations Kind = 1
	// KindEpoch opens a file. It is always sequence 0 with a zero previous
	// hash, and it records which epoch it starts and what the epoch before it
	// ended on — the only surviving trace of an epoch whose frames were
	// deleted by a whole-file publish.
	KindEpoch Kind = 2
	// KindTrim opens a file whose head was cut. It records where the cut fell
	// and what the chain head was there, so a reader can tell a legitimately
	// short file from a truncated one.
	KindTrim Kind = 3
)

func (k Kind) valid() bool {
	return k == KindOperations || k == KindEpoch || k == KindTrim
}

// OpKind is one recorded bbolt operation.
//
// Reads are not recorded and neither is anything derived: the four below are
// the complete set of mutations the store performs, established by walking the
// package's own use of bbolt rather than from the library's API surface.
type OpKind string

const (
	OpPut          OpKind = "put"
	OpDelete       OpKind = "delete"
	OpCreateBucket OpKind = "create_bucket"
	OpDeleteBucket OpKind = "delete_bucket"
)

// Op is one operation, addressed by the bucket path it happened in.
//
// Path is a path rather than a name because buckets nest: a deployment's price
// timeline lives in its own bucket under deployment_price_timeline, so naming
// only the outer bucket would make two different writes indistinguishable.
//
// Every one of these is idempotent by construction, which is what lets an
// interrupted transaction be replayed rather than reasoned about: putting the
// same bytes twice, deleting an absent key, and creating an existing bucket all
// land in the same state as doing it once.
type Op struct {
	Kind  OpKind   `json:"kind"`
	Path  []string `json:"path"`
	Key   []byte   `json:"key,omitempty"`
	Value []byte   `json:"value,omitempty"`
}

func (o Op) validate() error {
	if !o.Kind.valid() {
		return fmt.Errorf("unknown operation %q", o.Kind)
	}
	if len(o.Path) == 0 {
		return errors.New("operation has no bucket path")
	}
	for _, segment := range o.Path {
		if segment == "" {
			return errors.New("operation path has an empty segment")
		}
	}
	switch o.Kind {
	case OpPut:
		if len(o.Key) == 0 {
			return errors.New("put has no key")
		}
	case OpDelete:
		if len(o.Key) == 0 {
			return errors.New("delete has no key")
		}
	case OpCreateBucket, OpDeleteBucket:
		if len(o.Key) != 0 || len(o.Value) != 0 {
			return errors.New("a bucket operation carries no key or value")
		}
	}
	return nil
}

func (k OpKind) valid() bool {
	switch k {
	case OpPut, OpDelete, OpCreateBucket, OpDeleteBucket:
		return true
	}
	return false
}

// EpochHeader is KindEpoch's payload.
type EpochHeader struct {
	Epoch uint64 `json:"epoch"`
	// PreviousEpoch and PreviousChainHead describe what this epoch replaced.
	// Both are zero for the first epoch a data directory ever has.
	PreviousEpoch     uint64 `json:"previous_epoch"`
	PreviousChainHead []byte `json:"previous_chain_head,omitempty"`
	// Reason names the publish that started the epoch — a restore, a key
	// rotation, a compaction, a migration. It is an operator-facing fact and
	// carries no identifiers.
	Reason string `json:"reason"`
}

// TrimAnchor is KindTrim's payload: where the head was cut, and what the chain
// head was at the cut. Without it a trimmed file and a file whose head was
// deleted by someone else read identically.
type TrimAnchor struct {
	Epoch              uint64 `json:"epoch"`
	TrimmedThrough     uint64 `json:"trimmed_through"`
	ChainHeadAtTrim    []byte `json:"chain_head_at_trim"`
	TrimmedFrameCount  uint64 `json:"trimmed_frame_count"`
	TrimmedByteCount   int64  `json:"trimmed_byte_count"`
	RetainedFromSeqMin uint64 `json:"retained_from_sequence"`
}

// Record is one authenticated frame.
type Record struct {
	Kind     Kind
	Epoch    uint64
	Sequence uint64
	// Ops is populated for KindOperations and empty otherwise.
	Ops []Op
	// Epoch header and trim anchor payloads, populated for their kinds.
	Header EpochHeader
	Trim   TrimAnchor
	// Hash is this frame's own hash, which the next frame chains to.
	Hash [32]byte
	// Offset is where the frame starts in the file, so a caller can name the
	// byte a problem is at.
	Offset int64
}

// encodeFrame builds one authenticated frame and returns it with its hash.
func encodeFrame(key []byte, kind Kind, epoch, sequence uint64, previous [32]byte, payload []byte) ([]byte, [32]byte) {
	frame := make([]byte, frameHeaderSize+len(payload)+frameMACSize)
	copy(frame[:4], frameMagic)
	frame[4] = frameVersion
	frame[5] = byte(kind)
	binary.BigEndian.PutUint64(frame[8:16], epoch)
	binary.BigEndian.PutUint64(frame[16:24], sequence)
	binary.BigEndian.PutUint32(frame[24:28], uint32(len(payload)))
	copy(frame[28:60], previous[:])
	copy(frame[frameHeaderSize:], payload)
	mac := hmac.New(sha256.New, key)
	mac.Write(frame[:frameHeaderSize+len(payload)])
	copy(frame[frameHeaderSize+len(payload):], mac.Sum(nil))
	return frame, sha256.Sum256(frame)
}

func encodePayload(kind Kind, ops []Op, header EpochHeader, anchor TrimAnchor) ([]byte, error) {
	switch kind {
	case KindOperations:
		if len(ops) == 0 {
			return nil, errors.New("an operations frame with no operations would record nothing")
		}
		for _, op := range ops {
			if err := op.validate(); err != nil {
				return nil, err
			}
		}
		return json.Marshal(ops)
	case KindEpoch:
		return json.Marshal(header)
	case KindTrim:
		return json.Marshal(anchor)
	}
	return nil, fmt.Errorf("unknown frame kind %d", kind)
}

func decodePayload(kind Kind, payload []byte, record *Record) error {
	switch kind {
	case KindOperations:
		var ops []Op
		if err := json.Unmarshal(payload, &ops); err != nil {
			return err
		}
		if len(ops) == 0 {
			return errors.New("operations frame carries no operations")
		}
		for _, op := range ops {
			if err := op.validate(); err != nil {
				return err
			}
		}
		record.Ops = ops
	case KindEpoch:
		return json.Unmarshal(payload, &record.Header)
	case KindTrim:
		return json.Unmarshal(payload, &record.Trim)
	}
	return nil
}
