package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	bbolt "go.etcd.io/bbolt"
)

func appendExportEvent(t testing.TB, m *Manager, e Event) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	r, err := m.log.Append(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.applyOutcome(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}
func exportSourceFixture(t *testing.T, disk bool, count int) (*Manager, *Log, *GovernanceProjection) {
	t.Helper()
	m, log, _, _, _ := testManager(t)
	t.Cleanup(func() { _ = log.Close() })
	for i := 0; i < count; i++ {
		appendExportEvent(t, m, projectionEvent(i))
	}
	if !disk {
		return m, log, nil
	}
	p := buildTestGovernanceProjection(t, log)
	return projectedTestManager(t, m, p), log, p
}
func collectOutcomeExport(ctx context.Context, v OutcomeExportView, rows, limit int) ([]domain.Outcome, error) {
	result := []domain.Outcome{}
	for calls := 0; ; calls++ {
		if calls > 2000 {
			return nil, errors.New("export did not progress")
		}
		page, err := v.ReadPage(ctx, rows, limit)
		if err != nil {
			return nil, err
		}
		if len(page.Items) > rows || page.Bytes > limit {
			return nil, errors.New("page exceeded limit")
		}
		result = append(result, page.Items...)
		if page.Done {
			return result, nil
		}
	}
}
func TestOutcomeExportMatchesImmutableFactsAndCompletesEmptyCut(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, count := range []int{0, 17} {
			t.Run(fmt.Sprintf("disk=%v/count=%d", disk, count), func(t *testing.T) {
				m, log, _ := exportSourceFixture(t, disk, count)
				oracle := NewState()
				if _, err := log.Replay(oracle.Apply); err != nil {
					t.Fatal(err)
				}
				want := oracle.Outcomes("", "", "")
				sort.Slice(want, func(i, j int) bool { return want[i].ID < want[j].ID })
				var saved *outcomeExportView
				err := m.WithOutcomeExport(context.Background(), func(view OutcomeExportView) error {
					saved = view.(*outcomeExportView)
					got, err := collectOutcomeExport(context.Background(), view, 3, OutcomePageMaxBytes)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(got, want) || view.Head() != log.Summary() {
						t.Fatal(got, want, view.Head())
					}
					if err := view.Complete(context.Background()); err != nil {
						return err
					}
					if err := view.Complete(context.Background()); !errors.Is(err, ErrOutcomeExportClosed) {
						t.Fatal(err)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if saved.m != nil || saved.log != nil || saved.state != nil || saved.projection != nil || saved.ctx != nil {
					t.Fatal("view retained owner after sealing")
				}
				if page, err := saved.ReadPage(context.Background(), 1, OutcomePageMaxBytes); !errors.Is(err, ErrOutcomeExportClosed) || page.Items != nil {
					t.Fatal(page, err)
				}
			})
		}
	}
}

func TestOutcomeExportCutExcludesFuturePrefixAndStopsBeforeSuffix(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(fmt.Sprintf("disk=%v", disk), func(t *testing.T) {
			m, _, p := exportSourceFixture(t, disk, 3)
			ctx := context.Background()
			if err := m.WithOutcomeExport(ctx, func(v OutcomeExportView) error {
				if p != nil && p.db.Stats().OpenTxN != 0 {
					t.Fatal("capture retained reader")
				}
				for i := 0; i < 430; i++ {
					e := projectionEvent(i + 1000)
					e.OutcomeID = fmt.Sprintf("out_00000000future%04d", i)
					appendExportEvent(t, m, e) // All are before the last original ID.
				}
				for i := 0; i < 40; i++ {
					e := projectionEvent(i + 2000)
					e.OutcomeID = fmt.Sprintf("out_zfuture%04d", i)
					appendExportEvent(t, m, e)
				}
				got := []string{}
				empty := 0
				for calls := 0; ; calls++ {
					if calls > 10 {
						t.Fatal("future prefix did not make finite progress")
					}
					page, err := v.ReadPage(ctx, 1, OutcomePageMaxBytes)
					if err != nil {
						return err
					}
					if len(page.Items) == 0 && !page.Done {
						empty++
					}
					for _, row := range page.Items {
						got = append(got, row.ID)
						if row.GovernanceSequence > v.Head().Records {
							t.Fatal("future fact leaked", row)
						}
					}
					if p != nil && p.db.Stats().OpenTxN != 0 {
						t.Fatal("page retained reader")
					}
					if page.Done {
						break
					}
				}
				if !reflect.DeepEqual(got, []string{"out_00000000", "out_00000001", "out_00000002"}) || disk && empty < 2 {
					t.Fatal(got, empty)
				}
				return v.Complete(ctx)
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutcomeExportIncompleteCanceledAndReplacedOwnersCannotComplete(t *testing.T) {
	for _, disk := range []bool{false, true} {
		for _, mode := range []string{"omit-complete", "early-complete", "cancel-root", "cancel-read", "missing-row", "owner-replace", "page-key", "same-head-hash", "close-log", "close-projection", "ignored-source"} {
			if mode == "close-projection" && !disk {
				continue
			}
			t.Run(fmt.Sprintf("disk=%v/%s", disk, mode), func(t *testing.T) {
				m, log, p := exportSourceFixture(t, disk, 3)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var saved *outcomeExportView
				err := m.WithOutcomeExport(ctx, func(v OutcomeExportView) error {
					saved = v.(*outcomeExportView)
					if mode == "early-complete" {
						return v.Complete(ctx)
					}
					readCtx := context.Background()
					switch mode {
					case "cancel-root":
						cancel()
					case "cancel-read":
						c, stop := context.WithCancel(readCtx)
						stop()
						readCtx = c
					case "owner-replace":
						m.mu.Lock()
						m.state = NewState()
						m.mu.Unlock()
					case "page-key":
						m.mu.Lock()
						m.pageKey[0] ^= 1
						m.mu.Unlock()
					case "same-head-hash":
						log.mu.Lock()
						log.lastHash[0] ^= 1
						log.mu.Unlock()
					case "close-log":
						if err := log.Close(); err != nil {
							t.Fatal(err)
						}
					case "close-projection":
						if err := p.Close(); err != nil {
							t.Fatal(err)
						}
					case "missing-row", "ignored-source":
						if p != nil {
							if err := p.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("outcomes")).Delete([]byte("out_00000002")) }); err != nil {
								t.Fatal(err)
							}
						} else {
							m.state.mu.Lock()
							delete(m.state.outcomes, "out_00000002")
							m.state.mu.Unlock()
						}
					}
					got, readErr := collectOutcomeExport(readCtx, v, 2, OutcomePageMaxBytes)
					if mode == "omit-complete" {
						if readErr != nil {
							t.Fatal(readErr)
						}
						return nil
					}
					if readErr == nil || got != nil {
						t.Fatal("bad source returned valid result", got, readErr)
					}
					if err := v.Complete(context.Background()); err == nil {
						t.Fatal("ignored source failure completed")
					}
					return nil
				})
				if err == nil {
					t.Fatal("incomplete source succeeded")
				}
				if mode == "cancel-root" && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if saved.m != nil || saved.log != nil || saved.state != nil || saved.projection != nil || saved.ctx != nil {
					t.Fatal("failure retained owner")
				}
			})
		}
	}
}

func TestOutcomeExportByteRetryDoesNotAdvanceAndConcurrentReadsSerialize(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(fmt.Sprintf("disk=%v", disk), func(t *testing.T) {
			m, _, _ := exportSourceFixture(t, disk, 0)
			for i := 0; i < 8; i++ {
				e := projectionEvent(i)
				e.ReporterKeyID = strings.Repeat("r", 50<<10)
				appendExportEvent(t, m, e)
			}
			ctx := context.Background()
			if err := m.WithOutcomeExport(ctx, func(v OutcomeExportView) error {
				if page, err := v.ReadPage(ctx, 1, 128); !errors.Is(err, ErrOutcomePageLimit) || page.Items != nil {
					t.Fatal(page, err)
				}
				var wg sync.WaitGroup
				results := make(chan OutcomeExportPage, 8)
				errs := make(chan error, 8)
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						page, err := v.ReadPage(ctx, 1, OutcomePageMaxBytes)
						if err != nil {
							errs <- err
						} else {
							results <- page
						}
					}()
				}
				wg.Wait()
				close(results)
				close(errs)
				for err := range errs {
					return err
				}
				ids := []string{}
				done := 0
				for page := range results {
					if len(page.Items) != 1 {
						t.Fatal(page)
					}
					ids = append(ids, page.Items[0].ID)
					if page.Done {
						done++
					}
				}
				sort.Strings(ids)
				if len(ids) != 8 || done != 1 {
					t.Fatal(ids, done)
				}
				for i, id := range ids {
					if id != projectionEvent(i).OutcomeID {
						t.Fatal("retry skipped an item", ids)
					}
				}
				return v.Complete(ctx)
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStreamingExportBeforePublishRequiresCompletedSources(t *testing.T) {
	for _, mode := range []string{"complete", "hook-error", "cancel-hook", "cancel-after-complete", "omit-source-page", "source-corrupt"} {
		t.Run(mode, func(t *testing.T) {
			m, _, p := exportSourceFixture(t, true, 5)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := filepath.Join(t.TempDir(), "export")
			hooks := 0
			err := m.WithOutcomeExport(ctx, func(v OutcomeExportView) error {
				input := ExportStreamInput{GovernanceSequence: v.Head().Records, GovernanceOffset: v.Head().Bytes}
				input.Outcomes = func(ctx context.Context, yield func(domain.Outcome) error) error {
					for {
						page, err := v.ReadPage(ctx, 2, OutcomePageMaxBytes)
						if err != nil {
							if mode == "source-corrupt" {
								return nil
							}
							return err
						}
						for _, row := range page.Items {
							if err := yield(row); err != nil {
								return err
							}
						}
						if mode == "omit-source-page" {
							return nil
						}
						if mode == "source-corrupt" {
							if err := p.db.Update(func(tx *bbolt.Tx) error {
								return tx.Bucket([]byte("outcomes")).Put([]byte("out_00000004"), []byte("{"))
							}); err != nil {
								return err
							}
						}
						if page.Done {
							return nil
						}
					}
				}
				input.BeforePublish = func(ctx context.Context) error {
					hooks++
					if _, err := os.Stat(filepath.Join(dir, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("manifest existed before gate", err)
					}
					if _, err := os.Stat(filepath.Join(dir, ".manifest.json.tmp")); err != nil {
						t.Fatal("manifest temp not synced first", err)
					}
					for _, name := range []string{"work_units", "runs", "outcomes", "outcome_definitions"} {
						if _, err := os.Stat(filepath.Join(dir, name+".ndjson")); err != nil {
							t.Fatal("dataset not complete before gate", name, err)
						}
					}
					if mode == "hook-error" {
						return errors.New("completion refused")
					}
					if mode == "cancel-hook" {
						cancel()
					}
					err := v.Complete(ctx)
					if mode == "cancel-after-complete" {
						cancel()
					}
					return err
				}
				manifest, err := WriteExportStream(ctx, dir, input)
				if mode != "complete" {
					if err == nil || !reflect.DeepEqual(manifest, ExportManifest{}) {
						t.Fatal("failed gate published", manifest, err)
					}
				}
				return err
			})
			if hooks != 1 {
				t.Fatal("gate did not run once", hooks)
			}
			if mode == "complete" {
				if err != nil {
					t.Fatal(err)
				}
				manifest, err := VerifyExport(dir)
				if err != nil || manifest.GovernanceWatermark["sequence"].(json.Number).String() != "5" {
					t.Fatal(manifest, err)
				}
			} else {
				if err == nil {
					t.Fatal("bad source succeeded")
				}
				if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("gate failure left output", err)
				}
			}
		})
	}
}
