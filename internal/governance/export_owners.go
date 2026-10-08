package governance

import (
	"context"
	"errors"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

var ErrExportInconsistent = errors.New("governance export references inconsistent accounting facts")

// The caller supplies a finite fixed metadata source and its pre-publication
// completion check; nil cannot silently substitute an empty definition dataset.
type ExportDefinitionSource struct {
	Read     ExportProducer[domain.OutcomeDefinition]
	Complete func(context.Context) error
}

// Capture definitions AFTER the Governance and accounting cuts: all captured
// facts and WorkUnit declarations must already have their immutable definitions.
// The owner keeps its view alive through Complete AND manifest publication.
type ExportDefinitionOwner func(context.Context, func(ExportDefinitionSource) error) error

// WriteExportFromOwners joins finite immutable Governance facts with one
// independent accounting cut. Capture Governance first so already-referenced
// WorkUnits can be present in the subsequent accounting cut. The selected
// accounting reader is the SAME Manager dependency used by Report, never a
// legacy State alias. Its operation ticket lasts through manifest publication.
// The caller retains/joins Governance and Metadata resources through this call.
func (m *Manager) WriteExportFromOwners(ctx context.Context, directory string, definitions ExportDefinitionSource) (ExportManifest, error) {
	if err := ctx.Err(); err != nil {
		return ExportManifest{}, err
	}
	if definitions.Read == nil || definitions.Complete == nil {
		return ExportManifest{}, ErrOutcomeExportIncomplete
	}
	return m.WriteExportWithDefinitionOwner(ctx, directory, func(_ context.Context, visit func(ExportDefinitionSource) error) error {
		return visit(definitions)
	})
}

func (m *Manager) WriteExportWithDefinitionOwner(ctx context.Context, directory string, owner ExportDefinitionOwner) (ExportManifest, error) {
	ctx, release, err := m.enterOperation(ctx)
	if err != nil {
		return ExportManifest{}, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return ExportManifest{}, err
	}
	if owner == nil {
		return ExportManifest{}, ErrOutcomeExportIncomplete
	}
	m.mu.Lock()
	accounting, ok := m.accounting.(ledger.AccountingExportReader)
	m.mu.Unlock()
	if !ok {
		return ExportManifest{}, ledger.ErrGovernanceExportBackend
	}
	var result ExportManifest
	err = m.WithOutcomeExport(ctx, func(facts OutcomeExportView) error {
		return accounting.WithGovernanceExport(ctx, func(accounts ledger.GovernanceExportView) error {
			return owner(ctx, func(definitions ExportDefinitionSource) error {
				if definitions.Read == nil || definitions.Complete == nil {
					return ErrOutcomeExportIncomplete
				}
				input := ExportStreamInput{AccountingWatermark: accounts.Head(), GovernanceSequence: facts.Head().Records,
					GovernanceOffset: facts.Head().Bytes, Definitions: definitions.Read}
				input.WorkUnits = func(ctx context.Context, yield func(domain.WorkUnit) error) error {
					o := ledger.GovernanceListOptions{Limits: ledger.ProjectionPageLimits{Rows: ledger.GovernanceListMaxRows, Bytes: ledger.GovernanceListMaxBytes}}
					for {
						page, err := accounts.ReadWorkUnitPage(ctx, o)
						if err != nil {
							return err
						}
						for _, w := range page.Items {
							row := domain.WorkUnit{ID: w.ID, ProjectID: w.ProjectID, Status: w.Status, CreatedAt: w.CreatedAt, ClosedAt: w.ClosedAt, OutcomeDefinitions: w.OutcomeDefinitions}
							if err := yield(row); err != nil {
								return err
							}
						}
						if page.NextID == "" {
							return nil
						}
						o.After = page.NextID
					}
				}
				input.Runs = func(ctx context.Context, yield func(domain.Run) error) error {
					o := ledger.GovernanceListOptions{Limits: ledger.ProjectionPageLimits{Rows: ledger.GovernanceListMaxRows, Bytes: ledger.GovernanceListMaxBytes}}
					for {
						page, err := accounts.ReadRunPage(ctx, o)
						if err != nil {
							return err
						}
						for _, r := range page.Items {
							row := domain.Run{ID: r.ID, ProjectID: r.ProjectID, WorkUnitID: r.WorkUnitID, BudgetMicrosUSD: r.BudgetMicrosUSD, Status: r.Status, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, ClosedAt: r.ClosedAt}
							if err := yield(row); err != nil {
								return err
							}
						}
						if page.NextID == "" {
							return nil
						}
						o.After = page.NextID
					}
				}
				input.Outcomes = func(ctx context.Context, yield func(domain.Outcome) error) error {
					for {
						page, err := facts.ReadPage(ctx, OutcomePageMaxRows, OutcomePageMaxBytes)
						if err != nil {
							return err
						}
						provisional, err := resolveExportMaturity(ctx, accounts, page.Items)
						if err != nil {
							return err
						}
						for i, row := range page.Items {
							row.Provisional = provisional[i]
							if err := yield(row); err != nil {
								return err
							}
						}
						if page.Done {
							return nil
						}
					}
				}
				input.BeforePublish = func(ctx context.Context) error {
					if err := definitions.Complete(ctx); err != nil {
						return err
					}
					if err := facts.Complete(ctx); err != nil {
						return err
					}
					return accounts.Complete(ctx)
				}
				var err error
				result, err = WriteExportStream(ctx, directory, input)
				return err
			})
		})
	})
	if err != nil {
		return ExportManifest{}, err
	}
	return result, nil
}

// Each input page is <=200 rows/1MiB JSON. Deduplicate BEFORE splitting a
// source-byte overflow: retry subsets must be disjoint so repeated revisions
// cannot query a reference already resolved by an earlier subset.
func resolveExportMaturity(ctx context.Context, accounts ledger.GovernanceExportView, rows []domain.Outcome) ([]bool, error) {
	if len(rows) > ledger.WorkUnitMaturityMaxRows {
		return nil, ErrOutcomePageLimit
	}
	refs := make([]ledger.WorkUnitReference, 0, len(rows))
	indexes := make(map[ledger.WorkUnitReference]int, len(rows))
	identityBytes := 0
	for _, row := range rows {
		ref := ledger.WorkUnitReference{ProjectID: row.ProjectID, WorkUnitID: row.WorkUnitID}
		if _, exists := indexes[ref]; exists {
			continue
		}
		identityBytes += len(ref.ProjectID) + len(ref.WorkUnitID)
		if identityBytes > ledger.WorkUnitMaturityMaxBytes {
			return nil, ErrOutcomePageLimit
		}
		indexes[ref] = len(refs)
		refs = append(refs, ref)
	}
	observations := make([]ledger.WorkUnitMaturity, len(refs))
	var read func([]ledger.WorkUnitReference, []ledger.WorkUnitMaturity) error
	read = func(refs []ledger.WorkUnitReference, output []ledger.WorkUnitMaturity) error {
		page, err := accounts.ReadMaturityPage(ctx, refs)
		if errors.Is(err, ledger.ErrProjectionPageLimit) && len(refs) > 1 {
			middle := len(refs) / 2
			if err := read(refs[:middle], output[:middle]); err != nil {
				return err
			}
			return read(refs[middle:], output[middle:])
		}
		if err != nil {
			return err
		}
		if page.Watermark != accounts.Head() || len(page.Items) != len(refs) {
			return ErrExportInconsistent
		}
		for i, item := range page.Items {
			if item.Reference != refs[i] || !item.Found || item.Status != domain.WorkUnitOpen && item.Status != domain.WorkUnitClosed {
				return ErrExportInconsistent
			}
			output[i] = item
		}
		return nil
	}
	// Even an empty fact page checks the accounting owner at the same cut.
	if err := read(refs, observations); err != nil {
		return nil, err
	}
	result := make([]bool, len(rows))
	for i, row := range rows {
		item := observations[indexes[ledger.WorkUnitReference{ProjectID: row.ProjectID, WorkUnitID: row.WorkUnitID}]]
		result[i] = item.Status == domain.WorkUnitOpen || item.Inflight
	}
	return result, nil
}
