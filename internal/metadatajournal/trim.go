package metadatajournal

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Trim drops the frames at or below throughSequence and rewrites the file so
// it opens with an anchor recording the cut.
//
// Without this the journal is a file that grows on every Admin write and never
// shrinks — and under Standalone every Attempt with a Deployment writes two
// price-pin frames, so it grows with traffic rather than with administration.
// The cut point is the caller's: in Standalone it is the projection itself,
// because bbolt already holds everything the dropped frames described; under HA
// it is bounded by the slowest member still eligible for incremental catch-up
// (§11.2).
//
// The anchor is what keeps a trimmed file distinguishable from a mutilated one.
// It is MAC'd like every other frame, so a reader that finds a chain starting
// mid-stream has a signed statement of where it started and what came before —
// rather than having to accept any short file as legitimate, which is the
// failure mode a trim without an anchor creates.
func Trim(path string, key []byte, throughSequence uint64) (Head, error) {
	if len(key) != KeySize {
		return Head{}, fmt.Errorf("metadata journal key must be %d bytes", KeySize)
	}
	file, err := os.Open(path)
	if err != nil {
		return Head{}, err
	}
	var (
		epoch           uint64
		chainHeadAtTrim [32]byte
		trimmedFrames   uint64
		trimmedBytes    int64
		retained        []byte
		retainedFrom    uint64
		existingAnchor  TrimAnchor
		sawAnchor       bool
	)
	head, err := Replay(path, key, func(record Record) error {
		switch {
		case record.Kind == KindEpoch:
			epoch = record.Epoch
			trimmedBytes += frameSize(record, file)
			return nil
		case record.Kind == KindTrim:
			epoch, sawAnchor, existingAnchor = record.Epoch, true, record.Trim
			copy(chainHeadAtTrim[:], record.Trim.ChainHeadAtTrim)
			trimmedFrames = record.Trim.TrimmedFrameCount
			trimmedBytes += record.Trim.TrimmedByteCount + frameSize(record, file)
			return nil
		}
		size := frameSize(record, file)
		if record.Sequence <= throughSequence {
			trimmedFrames++
			trimmedBytes += size
			chainHeadAtTrim = record.Hash
			return nil
		}
		if retained == nil {
			retainedFrom = record.Sequence
		}
		chunk, readErr := readAt(file, record.Offset, size)
		if readErr != nil {
			return readErr
		}
		retained = append(retained, chunk...)
		return nil
	})
	file.Close()
	if err != nil {
		return Head{}, err
	}
	if sawAnchor && throughSequence < existingAnchor.TrimmedThrough {
		return Head{}, fmt.Errorf("metadata journal was already trimmed through %d, past the requested %d",
			existingAnchor.TrimmedThrough, throughSequence)
	}
	if throughSequence > head.Sequence {
		return Head{}, fmt.Errorf("cannot trim through %d: the journal ends at %d", throughSequence, head.Sequence)
	}
	if sawAnchor && throughSequence == existingAnchor.TrimmedThrough {
		// Nothing to do, and rewriting the file anyway would replace a durable
		// artefact to achieve no change.
		return head, nil
	}
	if retainedFrom == 0 {
		retainedFrom = throughSequence + 1
	}
	anchor := TrimAnchor{
		Epoch: epoch, TrimmedThrough: throughSequence, ChainHeadAtTrim: chainHeadAtTrim[:],
		TrimmedFrameCount: trimmedFrames, TrimmedByteCount: trimmedBytes,
		RetainedFromSeqMin: retainedFrom,
	}
	payload, err := encodePayload(KindTrim, nil, EpochHeader{}, anchor)
	if err != nil {
		return Head{}, err
	}
	frame, _ := encodeFrame(key, KindTrim, epoch, throughSequence, [32]byte{}, payload)
	if err := publish(path, append(frame, retained...)); err != nil {
		return Head{}, err
	}
	return Verify(path, key)
}

// frameSize recovers a frame's byte length from what the record carries. The
// scanner does not hand one out, and the alternative — re-encoding the payload
// to measure it — would not be the bytes on disk if the encoder ever changed.
func frameSize(record Record, file *os.File) int64 {
	header := make([]byte, frameHeaderSize)
	if _, err := file.ReadAt(header, record.Offset); err != nil {
		return 0
	}
	length := int64(uint32(header[24])<<24 | uint32(header[25])<<16 | uint32(header[26])<<8 | uint32(header[27]))
	return frameHeaderSize + length + frameMACSize
}

func readAt(file *os.File, offset, size int64) ([]byte, error) {
	if size <= 0 {
		return nil, errors.New("cannot read a frame of no size")
	}
	chunk := make([]byte, size)
	if _, err := file.ReadAt(chunk, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return chunk, nil
}
