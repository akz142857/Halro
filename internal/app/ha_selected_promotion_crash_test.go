package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/akz142857/Halro/internal/audit"
	"github.com/akz142857/Halro/internal/config"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/store/lock"
)

type haPromotionCrashInput struct {
	Config  config.Config
	Options PromoteMemberOptions
}

func controlledHAPromotionContext(t *testing.T, ctx context.Context, cfg config.Config) context.Context {
	t.Helper()
	pressure, err := newResourcePressureWithSource(cfg, pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() }))
	if err != nil {
		t.Fatal(err)
	}
	return context.WithValue(ctx, commandResourceScopeKey{}, &commandResourceScope{pressure: pressure, budget: *cfg.ResourceBudget, policy: *cfg.ResourcePressure})
}

func TestHASelectedPromotionSIGKILLChild(t *testing.T) {
	path := os.Getenv("HALRO_HA_PROMOTION_CRASH_INPUT")
	if path == "" {
		t.Skip("actual SIGKILL child only")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input haPromotionCrashInput
	if err = json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ctx = controlledHAPromotionContext(t, ctx, input.Config)
	guard, err := lock.Acquire(input.Config.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	wanted := os.Getenv("HALRO_HA_PROMOTION_CRASH_PHASE")
	_, err = promoteHASelectedMember(ctx, input.Config, guard, input.Options, func(phase string) error {
		if phase == wanted {
			if e := syscall.Kill(os.Getpid(), syscall.SIGKILL); e != nil {
				return e
			}
			select {}
		}
		return nil
	})
	t.Fatal("actual crash phase not reached", wanted, err)
}

func TestHASelectedPromotionActualSIGKILLResumesExactProposalAndTerm(t *testing.T) {
	for _, phase := range []string{"prepared", "promised", "fulfilled", "promoted", "leadership", "audit-native", "audit-ordered", "audit-capsule", "governance-capsule", "published"} {
		t.Run(phase, func(t *testing.T) {
			stressPhase(t, phase, "prepared", "leadership", "published")
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			fixture, owners, _ := prepareHASelectedTransportFixture(t, ctx)
			before := owners[1].replication.publisher.Snapshot()
			for _, f := range owners {
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				if err := f.owners.Components.Guard.Close(); err != nil {
					t.Fatal(err)
				}
			}
			cfg := fixture.configs[1]
			oldAudit, err := audit.VerifyWithVisitorContext(ctx, cfg.AuditPath(), fixture.keys.Audit, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			options := PromoteMemberOptions{ExpectedTerm: before.Term, ExpectedAppliedIndex: before.AppliedIndex, OldPrimaryNodeID: fixture.configs[0].Replication.NodeID, FencedBy: replication.FenceNodeIsolated, NoPeerPromise: true, Username: "admin", Password: []byte("correct horse battery staple")}
			raw, err := json.Marshal(haPromotionCrashInput{Config: cfg, Options: options})
			if err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(t.TempDir(), "promotion-input.json")
			if err = os.WriteFile(input, raw, 0600); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHASelectedPromotionSIGKILLChild$", "-test.count=1", "-test.timeout=50s")
			command.Env = append(os.Environ(), "HALRO_HA_PROMOTION_CRASH_INPUT="+input, "HALRO_HA_PROMOTION_CRASH_PHASE="+phase)
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal("child did not terminate by SIGKILL", err, string(output))
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("child reached no actual crash", err, string(output))
			}
			resume := controlledHAPromotionContext(t, ctx, cfg)
			result, err := PromoteMember(resume, cfg, options)
			if err != nil {
				t.Fatal("actual authenticated restart", err, string(output))
			}
			if result.State.Term != before.Term+1 || result.State.PromisedTerm != before.Term+1 || result.State.Role != replication.RolePrimary || result.State.DurableIndex != before.DurableIndex+2 || result.State.AppliedIndex != before.AppliedIndex {
				t.Fatal("retry invented another proposal/term or lost prefix", result.State)
			}
			count := 0
			if _, err := audit.VerifyWithVisitorContext(ctx, cfg.AuditPath(), fixture.keys.Audit, 0, func(record audit.Record) error {
				if record.Sequence <= oldAudit.Records {
					return nil
				}
				if record.Event.Action != "cluster.promote" || record.Event.ActorID != options.Username || record.Event.Metadata["promotion_authority"] == nil {
					return errors.New("unexpected promotion Audit payload")
				}
				count++
				return nil
			}); err != nil || count != 1 {
				t.Fatal("promotion Audit was duplicated or changed after SIGKILL", count, err)
			}
		})
	}
}
