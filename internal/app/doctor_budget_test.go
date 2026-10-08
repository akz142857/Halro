package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/advisor"
	"github.com/akz142857/Halro/internal/diskset"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

func TestDoctorWholeJobAndActualOutputShareOriginalEnvelope(t *testing.T) {
	cfg := budgetedBackupOperationFixture(t)
	root, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	outputErr := errors.New("actual output failed")
	err = DoctorWithReport(root, cfg, DoctorOptions{}, func(report DoctorReport) error {
		called = true
		snapshot := pressure.logicalBudget().Snapshot()
		if snapshot.Classes[resourcebudget.Recovery].RAM < doctorMaintenanceRAMBytes {
			t.Fatal("output escaped whole-job fee", snapshot)
		}
		// Native readers have completed while the bounded result is owned by output.
		if len(report.Checks) == 0 {
			t.Fatal("output lacks inspection results")
		}
		return outputErr
	})
	if !called || !errors.Is(err, outputErr) {
		t.Fatal(called, err)
	}
	if s := pressure.logicalBudget().Snapshot(); s.Classes[resourcebudget.Recovery].RAM >= doctorMaintenanceRAMBytes {
		t.Fatal("output did not release job envelope", s)
	}
}

func TestDoctorCounterAdmissionAndForeignCleanupRetainOriginalFees(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			cfg := budgetedBackupOperationFixture(t)
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			root, pressure, err := commandBudgetContext(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			err = withDoctorFanOutCounter(root, cfg, func(set *diskset.Set) error {
				snapshot := pressure.logicalBudget().Snapshot()
				if snapshot.Classes[resourcebudget.Recovery].RAM != doctorMaintenanceRAMBytes || snapshot.Classes[resourcebudget.Recovery].Disk != newProviderMaintenanceOwner(cfg, pressure).limit {
					t.Fatal(snapshot)
				}
				if count, err := set.Increment(root, [32]byte{}); err != nil || count != 1 {
					t.Fatal(count, err)
				}
				if foreign {
					dirs, err := os.ReadDir(temp)
					if err != nil || len(dirs) != 1 {
						t.Fatal(dirs, err)
					}
					path := filepath.Join(temp, dirs[0].Name(), "counts")
					if err := os.Rename(path, path+".saved"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			})
			snapshot := pressure.logicalBudget().Snapshot()
			if foreign {
				if err == nil || snapshot.Classes[resourcebudget.Recovery].RAM != doctorMaintenanceRAMBytes || snapshot.Classes[resourcebudget.Recovery].Disk == 0 {
					t.Fatal(err, snapshot)
				}
				dirs, _ := os.ReadDir(temp)
				raw, err := os.ReadFile(filepath.Join(temp, dirs[0].Name(), "counts"))
				if err != nil || string(raw) != "foreign" {
					t.Fatal(string(raw), err)
				}
			} else if err != nil || snapshot.Used != (resourcebudget.Cost{}) {
				t.Fatal(err, snapshot)
			}
		})
	}
	cfg := budgetedBackupOperationFixture(t)
	root, pressure, err := commandBudgetContext(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := pressure.logicalBudget().TryAcquireRecovery(resourcebudget.Cost{RAM: cfg.ResourceBudget.RecoveryReserveRAMBytes - doctorMaintenanceRAMBytes + 1})
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	called := false
	err = withDoctorFanOutCounter(root, cfg, func(*diskset.Set) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("work ran before admission", called, err)
	}
}

func TestDoctorFiniteAdvisorCountsReachableRoutesAndBreaksTies(t *testing.T) {
	cfg, _ := journalledInstance(t)
	store, err := openMetadataForTest(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	routes, err := store.ListRoutes(context.Background())
	if err != nil || len(routes) != 1 {
		t.Fatal(routes, err)
	}
	original := routes[0]
	// Grow beyond the native row page; every alias has the same exact count.
	for i := 0; i < 130; i++ {
		route := original
		route.ID = fmt.Sprintf("fanout-%04d", i)
		route.PublicModel = "zeta"
		if i%2 == 0 {
			route.PublicModel = "alpha"
		}
		route.Revision = 0
		if _, err := store.PutRoute(context.Background(), route, 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	input := doctorAdvisorInput(context.Background(), cfg, store)
	if !input.TopologyKnown || input.WidestFanOut != (advisor.FanOut{PublicModel: "alpha", Candidates: 65}) {
		t.Fatal(input)
	}
	if err := store.WalkRoutesMaintenance(context.Background(), func(route domain.Route) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if input := doctorAdvisorInput(cancelled, cfg, store); input.TopologyKnown {
		t.Fatal("cancelled source credited topology", input)
	}
}

func TestDoctorReportCapacityFailsExplicitlyBeforeOutput(t *testing.T) {
	cfg := testConfig(t)
	cfg.Storage.DataDir = filepath.Join(t.TempDir(), strings.Repeat("x", doctorReportBytes+1))
	called := false
	err := DoctorWithReport(context.Background(), cfg, DoctorOptions{}, func(report DoctorReport) error {
		called = true
		bytes := 0
		for _, check := range report.Checks {
			bytes += len(check.Name) + len(check.Status) + len(check.Detail)
		}
		if report.Healthy || bytes > doctorReportBytes || len(report.Checks) == 0 || report.Checks[len(report.Checks)-1].Name != "report_capacity" {
			t.Fatal("oversized report escaped", bytes, len(report.Checks))
		}
		return nil
	})
	if err == nil || !called {
		t.Fatal(called, err)
	}
}
