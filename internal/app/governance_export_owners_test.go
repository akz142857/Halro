package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/governance"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
)

func TestGovernanceExportFromRealDiskOwnersKeepsCutsAndRejectsPublicationFailures(t *testing.T) {
	for _, mode := range []string{"complete", "retire-at-gate", "cancel-at-gate", "close-gov-owner", "metadata-incomplete", "outcome-inconsistent"} {
		t.Run(mode, func(t *testing.T) {
			r, bootstrap, _ := openBootstrappedRuntime(t)
			defer r.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			project, err := r.store.GetProject(ctx, bootstrap.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			creator, count, key := "admin", 1, bootstrap.KeyID
			if mode == "complete" {
				creator = strings.Repeat("d", (1<<20)+1)
				count = 10
				key = strings.Repeat("k", 900<<10)
			}
			now := time.Now().UTC()
			definition := domain.OutcomeDefinition{ID: "odef_owners", ProjectID: bootstrap.ProjectID, Name: "owners", Version: 1, DataType: domain.OutcomeCategorical, AllowedValues: []string{"accepted", "rejected"}, SuccessValues: []string{"accepted"}, Enabled: true, CreatedAt: now, CreatedBy: creator}
			definition, err = r.store.PutOutcomeDefinition(ctx, definition, project.Revision, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			intent := func(label string) budget.GovernanceIntent {
				return budget.GovernanceIntent{Operation: label, IdempotencyKeyHash: outcomePageTestLabel(label), RequestFingerprint: outcomePageTestLabel("fp-" + label)}
			}
			requests := []budget.Request{}
			last := ""
			lastUnit := ""
			for i := 0; i < count; i++ {
				w, _, err := r.accounting.CreateWorkUnitWithDefinitions(ctx, bootstrap.ProjectID, key, 20, []domain.OutcomeDefinitionRef{{ID: definition.ID, Version: 1}}, intent(fmt.Sprintf("owners-unit-%d", i)))
				if err != nil {
					t.Fatal(err)
				}
				run, _, err := r.accounting.CreateRun(ctx, bootstrap.ProjectID, bootstrap.KeyID, w.ID, 1000, time.Hour, 20, intent(fmt.Sprintf("owners-run-%d", i)))
				if err != nil {
					t.Fatal(err)
				}
				request, err := r.accounting.BeginRequestAttributed(ctx, bootstrap.ProjectID, bootstrap.KeyID, fmt.Sprintf("req_owners%d", i), "model", w.ID, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				requests = append(requests, request)
				if _, _, err := r.accounting.CloseWorkUnit(ctx, bootstrap.ProjectID, bootstrap.KeyID, w.ID, intent(fmt.Sprintf("owners-close-%d", i))); err != nil {
					t.Fatal(err)
				}
				previous := ""
				for revision := 0; revision < 6; revision++ {
					label := fmt.Sprintf("owners-%d-%d", i, revision)
					outcome, _, err := r.governance.manager.Report(ctx, governance.ReportInput{ProjectID: bootstrap.ProjectID, WorkUnitID: w.ID, DefinitionID: definition.ID, ReporterKeyID: bootstrap.KeyID, Value: "accepted", ObservedAt: now, SupersedesOutcomeID: previous, Intent: governance.Intent{IdempotencyKeyHash: outcomePageTestLabel(label), RequestFingerprint: outcomePageTestLabel("fp-" + label)}})
					if err != nil {
						t.Fatal(err)
					}
					previous = outcome.ID
				}
				last, lastUnit = previous, w.ID
			}
			if mode == "outcome-inconsistent" {
				// Authenticated, schema-valid native input with a wrong accounting
				// scope must not become a provisional=false success during export.
				e := governance.Event{EventID: "gov_inconsistent", ProjectID: bootstrap.ProjectID + "x", WorkUnitID: lastUnit, DefinitionID: definition.ID, DefinitionVersion: 1,
					OutcomeID: "out_inconsistent", Value: "accepted", ReporterKeyID: bootstrap.KeyID, ObservedAt: now, IngestedAt: now, Revision: 1,
					IdempotencyKeyHash: outcomePageTestLabel("inconsistent"), RequestFingerprint: outcomePageTestLabel("fp-inconsistent")}
				record, err := r.governance.log.Append(ctx, e)
				if err != nil {
					t.Fatal(err)
				}
				if err := r.governance.owner.projection.Apply(ctx, record); err != nil {
					t.Fatal(err)
				}
			}
			expectedDefinitions := []domain.OutcomeDefinition{definition}
			if mode == "complete" {
				// Global metadata source: historical versions and another project
				// must be included, even when none of its definitions has an Outcome.
				version := definition
				version.Version++
				version.CreatedBy = "second-version"
				version, err = r.store.PutOutcomeDefinition(ctx, version, project.Revision, definition.Revision, nil)
				if err != nil {
					t.Fatal(err)
				}
				expectedDefinitions = append(expectedDefinitions, version)
				other, err := r.store.PutProject(ctx, domain.Project{ID: "prj_owners_extra", Name: "other", Enabled: true}, 0, nil)
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 15; i++ {
					row := domain.OutcomeDefinition{ID: fmt.Sprintf("odef_extra%04d", i), ProjectID: other.ID, Name: fmt.Sprintf("extra_%04d", i), Version: 1, DataType: domain.OutcomeBoolean, SuccessValues: []string{"true"}, CreatedAt: now, CreatedBy: "other-project"}
					row, err = r.store.PutOutcomeDefinition(ctx, row, other.Revision, 0, nil)
					if err != nil {
						t.Fatal(err)
					}
					expectedDefinitions = append(expectedDefinitions, row)
				}
			}
			// Select real authenticated fresh candidates after draining existing
			// producers. This fixture stays draining; it publishes no Runtime role.
			if err := r.drainAccountingBusiness(ctx, nil); err != nil {
				t.Fatal(err)
			}
			master, err := unlockMasterKey(ctx, r.config)
			if err != nil {
				t.Fatal(err)
			}
			ledgerKey, err := loadLedgerHMACKey(r.store, r.vault, master)
			clear(master)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(ledgerKey)
			lease, p := projectedWriterCandidate(t, r.ledger, r.config.LedgerPath(), ledgerKey)
			next, err := budget.NewWithProjection(ctx, r.ledger, p, r.periods, lease, budget.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			r.accounting = next
			defer func() { r.Close(); p.Close() }()
			oldState := r.accountingState.reader
			r.accountingState.reader = nil
			defer func() { r.accountingState.reader = oldState }()
			govProjection := installTestGovernanceProjection(t, r)
			if r.accountingState.reader != nil || r.governance.manager.State() != nil {
				t.Fatal("export retained legacy aliases")
			}
			cut, _, err := p.ReadPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			govCut := r.governance.log.Summary()
			directory := filepath.Join(t.TempDir(), "export")
			read, completed := false, 0
			// This fixture has exactly one immutable definition. It deliberately
			// does not claim to implement the missing global metadata prefix API.
			definitions := governance.ExportDefinitionSource{
				Read: func(ctx context.Context, yield func(domain.OutcomeDefinition) error) error {
					stored, err := r.store.GetOutcomeDefinition(ctx, bootstrap.ProjectID, definition.ID, definition.Version)
					if err != nil {
						return err
					}
					if err := yield(stored); err != nil {
						return err
					}
					read = true
					// All facts were already written at the old independent cuts.
					// Legitimate new native facts must not alter those file rows.
					for _, request := range requests {
						if err := next.Finalize(ctx, request, "success"); err != nil {
							return err
						}
					}
					_, _, err = r.governance.manager.Report(ctx, governance.ReportInput{ProjectID: bootstrap.ProjectID, WorkUnitID: lastUnit, DefinitionID: definition.ID, ReporterKeyID: bootstrap.KeyID, Value: "rejected", ObservedAt: now, SupersedesOutcomeID: last, Intent: governance.Intent{IdempotencyKeyHash: outcomePageTestLabel("owners-late"), RequestFingerprint: outcomePageTestLabel("fp-owners-late")}})
					if err != nil {
						return err
					}
					if mode == "close-gov-owner" {
						return govProjection.Close()
					}
					return nil
				},
				Complete: func(ctx context.Context) error {
					completed++
					if !read {
						return errors.New("fixture definition not read")
					}
					if _, err := os.Stat(filepath.Join(directory, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("manifest already published at completion", err)
					}
					if mode == "cancel-at-gate" {
						cancel()
						return ctx.Err()
					}
					if mode == "metadata-incomplete" {
						return errors.New("fixture metadata prefix incomplete")
					}
					if mode == "retire-at-gate" {
						deadline, stop := context.WithTimeout(ctx, 20*time.Millisecond)
						defer stop()
						if err := next.Retire(deadline); !errors.Is(err, context.DeadlineExceeded) {
							t.Fatal("publication ticket released early", err)
						}
						if _, _, err := next.ReadWorkUnit(ctx, bootstrap.ProjectID, lastUnit); !errors.Is(err, budget.ErrAccountingRetired) {
							t.Fatal("retired owner reopened", err)
						}
					}
					stored, err := r.store.GetOutcomeDefinition(ctx, bootstrap.ProjectID, definition.ID, definition.Version)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(stored, definition) {
						return errors.New("fixture immutable definition changed")
					}
					return nil
				},
			}
			var manifest governance.ExportManifest
			if mode == "complete" {
				var retained *boltstore.OutcomeDefinitionExportView
				manifest, err = r.governance.manager.WriteExportWithDefinitionOwner(ctx, directory, func(ctx context.Context, visit func(governance.ExportDefinitionSource) error) error {
					return r.store.WithOutcomeDefinitionExport(ctx, func(view *boltstore.OutcomeDefinitionExportView) error {
						retained = view
						if view.Head().Count != uint64(len(expectedDefinitions)) {
							t.Fatalf("metadata cut=%+v", view.Head())
						}
						return visit(governance.ExportDefinitionSource{
							Read: func(ctx context.Context, yield func(domain.OutcomeDefinition) error) error {
								for {
									page, err := view.ReadPage(ctx, 3, boltstore.OutcomeDefinitionExportMaxBytes)
									if err != nil {
										return err
									}
									for _, row := range page.Items {
										if err := yield(row); err != nil {
											return err
										}
									}
									if page.Done {
										break
									}
								}
								// Existing fixture appends native Ledger/Gov facts. Add
								// a lower ID and later version after the metadata cut too.
								if err := definitions.Read(ctx, func(domain.OutcomeDefinition) error { return nil }); err != nil {
									return err
								}
								future := definition
								future.ID = "odef_0000"
								future.Name = "future"
								if _, err := r.store.PutOutcomeDefinition(ctx, future, project.Revision, 0, nil); err != nil {
									return err
								}
								future = expectedDefinitions[1]
								future.Version++
								_, err := r.store.PutOutcomeDefinition(ctx, future, project.Revision, future.Revision, nil)
								return err
							},
							Complete: func(ctx context.Context) error {
								if err := definitions.Complete(ctx); err != nil {
									return err
								}
								return view.Complete(ctx)
							},
						})
					})
				})
				if retained == nil {
					t.Fatal("metadata owner never captured")
				}
				if _, late := retained.ReadPage(ctx, 1, 1); !errors.Is(late, boltstore.ErrDefinitionExportClosed) {
					t.Fatalf("metadata owner still active=%v", late)
				}
			} else {
				manifest, err = r.governance.manager.WriteExportFromOwners(ctx, directory, definitions)
			}
			if mode == "outcome-inconsistent" {
				if !errors.Is(err, governance.ErrExportInconsistent) || completed != 0 || !reflect.DeepEqual(manifest, governance.ExportManifest{}) {
					t.Fatal("bad scope credited/export gate ran", manifest, completed, err)
				}
				if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("bad scope left output", err)
				}
				return
			}
			if completed != 1 {
				t.Fatal("completion was not called once", completed, err)
			}
			if mode != "complete" && mode != "retire-at-gate" {
				if err == nil || !reflect.DeepEqual(manifest, governance.ExportManifest{}) {
					t.Fatal("failed owner published", manifest, err)
				}
				if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed gate left output", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			verified, err := governance.VerifyExport(directory)
			if err != nil {
				t.Fatal(err)
			}
			if verified.AccountingWatermark != cut || verified.GovernanceWatermark["sequence"].(json.Number).String() != fmt.Sprint(govCut.Records) || verified.GovernanceWatermark["offset"].(json.Number).String() != fmt.Sprint(govCut.Bytes) {
				t.Fatal("independent cuts changed", verified, cut, govCut)
			}
			for _, file := range verified.Files {
				want := count
				if file.Dataset == "outcomes" {
					want = count * 6
				}
				if file.Dataset == "outcome_definitions" {
					want = len(expectedDefinitions)
				}
				if file.RecordCount != want {
					t.Fatal(file, want)
				}
				data, err := os.ReadFile(filepath.Join(directory, file.Path))
				if err != nil {
					t.Fatal(err)
				}
				if file.Dataset == "outcomes" && strings.Count(string(data), `"provisional":true`) != want {
					t.Fatal("late finalize changed old maturity")
				}
				if file.Dataset == "runs" && (strings.Contains(string(data), "committed_micros_usd") || strings.Contains(string(data), "reserved_micros_usd")) {
					t.Fatal("duplicate accounting values")
				}
				if file.Dataset == "outcome_definitions" {
					decoder := json.NewDecoder(bytes.NewReader(data))
					for _, expected := range expectedDefinitions {
						var got domain.OutcomeDefinition
						if decoder.Decode(&got) != nil || !reflect.DeepEqual(got, expected) {
							t.Fatal("global/historical/wide definition changed")
						}
					}
				}
			}
			if live, _, err := p.ReadPosition(context.Background()); err != nil || live.Sequence <= cut.Sequence || r.governance.log.Summary().Records != govCut.Records+1 {
				t.Fatal("native writes did not advance", live, err)
			}
			if mode == "retire-at-gate" {
				if err := next.Retire(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "complete" {
				// Compose the public Admin endpoint with selected real owners.
				// The original Runtime remains drained; this does not publish its
				// role/gateway/background generation or claim business readiness.
				endpoint := &Runtime{config: r.config, logger: r.logger, store: r.store, governance: governanceRuntime{log: r.governance.log, manager: r.governance.manager}, accounting: next, audit: r.audit, adminSessions: r.adminSessions}
				cookie, csrf := loginAdminForTest(t, r)
				invoke := func() *httptest.ResponseRecorder {
					request := adminRequest(t, http.MethodPost, "/admin/api/v1/governance/export", nil)
					request.AddCookie(cookie)
					request.Header.Set("X-CSRF-Token", csrf)
					response := httptest.NewRecorder()
					endpoint.adminRouter().ServeHTTP(response, request)
					return response
				}
				response := invoke()
				if response.Code != http.StatusCreated {
					t.Fatal("disk public export", response.Code, response.Body.String())
				}
				var result struct {
					Directory string
					Manifest  governance.ExportManifest
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				verified, err := governance.VerifyExport(result.Directory)
				if err != nil {
					t.Fatal(err)
				}
				for _, file := range verified.Files {
					if file.Dataset == "outcome_definitions" && file.RecordCount != len(expectedDefinitions)+2 {
						t.Fatal("public metadata source omitted latest prefix", file)
					}
				}
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
				endpoint.accountingState.reader = oldState // A disk failure must not reuse this alias.
				response = invoke()
				if response.Code != http.StatusServiceUnavailable {
					t.Fatal("closed disk fell back", response.Code, response.Body.String())
				}
				if err := endpoint.governance.exports.drain(ctx); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
