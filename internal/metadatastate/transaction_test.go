package metadatastate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/authenticatedstate"
)

func TestMetadataTransactionReadsOwnWritesAndCommitsSingleNativeFrame(t *testing.T) {
	b, o, path := linkedDeltaCandidate(t)
	before := o.Budget.Snapshot().Used
	var escaped *Transaction
	input := []byte("first")
	r, err := b.Update(context.Background(), func(tx *Transaction) error {
		escaped = tx
		if err := tx.Put([]string{"projects"}, []byte("project"), input); err != nil {
			return err
		}
		input[0] = 'X'
		v, found, err := tx.Get([]string{"projects"}, []byte("project"))
		if err != nil || !found || string(v) != "first" {
			return fmt.Errorf("read own write: %q %v %v", v, found, err)
		}
		v[0] = 'Y'
		if err := tx.CreateBucket([]string{"deployment_price_timeline", "new"}); err != nil {
			return err
		}
		if err := tx.Put([]string{"deployment_price_timeline", "new"}, []byte("price"), []byte("123")); err != nil {
			return err
		}
		if err := tx.Delete([]string{"projects"}, []byte("project")); err != nil {
			return err
		}
		if _, found, err := tx.Get([]string{"projects"}, []byte("project")); err != nil || found {
			return fmt.Errorf("deleted staged row: %v %v", found, err)
		}
		return tx.Put([]string{"projects"}, []byte("project"), []byte("final"))
	})
	if err != nil || r.Sequence != 1 || len(r.Ops) != 5 {
		t.Fatal(r, err)
	}
	if err := b.View(context.Background(), func(tx *Transaction) error {
		v, found, err := tx.Get([]string{"projects"}, []byte("project"))
		if err != nil || !found || string(v) != "final" {
			return fmt.Errorf("published value: %q %v %v", v, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := escaped.Get([]string{"projects"}, []byte("project")); !errors.Is(err, authenticatedstate.ErrClosed) {
		t.Fatal("escaped transaction read", err)
	}
	if err := escaped.Put([]string{"projects"}, []byte("late"), []byte("bad")); !errors.Is(err, authenticatedstate.ErrClosed) {
		t.Fatal("escaped transaction write", err)
	}
	if o.Budget.Snapshot().Used != before {
		t.Fatal("transaction leaked fees", before, o.Budget.Snapshot())
	}
	if info, err := os.Stat(path); err != nil || info.Size() <= 0 {
		t.Fatal(info, err)
	}
}

func TestMetadataTransactionRefusalOrIgnoredErrorCannotAppend(t *testing.T) {
	for _, shape := range []string{"callback", "ignored forbidden", "operation limit", "read-only write", "cancel", "bucket collision"} {
		t.Run(shape, func(t *testing.T) {
			b, o, path := linkedDeltaCandidate(t)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			root, err := b.store.RootIdentity(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			fee := o.Budget.Snapshot().Used
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			callback := func(tx *Transaction) error {
				if err := tx.Put([]string{"projects"}, []byte("candidate"), []byte("value")); err != nil {
					return err
				}
				switch shape {
				case "callback":
					return errors.New("business conflict")
				case "ignored forbidden":
					_ = tx.Put([]string{"meta"}, []byte("schema_version"), []byte("future"))
				case "operation limit":
					for i := 0; i < 1024; i++ {
						_ = tx.Put([]string{"projects"}, []byte(fmt.Sprintf("k%d", i)), []byte("v"))
					}
				case "cancel":
					cancel()
				case "bucket collision":
					_ = tx.Put([]string{"deployment_price_timeline"}, []byte("p\x00/"), []byte("bad"))
				}
				return nil
			}
			if shape == "read-only write" {
				err = b.View(ctx, callback)
			} else {
				_, err = b.Update(ctx, callback)
			}
			if err == nil {
				t.Fatal("transaction refusal ignored")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("candidate appended", err)
			}
			got, err := b.store.RootIdentity(context.Background())
			if err != nil || got != root || !b.ready.Load() || fee != o.Budget.Snapshot().Used {
				t.Fatal("refusal changed healthy root or fee", got, root, err, o.Budget.Snapshot())
			}
		})
	}
}

func TestMetadataTransactionCloseCancelsAndJoinsWholeCallback(t *testing.T) {
	b, o, _ := linkedDeltaCandidate(t)
	before := o.Budget.Snapshot().Used
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := b.Update(context.Background(), func(tx *Transaction) error {
			if err := tx.Put([]string{"projects"}, []byte("uncommitted"), []byte("value")); err != nil {
				return err
			}
			close(entered)
			<-tx.Context().Done()
			<-release
			return nil
		})
		finished <- err
	}()
	<-entered
	closing := make(chan error, 1)
	go func() { closing <- b.Close() }()
	select {
	case err := <-closing:
		t.Fatal("Close crossed running callback", err)
	case <-time.After(30 * time.Millisecond):
	}
	if o.Budget.Snapshot().Used.RAM < before.RAM+TransactionRAMBytes {
		t.Fatal("active callback fee refunded", o.Budget.Snapshot())
	}
	close(release)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closing; err != nil {
		t.Fatal(err)
	}
	if b.position.Sequence != 0 {
		t.Fatal("Close published staged operation", b.position)
	}
}

func TestMetadataTransactionEmptyCallbackDoesNotCreateNativeFact(t *testing.T) {
	b, _, _ := linkedDeltaCandidate(t)
	root, err := b.store.RootIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, err := b.Update(context.Background(), func(tx *Transaction) error {
		return tx.CreateBucket([]string{"projects"})
	})
	if err != nil || r.Sequence != 0 || len(r.Ops) != 0 {
		t.Fatal(r, err)
	}
	got, err := b.store.RootIdentity(context.Background())
	if err != nil || got != root || b.position.Sequence != 0 {
		t.Fatal(got, root, err)
	}
}

func TestMetadataTransactionSeekSkipsNestedHistoryAndMergesOwnWrites(t *testing.T) {
	b, _, _ := linkedDeltaCandidate(t)
	_, err := b.Update(context.Background(), func(tx *Transaction) error {
		parent := []string{"deployment_price_timeline"}
		if err := tx.CreateBucket([]string{"deployment_price_timeline", "new"}); err != nil {
			return err
		}
		if err := tx.Put(parent, []byte("middle"), []byte("value")); err != nil {
			return err
		}
		for _, query := range []struct {
			key                string
			reverse, exclusive bool
			want               string
			bucket             bool
		}{
			{"", false, false, "empty", true},
			{"empty", false, true, "middle", false},
			{"middle", false, false, "middle", false},
			{"middle", false, true, "new", true},
			{"new", false, false, "new", true},
			{"new", false, true, "p\x00/", true},
			{"p\x00/", false, true, "", false},
			{"", true, false, "p\x00/", true},
			{"p\x00/", true, true, "new", true},
			{"new", true, false, "new", true},
			{"new", true, true, "middle", false},
			{"middle", true, false, "middle", false},
			{"middle", true, true, "empty", true},
			{"empty", true, true, "", false},
		} {
			entry, found, err := tx.Seek(parent, []byte(query.key), query.reverse, query.exclusive)
			if err != nil || found != (query.want != "") || string(entry.Key) != query.want || found && entry.Bucket != query.bucket {
				return fmt.Errorf("query=%+v entry=%+v found=%v err=%v", query, entry, found, err)
			}
		}
		if err := tx.DeleteBucket([]string{"deployment_price_timeline", "p\x00/"}); err != nil {
			return err
		}
		entry, found, err := tx.Seek(parent, nil, true, false)
		if err != nil || !found || string(entry.Key) != "new" {
			return fmt.Errorf("deleted subtree seek: %+v %v %v", entry, found, err)
		}
		if err := tx.Delete(parent, []byte("middle")); err != nil {
			return err
		}
		entry, found, err = tx.Seek(parent, []byte("new"), true, true)
		if err != nil || !found || string(entry.Key) != "empty" {
			return fmt.Errorf("deleted value seek: %+v %v %v", entry, found, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
