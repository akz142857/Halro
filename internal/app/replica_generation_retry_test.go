package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akz142857/Halro/internal/ledger"
	"github.com/akz142857/Halro/internal/replication"
	"github.com/akz142857/Halro/internal/resourcebudget"
	"github.com/akz142857/Halro/internal/usage"
)

// Exercise the public startup stack, including failures after the generation
// builder returns. The old Runtime is closed before the explicitly authorized
// offline cut changes; this does not claim an online role-change protocol.
func TestReplicaGenerationPublicOpenFailureRetriesPreserveNativeFacts(t *testing.T) {
	for _, confirmedSuffix := range []bool{false, true} {
		name := "excluded_durable_suffix"
		if confirmedSuffix {
			name = "confirmed_unapplied_suffix"
		}
		t.Run(name, func(t *testing.T) {
			for _, fault := range []string{"ledger_ceiling", "usage_ceiling", "transport_after_generation"} {
				t.Run(fault, func(t *testing.T) {
					cfg, baseline, tail := replicaStartupPrefixFixture(t)
					ctx := context.Background()
					logger := slog.New(slog.NewTextHandler(io.Discard, nil))
					old, err := Open(ctx, cfg, logger)
					if err != nil {
						t.Fatal(err)
					}
					old.stopBackgroundWork()
					state := old.replication.publisher.Snapshot()
					if err := old.replication.receiver.BindPrimary("halro-0"); err != nil {
						t.Fatal(err)
					}
					if err := old.replication.receiver.Confirm(replication.CommitNotice{ClusterID: state.ClusterID, Incarnation: state.Incarnation, NodeID: "halro-0", Term: state.Term, ConfirmedIndex: 2}); err != nil {
						t.Fatal(err)
					}
					if _, err := old.replication.applier.ApplyConfirmed(ctx); err != nil {
						t.Fatal(err)
					}
					formerLedger, formerUsage, formerGeneration := old.accountingState.projection, old.usage.disk, old.accountingState.replica
					if err := old.Close(); err != nil {
						t.Fatal(err)
					}
					replaceHARuntimeMemberState(t, cfg, func(s *replication.MemberState) {
						s.AppliedIndex, s.Projection.Index = 1, 1
						s.ConfirmedIndex = 1
						if confirmedSuffix {
							s.ConfirmedIndex = 2
						}
					})
					paths := []string{cfg.LedgerPath(), cfg.AuditPath(), cfg.GovernancePath(), filepathMetadataJournal(cfg), cfg.OrderingJournalPath(), cfg.ReplicationStatePath()}
					digests := make(map[string][32]byte)
					for _, path := range paths {
						b, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						digests[path] = sha256.Sum256(b)
					}
					bad := cfg
					options := OpenOptions{}
					if fault == "transport_after_generation" {
						peers := *cfg.Replication
						peers.TLS.CertFile = filepath.Join(t.TempDir(), "missing-client-cert.pem")
						bad.Replication = &peers
					} else {
						b, p := runtimeCandidateBudgetFixture(), runtimePressureFixture()
						b.TotalDiskBytes, b.BackgroundDiskBytes = 1<<30, 512<<20
						b.RecoveryDiskBytes, b.RecoveryReserveDiskBytes = 512<<20, 512<<20
						b.LedgerProjectionDiskBytes, b.UsageProjectionDiskBytes = 1<<20, 1<<20
						b.MaxClaims += 2
						b.ControlRAMBytes += 2 * resourcebudget.ClaimRAMBytes
						b.TotalRAMBytes += 2 * resourcebudget.ClaimRAMBytes
						addProjectionMappingFixtureRAM(t, &b)
						// Private fixture slack for the Replica recovery envelopes the
						// candidate fixture does not itemize (metadata window, inventory,
						// archive readers); the subject is the disk ceiling, not capacity.
						const recoverySlack, slackClaims = 256 << 20, 8
						b.RecoveryRAMBytes += recoverySlack
						b.RecoveryReserveRAMBytes += recoverySlack + slackClaims*resourcebudget.ClaimRAMBytes
						b.RecoveryClaims += slackClaims
						b.MaxClaims += slackClaims
						b.ControlRAMBytes += slackClaims * resourcebudget.ClaimRAMBytes
						b.TotalRAMBytes += recoverySlack + slackClaims*resourcebudget.ClaimRAMBytes
						if fault == "ledger_ceiling" {
							b.LedgerProjectionDiskBytes = 4096
						} else {
							b.UsageProjectionDiskBytes = 4096
						}
						bad.ResourceBudget, bad.ResourcePressure = &b, &p
						// Deterministic admission sample, not a physical pressure or
						// original-capacity acceptance claim.
						options.resourceSampleSource = pressureSampleFunc(func(context.Context) resourcebudget.Sample { return runtimeLowSample() })
					}
					for attempt := 0; attempt < 2; attempt++ {
						observedLedger, observedPair := false, false
						probe := generationProbeContext{Context: ctx, probe: func() {
							_, ledgerErr := os.Stat(replicaRuntimeProjectionPath(cfg))
							_, usageErr := os.Stat(replicaRuntimeProjectionPath(cfg) + ".usage")
							observedLedger = observedLedger || ledgerErr == nil
							observedPair = observedPair || ledgerErr == nil && usageErr == nil
						}}
						candidate, err := OpenWithOptions(probe, bad, logger, options)
						if candidate != nil {
							_ = candidate.Close()
							t.Fatal("failed construction published a Runtime")
						}
						matches := fault == "ledger_ceiling" && errors.Is(err, ledger.ErrProjectionDiskLimit) || fault == "usage_ceiling" && errors.Is(err, usage.ErrDiskUsageDiskLimit) || fault == "transport_after_generation" && err != nil && strings.Contains(err.Error(), "missing-client-cert.pem")
						if !matches {
							t.Fatal("did not reach the intended startup failure", fault, err)
						}
						if fault == "usage_ceiling" && !observedLedger || fault == "transport_after_generation" && !observedPair {
							t.Fatal("failure did not exercise the intended constructed owners", observedLedger, observedPair)
						}
						for _, path := range []string{replicaRuntimeProjectionPath(cfg), replicaRuntimeProjectionPath(cfg) + ".usage"} {
							if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
								t.Fatal("failed startup retained a candidate", path, err)
							}
						}
						for path, before := range digests {
							b, err := os.ReadFile(path)
							if err != nil || sha256.Sum256(b) != before {
								t.Fatal("failed startup altered native/member authority", path, err)
							}
						}
						if err := formerGeneration.ValidateLedgerForPublication(ctx); !errors.Is(err, errReplicaAccountingRetired) {
							t.Fatal("failed retry restored withdrawn adapter", err)
						}
						if _, _, err := formerLedger.ReadPosition(ctx); err == nil {
							t.Fatal("former Ledger remained readable")
						}
						if _, err := formerUsage.ReadStats(ctx); err == nil {
							t.Fatal("former Usage remained readable")
						}
					}
					retry, err := Open(ctx, cfg, logger)
					if err != nil {
						t.Fatal("fresh public retry failed", err)
					}
					t.Cleanup(func() { _ = retry.Close() })
					retry.stopBackgroundWork()
					head, _, err := retry.accountingState.projection.ReadPosition(ctx)
					stats, statsErr := retry.usage.disk.ReadStats(ctx)
					wantHead, wantActive := baseline, uint64(0)
					if confirmedSuffix {
						wantHead, wantActive = tail, 1
					}
					if err != nil || statsErr != nil || head != wantHead || stats.Metrics.ActiveRequests != wantActive || retry.accountingState.replica == formerGeneration {
						t.Fatal("fresh retry lost its role prefix or double counted", head, stats, err, statsErr)
					}
					for _, path := range paths[:5] {
						b, err := os.ReadFile(path)
						if err != nil || sha256.Sum256(b) != digests[path] {
							t.Fatal("successful retry modified authoritative native bytes", path, err)
						}
					}
				})
			}
		})
	}
}
