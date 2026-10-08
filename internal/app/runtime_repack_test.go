package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/akz142857/Halro/internal/usage"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestSelectedRuntimeOnlineCheckpointReclaimsActualMetadataAndReopens(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	legacy, credentials, _ := openBootstrappedRuntimeForModel(t, "gpt-test")
	cfg, instance := legacy.config, legacy.instanceID
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	limits := selectedRuntimeBudgetFixture()
	limits.MetadataStoreDiskBytes = 16 << 20
	cfg.ResourceBudget = &limits
	cfg = activateClosedRuntimeMemoryStateFixture(t, ctx, cfg, instance, false)
	opts := OpenOptions{AllowGeneratedSetupToken: true, resourceSampleSource: pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := OpenWithOptions(ctx, cfg, logger, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
		t.Fatal(err)
	}
	project := domain.Project{ID: "prj_repack", Name: "Native-backed page churn", Enabled: true}
	// A bounded one-row live value creates obsolete COW pages while retaining
	// constant logical history. The real Metadata journal remains authoritative.
	project.AllowedModels = []string{strings.Repeat("model", 48<<10)}
	// Four actual online cycles alternate both fixed pack slots. Each cycle
	// includes fresh native accounting facts, then reopens the default Runtime.
	for cycle := 0; cycle < 4; cycle++ {
		name, next := "pages", "pages.next"
		if cycle%2 != 0 {
			name, next = next, name
		}
		old := filepath.Join(memoryStateDirectory(cfg), "metadata-root", name)
		for i := 0; ; i++ {
			info, err := os.Stat(old)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() >= limits.MetadataStoreDiskBytes/2 {
				break
			}
			if i >= 128 {
				t.Fatal("actual DAO churn did not reach reclamation threshold")
			}
			project.RPM++
			project, err = r.store.PutProject(ctx, project, project.Revision, nil)
			if err != nil {
				t.Fatal("actual native/Metadata update", cycle, err)
			}
		}
		q, err := r.accounting.BeginRequestDetailed(ctx, credentials.ProjectID, credentials.KeyID, fmt.Sprintf("repack_request_%d", cycle), "chat")
		if err != nil {
			t.Fatal(err)
		}
		a, err := r.accounting.ReserveAttempt(ctx, q, 100, 3)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.Settle(ctx, a, budget.Settlement{CommittedMicrosUSD: 7, Outcome: "success"}); err != nil {
			t.Fatal(err)
		}
		if err := r.accounting.Finalize(ctx, q, "success"); err != nil {
			t.Fatal(err)
		}
		if err := r.usageCollector.CatchUp(ctx); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(old)
		if err != nil {
			t.Fatal(err)
		}
		sealBefore := r.selectedState.generation.Seal.Revision
		accounting, metadata, governance := r.accounting, r.store, r.governance.manager
		if err := r.CheckpointMemoryState(ctx); err != nil {
			t.Fatal("actual online reclamation", cycle, err)
		}
		if r.accounting != accounting || r.store != metadata || r.governance.manager != governance {
			t.Fatal("online checkpoint replaced a live domain owner")
		}
		if r.selectedState.generation.Seal.Revision != sealBefore+2 {
			t.Fatal("missing checkpoint and compact common selections", cycle)
		}
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatal("selected obsolete physical pages retained", cycle, err)
		}
		compact, err := os.Stat(filepath.Join(memoryStateDirectory(cfg), "metadata-root", next))
		if err != nil || compact.Size() >= before.Size()/2 {
			t.Fatal("actual physical page pack was not reduced", cycle, compact, err)
		}
		assertData := func() {
			t.Helper()
			got, err := r.store.GetProject(ctx, project.ID)
			if err != nil || got.Revision != project.Revision || got.RPM != project.RPM || len(got.AllowedModels) != 1 || got.AllowedModels[0] != project.AllowedModels[0] {
				t.Fatal("checkpoint/restart lost actual Metadata", cycle, err)
			}
			// ReadStats fails fast while the reopened Runtime's own Usage
			// catch-up holds a reader slot; that is transient, so retry it
			// within a bound. A leaked slot still fails at the deadline.
			stats, err := r.usage.disk.ReadStats(ctx)
			for deadline := time.Now().Add(30 * time.Second); errors.Is(err, usage.ErrDiskUsageBusy) && time.Now().Before(deadline); {
				time.Sleep(10 * time.Millisecond)
				stats, err = r.usage.disk.ReadStats(ctx)
			}
			if err != nil || stats.Totals.Requests != int64(cycle+1) || stats.Totals.CostMicrosUSD != int64(7*(cycle+1)) || stats.Metrics.ActiveRequests != 0 {
				t.Fatal("checkpoint/restart lost actual accounting/Usage", cycle, stats, err)
			}
		}
		assertData()
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		r, err = OpenWithOptions(ctx, cfg, logger, opts)
		if err != nil {
			t.Fatal("default Runtime compact-root reopen", cycle, err)
		}
		if err := r.selectedState.generation.WaitStartupAudit(ctx); err != nil {
			t.Fatal(err)
		}
		assertData()
	}
}
