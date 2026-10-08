package governance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestGovernanceVerifyHeadRequiresActualWriterFileAndFrozenBoundary(t *testing.T) {
	for _, shape := range []string{"unchanged", "replacement-identical-bytes", "deleted", "truncated", "grown", "stale-head", "cancelled", "closed"} {
		t.Run(shape, func(t *testing.T) {
			_, log, _, _, _ := testManager(t)
			if _, err := log.Append(context.Background(), projectionEvent(1)); err != nil {
				t.Fatal(err)
			}
			expected := log.Summary()
			ctx := context.Background()
			original, err := os.ReadFile(log.path)
			if err != nil {
				t.Fatal(err)
			}
			switch shape {
			case "replacement-identical-bytes":
				if err := os.Rename(log.path, log.path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(log.path, original, 0600); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := os.Remove(log.path); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				if err := os.Truncate(log.path, 0); err != nil {
					t.Fatal(err)
				}
			case "grown":
				f, err := os.OpenFile(log.path, os.O_WRONLY|os.O_APPEND, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.Write([]byte("HG"))
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
			case "stale-head":
				expected.Records++
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "closed":
				if err := log.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before, readErr := os.ReadFile(log.path)
			err = log.VerifyHead(ctx, expected)
			if shape == "unchanged" && err != nil {
				t.Fatal(err)
			}
			if shape != "unchanged" && err == nil {
				t.Fatal("unqualified native file accepted")
			}
			if shape == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if shape != "unchanged" && shape != "cancelled" && shape != "closed" && !errors.Is(err, ErrCorrupt) {
				t.Fatal(err)
			}
			after, afterErr := os.ReadFile(log.path)
			if (readErr == nil) != (afterErr == nil) || !bytes.Equal(before, after) {
				t.Fatal("verification altered native file", afterErr)
			}
		})
	}
}

func TestGovernanceAppendRejectsDetachedNativeWriterBeforeCachedOrNewFacts(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprint(retry), func(t *testing.T) {
			_, log, _, _, _ := testManager(t)
			if _, err := log.Append(context.Background(), projectionEvent(1)); err != nil {
				t.Fatal(err)
			}
			expected := log.Summary()
			original, err := os.ReadFile(log.path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(log.path, log.path+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(log.path, original, 0600); err != nil {
				t.Fatal(err)
			}
			event := projectionEvent(2)
			if retry {
				event = projectionEvent(1)
			}
			receipt, err := log.Append(context.Background(), event)
			if !errors.Is(err, ErrCorrupt) || receipt.Sequence != 0 || receipt.Hash != ([32]byte{}) || receipt.Event.EventID != "" || log.Summary() != expected {
				t.Fatal(receipt, err)
			}
			// Restoring the named file cannot clear the terminal failure in this owner.
			if err := os.Remove(log.path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(log.path+".old", log.path); err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(context.Background(), projectionEvent(2)); !errors.Is(err, ErrCorrupt) {
				t.Fatal("native integrity fault was reset", err)
			}
			got, err := os.ReadFile(log.path)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatal("failed append changed native bytes", err)
			}
		})
	}
}

type nativeIdentityDurability struct {
	io.Writer
	sync func() error
}

func (w nativeIdentityDurability) Sync() error { return w.sync() }

func TestGovernanceAppendRechecksActualFileAfterSyncAndOrderingCallback(t *testing.T) {
	for _, phase := range []string{"sync", "after-durable"} {
		t.Run(phase, func(t *testing.T) {
			_, log, _, _, _ := testManager(t)
			moved := false
			replace := func() {
				raw, err := os.ReadFile(log.path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(log.path, log.path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(log.path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				moved = true
			}
			callbacks := 0
			log.afterDurable = func(DurableBatch) error {
				callbacks++
				if phase == "after-durable" {
					replace()
				}
				return nil
			}
			if phase == "sync" {
				log.durability = nativeIdentityDurability{Writer: log.file, sync: func() error {
					err := log.file.Sync()
					if err == nil {
						replace()
					}
					return err
				}}
			}
			receipt, err := log.Append(context.Background(), projectionEvent(1))
			if !moved || !errors.Is(err, ErrCorrupt) || receipt.Sequence != 0 || receipt.Hash != ([32]byte{}) || receipt.Event.EventID != "" {
				t.Fatal("detached durable writer acknowledged", receipt, err)
			}
			if phase == "sync" && callbacks != 0 || phase == "after-durable" && callbacks != 1 {
				t.Fatal("incorrect durability callback boundary", callbacks)
			}
			// Actual durable bytes survive failure; no rollback or manufactured success.
			old, err := os.ReadFile(log.path + ".old")
			if err != nil {
				t.Fatal(err)
			}
			named, err := os.ReadFile(log.path)
			if err != nil || !bytes.Equal(old, named) || len(named) == 0 {
				t.Fatal("durable evidence lost", err)
			}
			if log.Summary().Records != 1 || log.terminalErr == nil {
				t.Fatal("uncertain writer was not terminal")
			}
		})
	}
}

func TestGovernanceReplicaAppendRejectsDetachedWriterForDuplicateAndSuffix(t *testing.T) {
	_, source, _, _, _ := testManager(t)
	first, err := source.Append(context.Background(), projectionEvent(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Append(context.Background(), projectionEvent(2)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(source.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprint(duplicate), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "replica.log")
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			replica, err := OpenWithOptions(path, source.key, Options{Replica: true})
			if err != nil {
				t.Fatal(err)
			}
			defer replica.Close()
			if err := replica.AppendReplicated(1, 1, raw[:first.Offset]); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			original := raw[:first.Offset]
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			if duplicate {
				err = replica.AppendReplicated(1, 1, original)
			} else {
				err = replica.AppendReplicated(2, 2, raw[first.Offset:])
			}
			if !errors.Is(err, ErrCorrupt) || replica.Summary().Records != 1 || replica.terminalErr == nil {
				t.Fatal("detached replica writer accepted", err)
			}
			for _, name := range []string{path, path + ".old"} {
				got, err := os.ReadFile(name)
				if err != nil || !bytes.Equal(got, original) {
					t.Fatal("replica refusal changed native bytes", name, err)
				}
			}
		})
	}
}

func TestGovernanceProjectedManagerRejectsReadsAndIdempotencyAfterNativeReplacement(t *testing.T) {
	for _, api := range []string{"availability", "current", "retry"} {
		t.Run(api, func(t *testing.T) {
			old, log, _, _, _ := testManager(t)
			projection := buildTestGovernanceProjection(t, log)
			manager := projectedTestManager(t, old, projection)
			input := reportInput("accepted", "", "first")
			first, _, err := manager.Report(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(log.path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(log.path, log.path+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(log.path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			switch api {
			case "availability":
				available, err := manager.Ready()
				if available || !errors.Is(err, ErrUnavailable) {
					t.Fatal("detached manager available", available, err)
				}
			case "current":
				got, err := manager.ReadOutcomePage(context.Background(), OutcomePageQuery{ProjectID: first.ProjectID, Rows: 50, Bytes: OutcomePageMaxBytes})
				if len(got.Items) != 0 || got.Head != (Summary{}) || !errors.Is(err, ErrUnavailable) {
					t.Fatal("unqualified native content used", got, err)
				}
			case "retry":
				got, replay, err := manager.Report(context.Background(), input)
				if got.ID != "" || replay || !errors.Is(err, ErrUnavailable) {
					t.Fatal("detached native fact acknowledged", got, replay, err)
				}
			}
			if manager.unavailable == nil || log.terminalErr == nil {
				t.Fatal("native fault was not persistent")
			}
			if err := os.Remove(log.path); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(log.path+".old", log.path); err != nil {
				t.Fatal(err)
			}
			if available, _ := manager.Ready(); available {
				t.Fatal("named-file restoration cleared fault")
			}
			after, err := os.ReadFile(log.path)
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatal("query/retry changed native facts", err)
			}
		})
	}
}
