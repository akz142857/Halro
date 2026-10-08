package metadatajournal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

type cancelingScanReader struct {
	*bytes.Reader
	cancel  context.CancelFunc
	after   int
	reads   int
	maxRead int
}

func (r *cancelingScanReader) Read(b []byte) (int, error) {
	r.reads++
	r.maxRead = max(r.maxRead, len(b))
	n, err := r.Reader.Read(b)
	if r.reads == r.after {
		r.cancel()
	}
	return n, err
}

func TestMetadataScanContextCancelDuringWideFrameDoesNotPublishAuthenticatedHead(t *testing.T) {
	path, log := newJournal(t)
	if _, err := log.Append([]Op{put("projects", "wide", string(bytes.Repeat([]byte("x"), 1<<20)))}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &cancelingScanReader{Reader: bytes.NewReader(raw), cancel: cancel, after: 4}
	head, partial, err := scanContext(ctx, reader, testKey(), nil)
	if !errors.Is(err, context.Canceled) || partial || head != (Head{}) {
		t.Fatal(head, partial, err)
	}
	if reader.reads != 4 || reader.maxRead > scanReadBytes {
		t.Fatal(reader.reads, reader.maxRead)
	}
	// The same input still fully authenticates; cancellation is not corruption
	// and must not authorize partial-tail repair.
	complete, partial, err := scanContext(context.Background(), bytes.NewReader(raw), testKey(), nil)
	if err != nil || partial || complete != log.Head() {
		t.Fatal(complete, partial, err)
	}
}

type blockedScanReader struct {
	*bytes.Reader
	entered, release chan struct{}
	blocked          bool
}

func (r *blockedScanReader) Read(b []byte) (int, error) {
	if !r.blocked {
		r.blocked = true
		close(r.entered)
		<-r.release
	}
	return r.Reader.Read(b)
}

func TestMetadataScanCancelWaitsForActualBlockingReadAndStopsNextRead(t *testing.T) {
	path, log := newJournal(t)
	if _, err := log.Append([]Op{put("projects", "p", "v")}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader := &blockedScanReader{Reader: bytes.NewReader(raw), entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := scanContext(ctx, reader, testKey(), nil); done <- err }()
	<-reader.entered
	cancel()
	select {
	case err := <-done:
		close(reader.release)
		t.Fatal("returned before Read exited", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(reader.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if reader.Reader.Len() != len(raw)-frameHeaderSize {
		t.Fatal("read continued after cancel")
	}
}

func TestMetadataScanReusableBytesDoNotAliasRetainedDecodedRecords(t *testing.T) {
	path, log := newJournal(t)
	values := []string{"first", string(bytes.Repeat([]byte("w"), 100000)), "last"}
	for _, value := range values {
		if _, err := log.Append([]Op{put("projects", value[:min(len(value), 5)], value)}); err != nil {
			t.Fatal(err)
		}
	}
	var records []Record
	head, err := ReplayContext(context.Background(), path, testKey(), func(r Record) error {
		if r.Kind == KindOperations {
			records = append(records, r)
		}
		return nil
	})
	if err != nil || head != log.Head() || len(records) != len(values) {
		t.Fatal(head, err)
	}
	for i, r := range records {
		if string(r.Ops[0].Value) != values[i] || string(r.Ops[0].Key) != values[i][:min(len(values[i]), 5)] {
			t.Fatal("reused tail changed decoded record", i)
		}
	}
}

func TestMetadataScanCanceledOpenPreservesPartialTailAndRangeCallsNoVisitor(t *testing.T) {
	path, log := newJournal(t)
	if _, err := log.Append([]Op{put("projects", "p", "v")}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	visited := false
	if err := log.ReplayRangeValidatedContext(ctx, 0, 1, func(Record) error { visited = true; return nil }); !errors.Is(err, context.Canceled) || visited {
		t.Fatal(err, visited)
	}
	if _, err := ReplayContext(ctx, path, testKey(), nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := VerifyContext(ctx, path, testKey()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw[:len(raw)-1], 0600); err != nil {
		t.Fatal(err)
	}
	for _, open := range []func(context.Context, string, []byte) (*Log, error){OpenContext, OpenReplicaContext} {
		if l, err := open(ctx, path, testKey()); !errors.Is(err, context.Canceled) || l != nil {
			t.Fatal(l, err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, raw[:len(raw)-1]) {
		t.Fatal("cancellation repaired torn tail", err)
	}
}

type shortScanReader struct{ data []byte }

func (r *shortScanReader) Read(dst []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	dst[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}
func TestMetadataReadContextPreservesShortReadEOFContracts(t *testing.T) {
	for _, n := range []int{0, 1, 5, 10} {
		r := &shortScanReader{data: bytes.Repeat([]byte("a"), n)}
		dst := make([]byte, 5)
		got, err := readFullContext(context.Background(), r, dst)
		if n == 0 {
			if got != 0 || err != io.EOF {
				t.Fatal(got, err)
			}
		} else if n < 5 {
			if got != n || err != io.ErrUnexpectedEOF {
				t.Fatal(got, err)
			}
		} else if got != 5 || err != nil {
			t.Fatal(got, err)
		}
	}
}

func TestMetadataScanMaximumPayloadAndFollowingFrameKeepExactChain(t *testing.T) {
	epochPayload, err := encodePayload(KindEpoch, nil, EpochHeader{Epoch: 1, Reason: "maximum-frame"}, TrimAnchor{})
	if err != nil {
		t.Fatal(err)
	}
	epoch, hash := encodeFrame(testKey(), KindEpoch, 1, 0, [32]byte{}, epochPayload)
	wideValue := bytes.Repeat([]byte("v"), (MaxPayloadSize-1024)*3/4)
	widePayload, err := encodePayload(KindOperations, []Op{{Kind: OpPut, Path: []string{"projects"}, Key: []byte("wide"), Value: wideValue}}, EpochHeader{}, TrimAnchor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(widePayload) > MaxPayloadSize {
		t.Fatal(len(widePayload))
	}
	// JSON permits trailing whitespace; this constructs an authenticated exact
	// native ceiling frame instead of reducing the ceiling to a nearby size.
	widePayload = append(widePayload, bytes.Repeat([]byte(" "), MaxPayloadSize-len(widePayload))...)
	wide, hash := encodeFrame(testKey(), KindOperations, 1, 1, hash, widePayload)
	smallPayload, err := encodePayload(KindOperations, []Op{put("projects", "last", "small")}, EpochHeader{}, TrimAnchor{})
	if err != nil {
		t.Fatal(err)
	}
	small, hash := encodeFrame(testKey(), KindOperations, 1, 2, hash, smallPayload)
	source := bytes.Join([][]byte{epoch, wide, small}, nil)
	var retained []Record
	head, partial, err := scanContext(context.Background(), bytes.NewReader(source), testKey(), func(r Record) error {
		if r.Kind == KindOperations {
			retained = append(retained, r)
		}
		return nil
	})
	if err != nil || partial || head.Hash != hash || head.Sequence != 2 || head.Offset != int64(len(source)) {
		t.Fatal(head, partial, err)
	}
	if len(retained) != 2 || !bytes.Equal(retained[0].Ops[0].Value, wideValue) || string(retained[1].Ops[0].Value) != "small" {
		t.Fatal("maximum frame/retained bytes changed")
	}
}
