package bolt

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func definitionExportFixture(t *testing.T, count int, creatorBytes int) (*Store, domain.Project, []domain.OutcomeDefinition) {
	t.Helper()
	s := openJournalledStore(t, filepath.Join(t.TempDir(), "metadata.db"))
	project, err := s.PutProject(context.Background(), domain.Project{ID: "prj_export", Name: "export", Enabled: true}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	items := make([]domain.OutcomeDefinition, 0, count)
	for i := 0; i < count; i++ {
		item := domain.OutcomeDefinition{ID: fmt.Sprintf("odef_%04d", count-i), ProjectID: project.ID, Name: fmt.Sprintf("name_%04d", i), Version: 1,
			DataType: domain.OutcomeBoolean, SuccessValues: []string{"true"}, CreatedAt: time.Now().UTC(), CreatedBy: strings.Repeat("x", creatorBytes)}
		stored, err := s.PutOutcomeDefinition(context.Background(), item, project.Revision, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, stored)
	}
	return s, project, items
}

func readAllDefinitionExport(ctx context.Context, v *OutcomeDefinitionExportView) ([]domain.OutcomeDefinition, error) {
	var result []domain.OutcomeDefinition
	for {
		page, err := v.ReadPage(ctx, 3, OutcomeDefinitionExportMaxBytes)
		if err != nil {
			return nil, err
		}
		result = append(result, page.Items...)
		if page.Done {
			return result, v.Complete(ctx)
		}
	}
}

func TestOutcomeDefinitionExportKeepsFixedGlobalPrefixAndReleasesOwner(t *testing.T) {
	for _, count := range []int{0, 17} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, project, expected := definitionExportFixture(t, count, 1)
			ctx := context.Background()
			var retained *OutcomeDefinitionExportView
			err := s.WithOutcomeDefinitionExport(ctx, func(v *OutcomeDefinitionExportView) error {
				retained = v
				if v.Head().Count != uint64(count) {
					t.Fatalf("head=%+v", v.Head())
				}
				newRow := domain.OutcomeDefinition{ID: "odef_0000", ProjectID: project.ID, Name: "late", Version: 1, DataType: domain.OutcomeBoolean, SuccessValues: []string{"true"}, CreatedAt: time.Now().UTC(), CreatedBy: "late"}
				if _, err := s.PutOutcomeDefinition(ctx, newRow, project.Revision, 0, nil); err != nil {
					return err
				}
				if count > 0 {
					newVersion := expected[0]
					newVersion.Version++
					newVersion.CreatedBy = "newversion"
					if _, err := s.PutOutcomeDefinition(ctx, newVersion, project.Revision, newVersion.Revision, nil); err != nil {
						return err
					}
				}
				actual, err := readAllDefinitionExport(ctx, v)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(actual, expected) && !(len(actual) == 0 && len(expected) == 0) {
					t.Fatalf("prefix rows differ: %d/%d", len(actual), len(expected))
				}
				if s.db.Stats().OpenTxN != 0 {
					t.Fatal("view retained a transaction")
				}
				if err := v.Complete(ctx); !errors.Is(err, ErrDefinitionExportClosed) {
					t.Fatalf("repeated Complete=%v", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if retained.read != nil || retained.ctx != nil || retained.active {
				t.Fatal("owner retained after callback")
			}
			if _, err := retained.ReadPage(ctx, 1, 1); !errors.Is(err, ErrDefinitionExportClosed) {
				t.Fatalf("late=%v", err)
			}
			if err := s.WithOutcomeDefinitionExport(ctx, func(v *OutcomeDefinitionExportView) error {
				actual, err := readAllDefinitionExport(ctx, v)
				if err != nil {
					return err
				}
				want := count + 1
				if count > 0 {
					want++
				}
				if len(actual) != want {
					t.Fatalf("later prefix=%d want=%d", len(actual), want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutcomeDefinitionExportWideRowsRetryAndConcurrentPages(t *testing.T) {
	s, _, expected := definitionExportFixture(t, 8, (1<<20)+1)
	ctx := context.Background()
	if err := s.WithOutcomeDefinitionExport(ctx, func(v *OutcomeDefinitionExportView) error {
		page, err := v.ReadPage(ctx, 1, 100)
		if !errors.Is(err, ErrDefinitionExportLimit) || len(page.Items) != 0 || v.seen != 0 {
			t.Fatalf("small page=%+v err=%v seen=%d", page, err, v.seen)
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		actual := map[string]domain.OutcomeDefinition{}
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				page, err := v.ReadPage(ctx, 1, OutcomeDefinitionExportMaxBytes)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					t.Errorf("read=%v", err)
					return
				}
				if len(page.Items) != 1 {
					t.Errorf("page=%+v", page)
					return
				}
				row := page.Items[0]
				if _, ok := actual[row.ID]; ok {
					t.Error("duplicate")
				}
				actual[row.ID] = row
			}()
		}
		wg.Wait()
		for _, row := range expected {
			if !reflect.DeepEqual(actual[row.ID], row) {
				t.Error("wide definition changed")
			}
		}
		return v.Complete(ctx)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOutcomeDefinitionExportFailsClosedWithoutPartialRows(t *testing.T) {
	for _, mode := range []string{"omit", "early", "rootcancel", "childcancel", "missing", "changed", "chain", "epoch", "sequence", "closed", "ignored", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			s, _, items := definitionExportFixture(t, 3, 1)
			root, cancel := context.WithCancel(context.Background())
			defer cancel()
			var retained *OutcomeDefinitionExportView
			err := s.WithOutcomeDefinitionExport(root, func(v *OutcomeDefinitionExportView) error {
				retained = v
				if mode == "omit" {
					return nil
				}
				if mode == "early" {
					return v.Complete(context.Background())
				}
				child := context.Background()
				if mode == "rootcancel" {
					cancel()
				}
				if mode == "childcancel" {
					c, stop := context.WithCancel(child)
					stop()
					child = c
				}
				if mode == "closed" {
					if err := s.Close(); err != nil {
						return err
					}
				}
				if mode == "invalid" {
					p, err := v.ReadPage(child, OutcomeDefinitionExportMaxRows+1, OutcomeDefinitionExportMaxBytes)
					if !errors.Is(err, ErrDefinitionExportLimit) || len(p.Items) != 0 {
						t.Fatalf("invalid=%+v,%v", p, err)
					}
					_, err = readAllDefinitionExport(child, v)
					return err
				}
				if mode == "missing" || mode == "changed" || mode == "chain" || mode == "epoch" || mode == "sequence" || mode == "ignored" {
					if err := s.db.Update(func(tx *bbolt.Tx) error {
						switch mode {
						case "missing", "ignored":
							return tx.Bucket(bucketOutcomeDefinitionExport).Delete(encodeUint64(2))
						case "changed":
							return tx.Bucket(bucketOutcomeDefinitions).Put(outcomeDefinitionKey(items[1].ID, 1), []byte(`{"id":"changed"}`))
						case "chain":
							raw := append([]byte(nil), tx.Bucket(bucketOutcomeDefinitionExport).Get(encodeUint64(2))...)
							raw[len(raw)-1] ^= 1
							return tx.Bucket(bucketOutcomeDefinitionExport).Put(encodeUint64(2), raw)
						case "epoch":
							return tx.Bucket(bucketMeta).Put(keyMetadataJournalEpoch, encodeUint64(v.head.Epoch+1))
						case "sequence":
							return tx.Bucket(bucketMeta).Put(keyAppliedJournalSequence, encodeUint64(0))
						}
						return nil
					}); err != nil {
						return err
					}
				}
				p, err := v.ReadPage(child, 3, OutcomeDefinitionExportMaxBytes)
				if err == nil || len(p.Items) != 0 || v.seen != 0 {
					t.Fatalf("partial=%+v err=%v seen=%d", p, err, v.seen)
				}
				if mode == "ignored" {
					return nil
				}
				if again := v.Complete(context.Background()); again == nil {
					t.Fatal("Complete ignored source failure")
				}
				return err
			})
			if mode == "invalid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid export accepted")
			}
			if retained.read != nil || retained.ctx != nil {
				t.Fatal("failed callback retained owner")
			}
		})
	}
}

func TestOutcomeDefinitionExportIndexFailureRollsBackDefinition(t *testing.T) {
	s, project, items := definitionExportFixture(t, 1, 1)
	ctx := context.Background()
	if err := s.update(func(tx *Tx) error {
		return tx.Bucket(bucketOutcomeDefinitionExport).Put(encodeUint64(2), []byte("bad"))
	}); err != nil {
		t.Fatal(err)
	}
	next := items[0]
	next.Version++
	if _, err := s.PutOutcomeDefinition(ctx, next, project.Revision, next.Revision, nil); !errors.Is(err, ErrDefinitionExportCorrupt) {
		t.Fatalf("Put=%v", err)
	}
	if _, err := s.GetOutcomeDefinition(ctx, project.ID, next.ID, next.Version); !errors.Is(err, ErrNotFound) {
		t.Fatalf("definition survived index failure: %v", err)
	}
}
