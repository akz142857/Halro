package governance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/ledger"
)

func TestOwnerExportRequiresDefinitionCompletionAndRejectsLegacyAccounting(t *testing.T) {
	m, log, _, _, _ := testManager(t)
	defer log.Close()
	ctx := context.Background()
	called := false
	read := func(context.Context, func(domain.OutcomeDefinition) error) error { called = true; return nil }
	if result, err := m.WriteExportWithDefinitionOwner(ctx, filepath.Join(t.TempDir(), "export"), nil); !errors.Is(err, ErrOutcomeExportIncomplete) || len(result.Files) != 0 {
		t.Fatal("missing owner accepted", result, err)
	}
	for _, input := range []ExportDefinitionSource{{}, {Read: read}, {Complete: func(context.Context) error { return nil }}} {
		dir := filepath.Join(t.TempDir(), "export")
		if result, err := m.WriteExportFromOwners(ctx, dir, input); !errors.Is(err, ErrOutcomeExportIncomplete) || len(result.Files) != 0 {
			t.Fatal(result, err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("missing completer wrote output", err)
		}
	}
	if result, err := m.WriteExportFromOwners(ctx, filepath.Join(t.TempDir(), "export"), ExportDefinitionSource{Read: read, Complete: func(context.Context) error { return nil }}); !errors.Is(err, ledger.ErrGovernanceExportBackend) || len(result.Files) != 0 {
		t.Fatal("legacy fallback used", result, err)
	}
	if called {
		t.Fatal("unsupported input ran metadata producer")
	}
}

type faultyExportMaturity struct {
	ledger.GovernanceExportView
	head ledger.Watermark
	read func([]ledger.WorkUnitReference) (ledger.WorkUnitMaturityPage, error)
}

func (v faultyExportMaturity) Head() ledger.Watermark { return v.head }
func (v faultyExportMaturity) ReadMaturityPage(ctx context.Context, refs []ledger.WorkUnitReference) (ledger.WorkUnitMaturityPage, error) {
	return v.read(refs)
}

func TestExportMaturityRejectsAbsenceWrongScopeHeadCountAndStatus(t *testing.T) {
	for _, mode := range []string{"missing", "wrong-scope", "wrong-head", "short", "wrong-status", "source-error"} {
		t.Run(mode, func(t *testing.T) {
			head := ledger.Watermark{Generation: 1, Sequence: 5, Offset: 500}
			v := faultyExportMaturity{head: head, read: func(refs []ledger.WorkUnitReference) (ledger.WorkUnitMaturityPage, error) {
				p := ledger.WorkUnitMaturityPage{Watermark: head, Items: []ledger.WorkUnitMaturity{{Reference: refs[0], Found: true, Status: domain.WorkUnitClosed}}}
				switch mode {
				case "missing":
					p.Items[0].Found = false
				case "wrong-scope":
					p.Items[0].Reference.ProjectID = "other"
				case "wrong-head":
					p.Watermark.Sequence++
				case "short":
					p.Items = nil
				case "wrong-status":
					p.Items[0].Status = "invalid"
				case "source-error":
					return ledger.WorkUnitMaturityPage{}, ledger.ErrProjectionCorrupt
				}
				return p, nil
			}}
			result, err := resolveExportMaturity(context.Background(), v, []domain.Outcome{{ProjectID: "prj_1", WorkUnitID: "wku_unit"}})
			if err == nil || result != nil {
				t.Fatal("bad accounting reply became false provisional", result, err)
			}
			if mode == "source-error" && !errors.Is(err, ledger.ErrProjectionCorrupt) {
				t.Fatal(err)
			}
		})
	}
}
