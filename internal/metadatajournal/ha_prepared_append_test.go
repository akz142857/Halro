package metadatajournal

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestBoundPreparedAppendActualAdmissionOrderingAndPublication(t *testing.T) {
	path, l, a := boundReplicationFixture(t)
	var steps []string
	admission := &metadataAppendAdmissionFixture{}
	install := l.SetBoundReplicationAdmissionAndAfterDurable(a, func(b DurableBatch) (AppendAdmission, error) {
		steps = append(steps, "admission")
		if _, seq, err := BoundReplicationCursor(context.Background(), path, testKey(), a); err != nil || seq != 0 {
			t.Fatal(seq, err)
		}
		return admission, nil
	}, func(b DurableBatch) (uint64, error) {
		steps = append(steps, "ordering")
		if _, seq, err := BoundReplicationCursor(context.Background(), path, testKey(), a); err != nil || seq != 1 {
			t.Fatal(seq, err)
		}
		if b.Admission != admission {
			t.Fatal("actual loan not transferred")
		}
		return 7, admission.Release()
	}, func(_ context.Context, index uint64) error {
		steps = append(steps, "confirmed")
		if index != 7 || len(steps) != 5 {
			t.Fatal(index, steps)
		}
		return nil
	})
	if install != nil {
		t.Fatal(install)
	}
	invoked := false
	if _, err := l.AppendPreparedContext(context.Background(), []Op{put("projects", "wrong", "v")}, func(Record, func() error) error { invoked = true; return nil }); err == nil || invoked {
		t.Fatal("Standalone append admitted HA tenure", err)
	}
	wrong := a
	wrong.FileRevision++
	if _, err := l.AppendBoundPreparedContext(context.Background(), wrong, []Op{put("projects", "wrong", "v")}, func(Record, func() error) error { invoked = true; return nil }); err == nil || invoked {
		t.Fatal("foreign permission admitted", err)
	}
	receipt, err := l.AppendBoundPreparedContext(context.Background(), a, []Op{put("projects", "p", "v")}, func(_ Record, durable func() error) error {
		steps = append(steps, "prepared")
		if err := durable(); err != nil {
			return err
		}
		steps = append(steps, "published")
		return nil
	})
	if err != nil || receipt.Record.Sequence != 1 || receipt.ReplicationIndex != 7 {
		t.Fatal(receipt, err)
	}
	if err := l.WaitConfirmed(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(steps, []string{"prepared", "admission", "ordering", "published", "confirmed"}) || admission.releases != 1 {
		t.Fatal(steps, admission)
	}
}

func TestBoundPreparedAppendRefusalsAndUncertainAdmissionOwnership(t *testing.T) {
	for _, mode := range []string{"prepare", "admission", "cancel-after-admission", "foreign-after-admission", "admission-mutates-frames", "sync", "ordering", "zero-ordering", "ignored-ordering-error", "publication"} {
		t.Run(mode, func(t *testing.T) {
			path, l, a := boundReplicationFixture(t)
			before := l.Head()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			admission := &metadataAppendAdmissionFixture{}
			orderingOwned := false
			if mode == "sync" {
				l.durability = &failingDurability{file: l.file, syncErr: errors.New("actual sync failed")}
			}
			err := l.SetBoundReplicationAdmissionAndAfterDurable(a, func(batch DurableBatch) (AppendAdmission, error) {
				if mode == "admission-mutates-frames" {
					batch.Frames[0] ^= 1
				}
				if mode == "admission" {
					return nil, errors.New("window full")
				}
				if mode == "cancel-after-admission" {
					cancel()
				}
				if mode == "foreign-after-admission" {
					if err := os.Rename(path, path+".retained"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return admission, nil
			}, func(b DurableBatch) (uint64, error) {
				orderingOwned = b.Admission == admission
				if mode == "ordering" || mode == "ignored-ordering-error" {
					return 0, errors.New("actual ordering failed")
				}
				if mode == "zero-ordering" {
					return 0, nil
				}
				return 7, admission.Release()
			}, func(context.Context, uint64) error { t.Fatal("implicit network wait before return"); return nil })
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := l.AppendBoundPreparedContext(ctx, a, []Op{put("projects", "p", "v")}, func(_ Record, durable func() error) error {
				if mode == "prepare" {
					return errors.New("candidate refused")
				}
				err := durable()
				if mode == "ignored-ordering-error" {
					return nil
				}
				if err != nil {
					return err
				}
				if mode == "publication" {
					return errors.New("root fsync uncertain")
				}
				return nil
			})
			if err == nil || receipt.Record.Hash != ([32]byte{}) || receipt.ReplicationIndex != 0 {
				t.Fatal("failure receipt admitted", receipt, err)
			}
			switch mode {
			case "prepare", "admission":
				if l.Head() != before || admission.releases != 0 || orderingOwned {
					t.Fatal(l.Head(), admission, orderingOwned)
				}
			case "cancel-after-admission", "foreign-after-admission", "admission-mutates-frames":
				if l.Head() != before || admission.releases != 1 || orderingOwned {
					t.Fatal(l.Head(), admission, orderingOwned)
				}
			case "sync":
				if len(l.uncertainAdmissions) != 1 || admission.releases != 0 || orderingOwned {
					t.Fatal("lost native debt", l.uncertainAdmissions, admission, orderingOwned)
				}
			case "ordering", "zero-ordering", "ignored-ordering-error":
				if l.Head().Sequence != 1 || !orderingOwned || len(l.uncertainAdmissions) != 0 || admission.releases != 0 {
					t.Fatal("double-owned ordering debt", l.Head(), orderingOwned, l.uncertainAdmissions, admission)
				}
				if err := admission.Release(); err != nil {
					t.Fatal(err)
				} // Test's callback was the actual transferred owner.
			case "publication":
				if l.Head().Sequence != 1 || !orderingOwned || admission.releases != 1 {
					t.Fatal(l.Head(), orderingOwned, admission)
				}
			}
			if mode == "sync" || mode == "ordering" || mode == "zero-ordering" || mode == "ignored-ordering-error" || mode == "publication" {
				entered := false
				if _, err := l.AppendBoundPreparedContext(context.Background(), a, []Op{put("projects", "again", "v")}, func(Record, func() error) error { entered = true; return nil }); err == nil || entered {
					t.Fatal("uncertain owner accepted another append", err)
				}
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			if mode == "sync" && admission.releases != 1 {
				t.Fatal("known native close failed to join debt", admission)
			}
		})
	}
}
