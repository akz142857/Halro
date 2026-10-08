package metadatajournal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPreparedAppendClonesInputAndClosesEscapedBarrier(t *testing.T) {
	path, l, _ := newBoundJournal(t)
	ops := []Op{put("projects", "p", "original")}
	var escaped func() error
	record, err := l.AppendPreparedContext(context.Background(), ops, func(record Record, durable func() error) error {
		escaped = durable
		ops[0].Value[0] = 'X'
		if string(record.Ops[0].Value) != "original" {
			return errors.New("input alias")
		}
		return durable()
	})
	if err != nil || record.Sequence != 1 || string(record.Ops[0].Value) != "original" {
		t.Fatal(record, err)
	}
	before, _ := os.ReadFile(path)
	if err := escaped(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatal("escaped barrier active", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || l.Head().Sequence != 1 {
		t.Fatal("escaped barrier mutated source")
	}
}

func TestPreparedAppendPreparationFailuresNeverAdvanceNative(t *testing.T) {
	for _, mode := range []string{"error", "missing-barrier", "mutated-facts", "cancel", "disk", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			path, l, _ := newBoundJournal(t)
			before, _ := os.ReadFile(path)
			old := l.Head()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			callback := false
			if mode == "disk" {
				l.maxBytes = l.offset + 1
			}
			if mode == "foreign" {
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			record, err := l.AppendPreparedContext(ctx, []Op{put("projects", "p", "v")}, func(record Record, durable func() error) error {
				callback = true
				switch mode {
				case "error":
					return errors.New("projection rejected")
				case "missing-barrier":
					return nil
				case "mutated-facts":
					record.Ops[0].Value = []byte("changed")
				case "cancel":
					cancel()
				}
				return durable()
			})
			if err == nil || record.Hash != ([32]byte{}) || l.Head() != old {
				t.Fatal(record, err)
			}
			if (mode == "disk" || mode == "foreign") && callback {
				t.Fatal("callback entered after source refusal")
			}
			if mode == "foreign" {
				actual, _ := os.ReadFile(path)
				if string(actual) != "foreign" {
					t.Fatal("foreign data overwritten")
				}
				actual, _ = os.ReadFile(path + ".original")
				if !bytes.Equal(actual, before) {
					t.Fatal("withdrawn descriptor appended")
				}
			} else {
				actual, _ := os.ReadFile(path)
				if !bytes.Equal(actual, before) {
					t.Fatal("preparation failure appended bytes")
				}
			}
		})
	}
}

func TestPreparedAppendDurableThenPublicationFailureRetainsHeadAndRefusesAppend(t *testing.T) {
	for _, mode := range []string{"error", "panic", "repeat", "late-mutation"} {
		t.Run(mode, func(t *testing.T) {
			_, l, _ := newBoundJournal(t)
			var record Record
			var err error
			func() {
				defer func() {
					if mode == "panic" && recover() == nil {
						t.Error("panic lost")
					}
				}()
				record, err = l.AppendPreparedContext(context.Background(), []Op{put("projects", "p", "v")}, func(record Record, durable func() error) error {
					if err := durable(); err != nil {
						return err
					}
					switch mode {
					case "error":
						return errors.New("root publication failed")
					case "panic":
						panic("publication panic")
					case "repeat":
						_ = durable()
					case "late-mutation":
						record.Ops[0].Value = []byte("changed")
					}
					return nil
				})
			}()
			if mode != "panic" && err == nil {
				t.Fatal("uncertainty accepted")
			}
			if record.Hash != ([32]byte{}) || l.Head().Sequence != 1 {
				t.Fatal("actual durable head lost", record, l.Head())
			}
			if _, err := l.Append([]Op{put("projects", "next", "v")}); err == nil {
				t.Fatal("publication uncertainty continued")
			}
		})
	}
}

type blockedPreparedSync struct {
	file             *os.File
	entered, release chan struct{}
}

func (d *blockedPreparedSync) Write(raw []byte) (int, error) { return d.file.Write(raw) }
func (d *blockedPreparedSync) Sync() error                   { close(d.entered); <-d.release; return d.file.Sync() }

func TestPreparedAppendCallbackReturnAndCloseJoinActualDurableWork(t *testing.T) {
	_, l, _ := newBoundJournal(t)
	entered, release := make(chan struct{}), make(chan struct{})
	l.durability = &blockedPreparedSync{l.file, entered, release}
	callbackReturned := make(chan struct{})
	done := make(chan error, 1)
	barrierDone := make(chan error, 1)
	go func() {
		_, err := l.AppendPreparedContext(context.Background(), []Op{put("projects", "p", "v")}, func(_ Record, durable func() error) error {
			go func() { barrierDone <- durable() }()
			<-entered
			close(callbackReturned)
			return nil
		})
		done <- err
	}()
	select {
	case <-callbackReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not reach actual Sync")
	}
	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()
	select {
	case err := <-done:
		t.Fatal("callback return released source during IO", err)
	case err := <-closed:
		t.Fatal("Close returned during IO", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-barrierDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if l.Head().Sequence != 1 {
		t.Fatal(l.Head())
	}
}

func TestPreparedAppendIOFailureWithdrawsWriter(t *testing.T) {
	for _, mode := range []string{"write", "sync"} {
		t.Run(mode, func(t *testing.T) {
			_, l, _ := newBoundJournal(t)
			d := &failingDurability{file: l.file}
			if mode == "write" {
				d.writeErr = errors.New("write failed")
			} else {
				d.syncErr = errors.New("sync failed")
			}
			l.durability = d
			if _, err := l.AppendPreparedContext(context.Background(), []Op{put("projects", "p", "v")}, func(_ Record, durable func() error) error { return durable() }); err == nil {
				t.Fatal("IO failure accepted")
			}
			if _, err := l.Append([]Op{put("projects", "next", "v")}); err == nil {
				t.Fatal("IO failure continued")
			}
		})
	}
}
