package bolt

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/metadatajournal"
)

func openReplicaBlockFixture(t *testing.T, path string) *Store {
	t.Helper()
	s, err := OpenReplica(path)
	if err != nil {
		t.Fatal(err)
	}
	log, err := metadatajournal.OpenReplica(s.MetadataJournalPath(), testJournalKey())
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	if _, err := s.AttachReplicaMetadataJournal(log, testJournalKey()); err != nil {
		log.Close()
		s.Close()
		t.Fatal(err)
	}
	return s
}

func TestReplicaMetadataBlocksResumeCommittedPrefixAndExcludeUnconfirmedTail(t *testing.T) {
	path, base, head := metadataAtomicTailFixture(t, 130, false)
	s := openReplicaBlockFixture(t, path)
	stop := errors.New("stop after64")
	applied, err := s.applyReplicaMetadataThrough(context.Background(), head.Epoch, base+100, func(n uint64) error {
		if n == base+64 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || applied != base+64 {
		t.Fatal(applied, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openReplicaBlockFixture(t, path)
	defer s.Close()
	if applied, err = s.ApplyReplicaMetadataThrough(context.Background(), head.Epoch, base+100); err != nil || applied != base+100 {
		t.Fatal(applied, err)
	}
	if _, err := s.ApplyReplicaMetadataThrough(context.Background(), head.Epoch, base+64); err == nil {
		t.Fatal("accepted regressed target")
	}
	if err := s.view(func(tx *Tx) error {
		b := tx.Bucket(bucketProjects)
		if b.Get([]byte("recovery_099")) == nil || b.Get([]byte("recovery_100")) != nil {
			t.Fatal("wrong confirmed cut")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReplicaMetadataBlocksBadFrameRollsBackAllOpsAndReturnsCommittedPosition(t *testing.T) {
	path, base, head := metadataAtomicTailFixture(t, 2, true)
	s := openReplicaBlockFixture(t, path)
	defer s.Close()
	for attempt := 0; attempt < 2; attempt++ {
		n, err := s.ApplyReplicaMetadataThrough(context.Background(), head.Epoch, head.Sequence)
		if err == nil || n != base+2 {
			t.Fatal(n, err)
		}
		if err := s.view(func(tx *Tx) error {
			if tx.Bucket(bucketProjects).Get([]byte("failed_frame_first_op")) != nil {
				t.Fatal("partial native frame committed")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReplicaMetadataBlocksAuthenticateEntireDurableSuffixBeforeAnyCommit(t *testing.T) {
	for _, mode := range []string{"bad-mac", "partial"} {
		t.Run(mode, func(t *testing.T) {
			path, base, head := metadataAtomicTailFixture(t, 3, false)
			s := openReplicaBlockFixture(t, path)
			defer s.Close()
			f, err := os.OpenFile(s.MetadataJournalPath(), os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				t.Fatal(err)
			}
			if mode == "partial" {
				err = f.Truncate(info.Size() - 1)
			} else {
				var b [1]byte
				if _, err = f.ReadAt(b[:], info.Size()-1); err == nil {
					b[0] ^= 1
					_, err = f.WriteAt(b[:], info.Size()-1)
				}
			}
			if err == nil {
				err = f.Sync()
			}
			err = errors.Join(err, f.Close())
			if err != nil {
				t.Fatal(err)
			}
			// Damage is beyond the requested confirmed prefix. Both a nonempty apply
			// and a same-position retry must authenticate it before exposing success.
			for _, target := range []uint64{base + 2, base} {
				n, err := s.ApplyReplicaMetadataThrough(context.Background(), head.Epoch, target)
				if err == nil || n != base {
					t.Fatal(n, err)
				}
			}
			pos, err := s.journalPosition()
			if err != nil || pos.sequence != base {
				t.Fatal(pos, err)
			}
			if err := s.view(func(tx *Tx) error {
				if tx.Bucket(bucketProjects).Get([]byte("recovery_000")) != nil {
					t.Fatal("committed before full authentication")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReplicaMetadataBlocksCancellationKeepsCommittedPrefixAndCloseJoinsCallback(t *testing.T) {
	path, base, head := metadataAtomicTailFixture(t, 3, false)
	s := openReplicaBlockFixture(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	n, err := s.applyReplicaMetadataThrough(ctx, head.Epoch, head.Sequence, func(uint64) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) || n != base+1 {
		t.Fatal(n, err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.applyReplicaMetadataThrough(context.Background(), head.Epoch, head.Sequence, func(uint64) error { close(entered); <-release; return nil })
		done <- err
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	<-s.lifetime.ctx.Done()
	select {
	case err := <-closed:
		close(release)
		t.Fatal("closed before callback exit", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	s = openReplicaBlockFixture(t, path)
	defer s.Close()
	pos, err := s.journalPosition()
	if err != nil || pos.sequence != base+2 {
		t.Fatal(pos, err)
	}
	if n, err := s.ApplyReplicaMetadataThrough(context.Background(), head.Epoch, head.Sequence); err != nil || n != head.Sequence {
		t.Fatal(n, err)
	}
}

func TestReplicaMetadataBlocksSerializeApplyJobs(t *testing.T) {
	path, _, head := metadataAtomicTailFixture(t, 130, false)
	s := openReplicaBlockFixture(t, path)
	defer s.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		once := false
		_, err := s.applyReplicaMetadataThrough(context.Background(), head.Epoch, head.Sequence-66, func(uint64) error {
			if !once {
				once = true
				close(entered)
				<-release
			}
			return nil
		})
		first <- err
	}()
	<-entered
	go func() {
		_, err := s.ApplyReplicaMetadataThrough(context.Background(), head.Epoch, head.Sequence)
		second <- err
	}()
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	pos, err := s.journalPosition()
	if err != nil || pos.sequence != head.Sequence {
		t.Fatal(pos, err)
	}
}
