package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"reflect"
	"testing"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

func TestDeletingUsageHistoryRebuildsIdenticalIndexFromLedger(t *testing.T) {
	cfg := testConfig(t)
	if err := Initialize(cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(
		context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	request, err := runtime.accounting.BeginRequestDetailed(
		context.Background(), "project_1", "key_1", "request_1", "chat",
	)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := runtime.accounting.ReserveAttemptDetailed(
		context.Background(), request, 1_000, 100,
		budget.AttemptMetadata{
			RouteID: "route_1", ProviderID: "provider_1", ProviderModel: "model_1",
			AttemptNumber: 1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.MarkStarted(context.Background(), attempt); err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.Settle(context.Background(), attempt, budget.Settlement{
		CommittedMicrosUSD: 90, ProviderInputTokens: 7, ProviderOutputTokens: 3,
		Outcome: "success", LatencyMillis: 12,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.accounting.Finalize(context.Background(), request, "success"); err != nil {
		t.Fatal(err)
	}
	expected := dumpUsage(t, runtime)
	if runtime.usage.Watermark().Sequence == 0 {
		t.Fatal("the usage history did not advance")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.HistoryPath()); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(
		context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	actual := dumpUsage(t, reopened)
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("rebuilt history differs:\nactual=%#v\nexpected=%#v", actual, expected)
	}
}

func TestRecoveredLeaseAttributionSurvivesUsageHistoryAndParquet(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "not_started", true: "started"}[started], func(t *testing.T) {
			cfg := testConfig(t)
			if err := Initialize(cfg); err != nil {
				t.Fatal(err)
			}
			discard := slog.New(slog.NewTextHandler(io.Discard, nil))
			runtime, err := Open(context.Background(), cfg, discard)
			if err != nil {
				t.Fatal(err)
			}
			request, err := runtime.accounting.BeginRequestDetailed(context.Background(), "project_recovery", "key_recovery", "request_recovery", "chat")
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := runtime.accounting.ReserveAttemptDetailed(context.Background(), request, 1_000, 100, budget.AttemptMetadata{
				RouteID: "route_recovery", DeploymentID: "deployment_recovery", ProviderID: "provider_recovery",
				OfferingID: domain.OfferingBigModelCodingPlan, ProfileID: domain.ProfileBigModelCNCodingChat,
				AccountRegionID: domain.RegionCN, ProviderPrimitive: provider.PrimitiveBigModelChat,
				ProviderModel: "glm-recovery", AttemptNumber: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if started {
				if err := runtime.accounting.MarkStarted(context.Background(), attempt); err != nil {
					t.Fatal(err)
				}
			}
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}

			recovered, err := Open(context.Background(), cfg, discard)
			if err != nil {
				t.Fatal(err)
			}
			assertRecovered := func(snapshot usageDump) {
				t.Helper()
				for _, got := range snapshot.Attempts {
					if got.AttemptID != attempt.AttemptID {
						continue
					}
					if got.OfferingID != attempt.OfferingID || got.ProfileID != attempt.ProfileID || got.AccountRegionID != attempt.AccountRegionID || got.FailurePhase != "accounting" {
						t.Fatalf("recovered usage attribution/phase was not preserved: %#v", got)
					}
					return
				}
				t.Fatalf("recovered attempt %q is absent from Usage", attempt.AttemptID)
			}
			assertRecovered(dumpUsage(t, recovered))
			if _, err := recovered.usageExporter.Export(recovered.usage.History()); err != nil {
				recovered.Close()
				t.Fatal(err)
			}
			if err := recovered.usageExporter.Verify(recovered.usage.History()); err != nil {
				recovered.Close()
				t.Fatal(err)
			}
			if err := recovered.Close(); err != nil {
				t.Fatal(err)
			}

			checkpointed, err := Open(context.Background(), cfg, discard)
			if err != nil {
				t.Fatal(err)
			}
			defer checkpointed.Close()
			assertRecovered(dumpUsage(t, checkpointed))
		})
	}
}
