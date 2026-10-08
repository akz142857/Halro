package gateway

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/budget"
	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/openaiapi"
	"github.com/akz142857/Halro/internal/requestmeta"
	"github.com/akz142857/Halro/internal/resourcebudget"
)

// The protocol/provider fixture supplies no accounting authority. Replace its
// empty legacy Manager with one built from this actual authenticated native
// source, under its real maintenance lease and the same resource Manager.
func bindActualCompletionAccounting(t *testing.T, f fixture) (*resourcebudget.Manager, *ledger.AuthenticatedLedgerProjection, *ledger.Log, string) {
	t.Helper()
	limits := resourcebudget.Limits{Total: resourcebudget.Cost{RAM: 1 << 30, Disk: 256 << 20}, MaxClaims: 32, MaxOwners: 4, MaxPressureAge: time.Hour}
	for i := range limits.Classes {
		limits.Classes[i] = limits.Total
	}
	limits.Classes[resourcebudget.Recovery] = resourcebudget.Cost{}
	limits.Classes[resourcebudget.Completion] = resourcebudget.Cost{RAM: 256 << 20, Disk: 8 << 20}
	limits.Classes[resourcebudget.Control] = resourcebudget.Cost{RAM: int64(limits.MaxClaims) * resourcebudget.ClaimRAMBytes}
	resources, err := resourcebudget.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := resources.SetPressure(resourcebudget.Healthy); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "actual-native.wal")
	native, err := ledger.OpenWithOptions(path, ledger.NewStatus(), ledger.Options{ChainKey: testChainKey, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := native.BeginMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	report, partial, err := ledger.InspectReplayAuthenticated(path, testChainKey, nil)
	if err != nil || partial {
		t.Fatal(report, partial, err)
	}
	p, err := ledger.BuildAuthenticatedLedgerProjection(context.Background(), path, testChainKey, filepath.Join(t.TempDir(), "actual-root"), report,
		ledger.AuthenticatedLedgerOptions{Identity: sha256.Sum256([]byte("independent-gateway-completion-fixture")), Budget: resources, MaxDiskBytes: 128 << 20, MaxAppendBytes: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	accounting, err := budget.NewWithAuthenticatedProjection(context.Background(), native, p, mustResolver(t, "UTC"), lease, budget.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	f.service.accounting = accounting
	t.Cleanup(func() {
		if err := native.Close(); err != nil {
			t.Error(err)
		}
		if err := p.Discard(); err != nil {
			t.Error(err)
		}
		if got := resources.Snapshot(); got.Used != (resourcebudget.Cost{}) || got.Owners != 0 {
			t.Error("real completion owners leaked", got)
		}
	})
	return resources, p, native, path
}

func TestGatewayActualAuthenticatedCompletionAfterProviderPressureAndCancel(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "success"
		if canceled {
			name = "caller-canceled"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 1000000)
			defer f.close()
			resources, p, native, path := bindActualCompletionAccounting(t, f)
			work, err := resources.TryAcquireCompletableWork(resourcebudget.Active, resourcebudget.Cost{RAM: 64 << 10}, resourcebudget.Cost{RAM: 256 << 20, Disk: 8 << 20}, ledger.CompletionWorkspaceRequirements())
			if err != nil {
				t.Fatal(err)
			}
			defer work.Active.Release()
			defer work.Close()
			ctx, cancel := context.WithCancel(work.Bind(requestmeta.WithRequestID(context.Background(), "req_real_completion")))
			defer cancel()
			started, release := make(chan struct{}, 1), make(chan struct{})
			f.adapter.started, f.adapter.release = started, release
			done := make(chan error, 1)
			go func() {
				maxTokens := int64(10)
				_, err := f.service.Chat(ctx, f.plaintext, openaiapi.ChatCompletionRequest{Model: "chat", MaxCompletionTokens: &maxTokens, Messages: []openaiapi.Message{{Role: "user", Content: openaiapi.TextContent("hello")}}})
				done <- err
			}()
			select {
			case <-started:
			case err := <-done:
				t.Fatal("request did not reach actual provider fixture", err)
			case <-time.After(10 * time.Second):
				t.Fatal("provider did not start")
			}
			before := resources.Snapshot()
			if before.CompletionReserved.RAM != 256<<20 || before.CompletionReserved.Disk != 8<<20 {
				t.Fatal("terminal resources not owned before provider", before)
			}
			if err := resources.SetPressure(resourcebudget.Pressured); err != nil {
				t.Fatal(err)
			}
			resources.CloseAdmission()
			if canceled {
				cancel()
			} else {
				close(release)
			}
			select {
			case err := <-done:
				if !canceled && err != nil {
					t.Fatal("accepted work failed to finish", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("terminal continuation did not retire")
			}
			if native.Status().Load() != ledger.AccountingHealthy {
				t.Fatal("completion latched recovery", native.Status().Load())
			}
			err = resourcebudget.WithCompletionWorkspace(context.WithoutCancel(ctx), func(ctx context.Context) error {
				request, found, err := p.ReadRequest(ctx, "req_real_completion")
				if err != nil || !found || request.Finalized == nil {
					return errors.Join(err, errors.New("actual COW request was not finalized"))
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			var settlements, finals int
			report, partial, err := ledger.InspectReplayAuthenticated(path, testChainKey, func(r ledger.Record) error {
				if r.Event.Kind == ledger.EventAttemptSettled {
					settlements++
				}
				if r.Event.Kind == ledger.EventRequestFinalized {
					finals++
				}
				return nil
			})
			if err != nil || partial || report.Head.Sequence != 5 || settlements != 1 || finals != 1 {
				t.Fatal("actual native lifecycle missing or duplicated", report, settlements, finals, err)
			}
			if after := resources.Snapshot(); after.Used != before.Used || after.NormalClaims != before.NormalClaims {
				t.Fatal("terminal reacquired or retired still-owned credits", before, after)
			}
		})
	}
}
